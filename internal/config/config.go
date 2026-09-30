// Package config loads Niten's TOML configuration, fills the built-in defaults and
// validates the v0.1 constraints. Unknown keys are errors. Mandatory built-in lists
// (protected paths, instruction paths, stripped environment names) are always part of
// the effective configuration: a config file can add entries, never remove them.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/killabayte/niten/internal/pathglob"
)

// Duration is a time.Duration written as a Go duration string ("90m").
type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// D returns the duration as time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Command is one allowed check command. Allowing a command does not make it a required check.
type Command struct {
	ID      string   `toml:"id" json:"id"`
	Argv    []string `toml:"argv" json:"argv"`
	Timeout Duration `toml:"timeout" json:"timeout"`
}

// RequiredCheck references an allowed command that must pass; the execution contract
// assigns its criteria.
type RequiredCheck struct {
	ID               string `toml:"id" json:"id"`
	CommandID        string `toml:"command_id" json:"command_id"`
	Cwd              string `toml:"cwd" json:"cwd"`
	ExpectedExitCode int    `toml:"expected_exit_code" json:"expected_exit_code"`
}

// Policy is the hard boundary shared by all roles; plan targets never widen it.
type Policy struct {
	ToolNetwork      string    `toml:"tool_network" json:"tool_network"`
	WriteRoots       []string  `toml:"write_roots" json:"write_roots"`
	ProtectedPaths   []string  `toml:"protected_paths" json:"protected_paths"`
	InstructionPaths []string  `toml:"instruction_paths" json:"instruction_paths"`
	Commands         []Command `toml:"commands" json:"commands"`
}

// Checks holds the required checks.
type Checks struct {
	Required []RequiredCheck `toml:"required" json:"required"`
}

// Config is the effective configuration.
type Config struct {
	Executor               string   `toml:"executor" json:"executor"`
	Reviewer               string   `toml:"reviewer" json:"reviewer"`
	ClaudeCommand          string   `toml:"claude_command" json:"claude_command"`
	CodexCommand           string   `toml:"codex_command" json:"codex_command"`
	ShogunCommand          string   `toml:"shogun_command" json:"shogun_command"`
	StoreDir               string   `toml:"store_dir" json:"store_dir"`
	ClaudeSettingsTemplate string   `toml:"claude_settings_template" json:"claude_settings_template"`
	VerifierBackend        string   `toml:"verifier_backend" json:"verifier_backend"`
	MaxInvocations         int      `toml:"max_invocations" json:"max_invocations"`
	MaxActiveTime          Duration `toml:"max_active_time" json:"max_active_time"`
	InvocationDeadline     Duration `toml:"invocation_deadline" json:"invocation_deadline"`
	MaxRepairsPerStep      int      `toml:"max_repairs_per_step" json:"max_repairs_per_step"`
	MaxAheadSteps          int      `toml:"max_ahead_steps" json:"max_ahead_steps"`
	GatePerStep            bool     `toml:"gate_per_step" json:"gate_per_step"`
	FinalReserveInvocation int      `toml:"final_reserve_invocations" json:"final_reserve_invocations"`
	FinalReserveTime       Duration `toml:"final_reserve_time" json:"final_reserve_time"`
	ClaudeMaxBudgetUSD     *float64 `toml:"claude_max_budget_usd_per_invocation" json:"claude_max_budget_usd_per_invocation"`
	StripEnv               []string `toml:"strip_env" json:"strip_env"`
	Policy                 Policy   `toml:"policy" json:"policy"`
	Checks                 Checks   `toml:"checks" json:"checks"`
}

// Built-in mandatory entries. A config file adds to them and cannot remove them.
var (
	BuiltinProtected   = []string{".git", "**/.git", "**/.git/**", ".claude/**", "**/.claude/**", ".codex/**", "**/.codex/**", ".agents/**", "**/.agents/**", ".mcp.json", "**/.mcp.json"}
	BuiltinInstruction = []string{"AGENTS.md", "**/AGENTS.md", "CLAUDE.md", "**/CLAUDE.md"}
	BuiltinStripEnv    = []string{"CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_EFFORT_LEVEL", "ANTHROPIC_MODEL"}
	// WriteRootNames are the logical write roots of the three v0.1 process profiles.
	WriteRootNames = []string{"executor_source", "executor_scratch", "reviewer_copy", "reviewer_scratch", "verifier_copy", "verifier_scratch"}
)

// Defaults returns the built-in configuration; it equals examples/niten.toml.
func Defaults() Config {
	return Config{
		Executor: "claude/claude-opus-5-5:xhigh", Reviewer: "codex/gpt-6-astra:xhigh",
		ClaudeCommand: "claude", CodexCommand: "codex", ShogunCommand: "shogun",
		VerifierBackend: "macos-seatbelt",
		MaxInvocations:  24, MaxActiveTime: Duration(90 * time.Minute), InvocationDeadline: Duration(20 * time.Minute),
		MaxRepairsPerStep: 2, FinalReserveInvocation: 3, FinalReserveTime: Duration(20 * time.Minute),
		StripEnv: slices.Clone(BuiltinStripEnv),
		Policy: Policy{
			ToolNetwork: "deny", WriteRoots: slices.Clone(WriteRootNames),
			ProtectedPaths: slices.Clone(BuiltinProtected), InstructionPaths: slices.Clone(BuiltinInstruction),
			Commands: []Command{
				{ID: "go-test", Argv: []string{"go", "test", "./..."}, Timeout: Duration(10 * time.Minute)},
				{ID: "go-build", Argv: []string{"go", "build", "./..."}, Timeout: Duration(10 * time.Minute)},
				{ID: "go-vet", Argv: []string{"go", "vet", "./..."}, Timeout: Duration(10 * time.Minute)},
			},
		},
		Checks: Checks{Required: []RequiredCheck{{ID: "go-tests", CommandID: "go-test", Cwd: ".", ExpectedExitCode: 0}}},
	}
}

// Loaded is the effective configuration and where it came from.
type Loaded struct {
	Config Config `json:"config"`
	// Path is the config file that was read, or "" for the built-in defaults.
	Path string `json:"path"`
	// SHA256 is the digest of the config file bytes, or "" for the defaults.
	SHA256 string `json:"sha256"`
}

// DefaultPath is $XDG_CONFIG_HOME/niten/config.toml, else ~/.config/niten/config.toml.
func DefaultPath(getenv func(string) string) (string, error) {
	if x := getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "niten", "config.toml"), nil
	}
	home := getenv("HOME")
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("cannot locate the config directory: HOME is not an absolute path")
	}
	return filepath.Join(home, ".config", "niten", "config.toml"), nil
}

// DefaultStoreDir is $XDG_STATE_HOME/niten, else ~/.local/state/niten.
func DefaultStoreDir(getenv func(string) string) (string, error) {
	if x := getenv("XDG_STATE_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "niten"), nil
	}
	home := getenv("HOME")
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("cannot locate the state directory: HOME is not an absolute path")
	}
	return filepath.Join(home, ".local", "state", "niten"), nil
}

// Load reads the config file at path. With explicit=false a missing file means the built-in
// defaults; with explicit=true it is an error. Relative paths inside the file resolve
// against the file's directory; "~/" expands to HOME.
func Load(path string, explicit bool, getenv func(string) string) (*Loaded, error) {
	cfg := Defaults()
	l := &Loaded{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		l.Path, l.SHA256 = path, digest(data)
		if err := decode(data, &cfg); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist) && !explicit:
	default:
		return nil, fmt.Errorf("config: %w", err)
	}
	base := filepath.Dir(path)
	if cfg.StoreDir == "" {
		if cfg.StoreDir, err = DefaultStoreDir(getenv); err != nil {
			return nil, err
		}
	} else if cfg.StoreDir, err = resolvePath(cfg.StoreDir, "", getenv); err != nil {
		return nil, fmt.Errorf("config: store_dir: %w", err)
	}
	if cfg.ClaudeSettingsTemplate != "" {
		if cfg.ClaudeSettingsTemplate, err = resolvePath(cfg.ClaudeSettingsTemplate, base, getenv); err != nil {
			return nil, fmt.Errorf("config: claude_settings_template: %w", err)
		}
	}
	cfg.Policy.ProtectedPaths = union(BuiltinProtected, cfg.Policy.ProtectedPaths)
	cfg.Policy.InstructionPaths = union(BuiltinInstruction, cfg.Policy.InstructionPaths)
	cfg.StripEnv = union(BuiltinStripEnv, cfg.StripEnv)
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	l.Config = cfg
	return l, nil
}

// decode overlays the file onto the defaults. A key present in the file replaces the
// default value, lists included; absent keys keep their defaults.
func decode(data []byte, cfg *Config) error {
	var probe map[string]any
	if err := toml.Unmarshal(data, &probe); err != nil {
		return err
	}
	var file Config
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return fmt.Errorf("unknown key(s): %s", strict.String())
		}
		return err
	}
	has := func(key string) bool { _, ok := probe[key]; return ok }
	set := func(key string, dst, src any) {
		if has(key) {
			switch d := dst.(type) {
			case *string:
				*d = *src.(*string)
			case *int:
				*d = *src.(*int)
			case *bool:
				*d = *src.(*bool)
			case *Duration:
				*d = *src.(*Duration)
			case *[]string:
				*d = *src.(*[]string)
			}
		}
	}
	set("executor", &cfg.Executor, &file.Executor)
	set("reviewer", &cfg.Reviewer, &file.Reviewer)
	set("claude_command", &cfg.ClaudeCommand, &file.ClaudeCommand)
	set("codex_command", &cfg.CodexCommand, &file.CodexCommand)
	set("shogun_command", &cfg.ShogunCommand, &file.ShogunCommand)
	set("store_dir", &cfg.StoreDir, &file.StoreDir)
	set("claude_settings_template", &cfg.ClaudeSettingsTemplate, &file.ClaudeSettingsTemplate)
	set("verifier_backend", &cfg.VerifierBackend, &file.VerifierBackend)
	set("max_invocations", &cfg.MaxInvocations, &file.MaxInvocations)
	set("max_active_time", &cfg.MaxActiveTime, &file.MaxActiveTime)
	set("invocation_deadline", &cfg.InvocationDeadline, &file.InvocationDeadline)
	set("max_repairs_per_step", &cfg.MaxRepairsPerStep, &file.MaxRepairsPerStep)
	set("max_ahead_steps", &cfg.MaxAheadSteps, &file.MaxAheadSteps)
	set("gate_per_step", &cfg.GatePerStep, &file.GatePerStep)
	set("final_reserve_invocations", &cfg.FinalReserveInvocation, &file.FinalReserveInvocation)
	set("final_reserve_time", &cfg.FinalReserveTime, &file.FinalReserveTime)
	set("strip_env", &cfg.StripEnv, &file.StripEnv)
	if has("claude_max_budget_usd_per_invocation") {
		cfg.ClaudeMaxBudgetUSD = file.ClaudeMaxBudgetUSD
	}
	if pol, ok := probe["policy"].(map[string]any); ok {
		in := func(k string) bool { _, ok := pol[k]; return ok }
		if in("tool_network") {
			cfg.Policy.ToolNetwork = file.Policy.ToolNetwork
		}
		if in("write_roots") {
			cfg.Policy.WriteRoots = file.Policy.WriteRoots
		}
		if in("protected_paths") {
			cfg.Policy.ProtectedPaths = file.Policy.ProtectedPaths
		}
		if in("instruction_paths") {
			cfg.Policy.InstructionPaths = file.Policy.InstructionPaths
		}
		if in("commands") {
			cfg.Policy.Commands = file.Policy.Commands
		}
	}
	if chk, ok := probe["checks"].(map[string]any); ok {
		if _, ok := chk["required"]; ok {
			cfg.Checks.Required = file.Checks.Required
		}
	}
	return nil
}

var (
	reModelSpec = regexp.MustCompile(`^(claude|codex)/([A-Za-z0-9][A-Za-z0-9._-]*):(low|medium|high|xhigh|max)$`)
	reEnvName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reID        = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// Validate enforces the v0.1 constraints.
func (c *Config) Validate() error {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }
	if m := reModelSpec.FindStringSubmatch(c.Executor); m == nil || m[1] != "claude" {
		add("executor %q must be claude/<exact model id>:<effort>", c.Executor)
	}
	if m := reModelSpec.FindStringSubmatch(c.Reviewer); m == nil || m[1] != "codex" {
		add("reviewer %q must be codex/<exact model id>:<effort>", c.Reviewer)
	}
	for name, v := range map[string]string{"claude_command": c.ClaudeCommand, "codex_command": c.CodexCommand, "shogun_command": c.ShogunCommand} {
		switch {
		case v == "":
			add("%s is empty", name)
		case strings.ContainsRune(v, '/') && !filepath.IsAbs(v):
			add("%s %q must be a name looked up in PATH or an absolute path", name, v)
		case !strings.ContainsRune(v, '/') && strings.ContainsAny(v, " \t"):
			add("%s %q is not an executable name; shell aliases and functions are not supported", name, v)
		}
	}
	if !filepath.IsAbs(c.StoreDir) {
		add("store_dir %q must be absolute", c.StoreDir)
	}
	if c.VerifierBackend != "macos-seatbelt" {
		add("verifier_backend %q is not supported; v0.1 has only macos-seatbelt", c.VerifierBackend)
	}
	if c.MaxInvocations < 1 {
		add("max_invocations must be at least 1")
	}
	if c.MaxActiveTime <= 0 || c.InvocationDeadline <= 0 || c.FinalReserveTime < 0 {
		add("max_active_time and invocation_deadline must be positive and final_reserve_time not negative")
	}
	if c.InvocationDeadline > c.MaxActiveTime {
		add("invocation_deadline %s exceeds max_active_time %s", c.InvocationDeadline.D(), c.MaxActiveTime.D())
	}
	if c.MaxRepairsPerStep < 0 {
		add("max_repairs_per_step must not be negative")
	}
	if c.MaxAheadSteps != 0 {
		add("max_ahead_steps %d is unsupported: v0.1 executes sequentially (P4 is a later decision)", c.MaxAheadSteps)
	}
	if c.FinalReserveInvocation < 0 || c.FinalReserveInvocation > c.MaxInvocations {
		add("final_reserve_invocations must be between 0 and max_invocations")
	}
	if c.FinalReserveTime > c.MaxActiveTime {
		add("final_reserve_time exceeds max_active_time")
	}
	if c.ClaudeMaxBudgetUSD != nil && *c.ClaudeMaxBudgetUSD <= 0 {
		add("claude_max_budget_usd_per_invocation must be positive when set")
	}
	for _, e := range c.StripEnv {
		if !reEnvName.MatchString(e) {
			add("strip_env entry %q is not an environment variable name", e)
		}
	}
	if c.Policy.ToolNetwork != "deny" {
		add("policy.tool_network %q is unsupported; v0.1 closes the model tools' network (deny)", c.Policy.ToolNetwork)
	}
	seen := map[string]bool{}
	for _, r := range c.Policy.WriteRoots {
		if !slices.Contains(WriteRootNames, r) || seen[r] {
			add("policy.write_roots entry %q is unknown or repeated", r)
		}
		seen[r] = true
	}
	for _, r := range WriteRootNames {
		if !seen[r] {
			add("policy.write_roots lacks %q, which a v0.1 process profile needs", r)
		}
	}
	for _, list := range [][]string{c.Policy.ProtectedPaths, c.Policy.InstructionPaths} {
		for _, pat := range list {
			if !pathglob.Valid(pat) {
				add("path pattern %q is not a valid relative pattern", pat)
			}
		}
	}
	cmds := map[string]bool{}
	for _, cmd := range c.Policy.Commands {
		switch {
		case !reID.MatchString(cmd.ID) || cmds[cmd.ID]:
			add("policy command id %q is invalid or repeated", cmd.ID)
		case len(cmd.Argv) == 0 || cmd.Argv[0] == "":
			add("policy command %s has an empty argv", cmd.ID)
		case cmd.Timeout <= 0:
			add("policy command %s needs a positive timeout", cmd.ID)
		}
		cmds[cmd.ID] = true
	}
	checks := map[string]bool{}
	for _, rc := range c.Checks.Required {
		switch {
		case !reID.MatchString(rc.ID) || checks[rc.ID]:
			add("required check id %q is invalid or repeated", rc.ID)
		case !cmds[rc.CommandID]:
			add("required check %s references unknown command %q", rc.ID, rc.CommandID)
		case rc.Cwd == "" || filepath.IsAbs(rc.Cwd) || filepath.Clean(rc.Cwd) != rc.Cwd || strings.HasPrefix(rc.Cwd, ".."):
			add("required check %s cwd %q must be a clean relative path inside the repository", rc.ID, rc.Cwd)
		case rc.ExpectedExitCode < 0 || rc.ExpectedExitCode > 255:
			add("required check %s expected_exit_code is out of range", rc.ID)
		}
		checks[rc.ID] = true
	}
	if len(p) > 0 {
		return errors.New(strings.Join(p, "; "))
	}
	return nil
}

func resolvePath(p, base string, getenv func(string) string) (string, error) {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home := getenv("HOME")
		if home == "" || !filepath.IsAbs(home) {
			return "", errors.New("~ cannot be expanded: HOME is not an absolute path")
		}
		return filepath.Join(home, rest), nil
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	if base == "" {
		return "", fmt.Errorf("%q must be absolute or start with ~/", p)
	}
	return filepath.Join(base, p), nil
}

func union(builtin, extra []string) []string {
	out := slices.Clone(builtin)
	for _, e := range extra {
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
