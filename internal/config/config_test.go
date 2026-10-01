package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The example file documents exactly the built-in defaults.
func TestExampleEqualsDefaults(t *testing.T) {
	home := t.TempDir()
	p, _ := filepath.Abs(filepath.Join("..", "..", "examples", "niten.toml"))
	l, err := Load(p, true, env(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Load(filepath.Join(t.TempDir(), "absent.toml"), false, env(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatal(err)
	}
	if d.Path != "" || d.SHA256 != "" || l.SHA256 == "" {
		t.Fatalf("provenance: defaults %q/%q, file %q", d.Path, d.SHA256, l.SHA256)
	}
	want := d.Config
	want.ClaudeSettingsTemplate = filepath.Join(filepath.Dir(p), "claude-settings.template.json")
	if !reflect.DeepEqual(l.Config, want) {
		t.Fatalf("example differs from defaults:\n%+v\n%+v", l.Config, want)
	}
	if l.Config.StoreDir != filepath.Join(home, ".local", "state", "niten") || l.Config.MaxActiveTime.D() != 90*time.Minute {
		t.Fatalf("store %s, time %s", l.Config.StoreDir, l.Config.MaxActiveTime.D())
	}
}

func TestOverlayAndBuiltins(t *testing.T) {
	home := t.TempDir()
	p := write(t, `
shogun_command = "/opt/shogun/bin/shogun"
store_dir = "~/state/niten"
max_invocations = 10
invocation_deadline = "5m"
strip_env = ["MY_TOKEN_OVERRIDE"]
[policy]
protected_paths = ["secrets/**"]
[[policy.commands]]
id = "make-test"
argv = ["make", "test"]
timeout = "3m"
[[checks.required]]
id = "unit"
command_id = "make-test"
cwd = "."
expected_exit_code = 0
`)
	l, err := Load(p, true, env(map[string]string{"HOME": home, "XDG_STATE_HOME": "/ignored-because-store-dir-is-set"}))
	if err != nil {
		t.Fatal(err)
	}
	c := l.Config
	if c.ShogunCommand != "/opt/shogun/bin/shogun" || c.StoreDir != filepath.Join(home, "state", "niten") || c.MaxInvocations != 10 || c.InvocationDeadline.D() != 5*time.Minute {
		t.Fatalf("overlay: %+v", c)
	}
	if c.MaxActiveTime.D() != 90*time.Minute || c.Executor != "claude/claude-opus-5-5:xhigh" {
		t.Fatal("absent keys lost their defaults")
	}
	for _, b := range BuiltinProtected {
		if !slices.Contains(c.Policy.ProtectedPaths, b) {
			t.Fatalf("built-in protected path %s was dropped", b)
		}
	}
	if !slices.Contains(c.Policy.ProtectedPaths, "secrets/**") || !slices.Contains(c.StripEnv, "MY_TOKEN_OVERRIDE") || !slices.Contains(c.StripEnv, "ANTHROPIC_MODEL") {
		t.Fatalf("additions: %v %v", c.Policy.ProtectedPaths, c.StripEnv)
	}
	if len(c.Policy.Commands) != 1 || c.Policy.Commands[0].ID != "make-test" || c.Checks.Required[0].CommandID != "make-test" {
		t.Fatalf("commands %+v", c.Policy.Commands)
	}
	if !slices.Contains(c.Policy.InstructionPaths, "**/CLAUDE.md") {
		t.Fatal("built-in instruction paths missing")
	}
}

func TestRejections(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"unknown key":          {`colour = "blue"`, "unknown key"},
		"unknown nested key":   {"[policy]\nnetwork = \"allow\"", "unknown key"},
		"alias model":          {`executor = "claude/opus"`, "executor"},
		"wrong provider":       {`reviewer = "claude/claude-opus-5-5:xhigh"`, "reviewer"},
		"ahead steps":          {`max_ahead_steps = 1`, "v0.1 executes sequentially"},
		"network":              {"[policy]\ntool_network = \"allow\"", "tool_network"},
		"relative command":     {`claude_command = "bin/claude"`, "absolute path"},
		"shell function":       {`codex_command = "zsh -ic codex"`, "aliases and functions"},
		"relative store":       {`store_dir = "state"`, "store_dir"},
		"deadline over total":  {"max_active_time = \"10m\"\ninvocation_deadline = \"20m\"\nfinal_reserve_time = \"5m\"", "exceeds max_active_time"},
		"bad duration":         {`max_active_time = "ninety minutes"`, "ninety"},
		"missing write root":   {"[policy]\nwrite_roots = [\"executor_source\"]", "lacks"},
		"bad pattern":          {"[policy]\nprotected_paths = [\"../up\"]", "not a valid relative pattern"},
		"check without cmd":    {"[[checks.required]]\nid = \"x\"\ncommand_id = \"nope\"\ncwd = \".\"", "unknown command"},
		"check cwd escapes":    {"[[checks.required]]\nid = \"x\"\ncommand_id = \"go-test\"\ncwd = \"../other\"", "clean relative path"},
		"backend":              {`verifier_backend = "docker"`, "verifier_backend"},
		"strip env name":       {`strip_env = ["NOT-A-NAME"]`, "strip_env"},
		"negative budget":      {`claude_max_budget_usd_per_invocation = -1.0`, "must be positive"},
		"duplicate command id": {"[[policy.commands]]\nid = \"a\"\nargv = [\"x\"]\ntimeout = \"1m\"\n[[policy.commands]]\nid = \"a\"\nargv = [\"y\"]\ntimeout = \"1m\"\n[[checks.required]]\nid = \"c\"\ncommand_id = \"a\"\ncwd = \".\"", "repeated"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, tc.body), true, env(map[string]string{"HOME": t.TempDir()}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent.toml"), true, env(map[string]string{"HOME": t.TempDir()})); err == nil {
		t.Fatal("an explicit missing config was accepted")
	}
}

func TestDefaultLocations(t *testing.T) {
	p, _ := DefaultPath(env(map[string]string{"HOME": "/h"}))
	x, _ := DefaultPath(env(map[string]string{"HOME": "/h", "XDG_CONFIG_HOME": "/x"}))
	s, _ := DefaultStoreDir(env(map[string]string{"HOME": "/h", "XDG_STATE_HOME": "/s"}))
	if p != "/h/.config/niten/config.toml" || x != "/x/niten/config.toml" || s != "/s/niten" {
		t.Fatalf("%s %s %s", p, x, s)
	}
	if _, err := DefaultPath(env(map[string]string{})); err == nil {
		t.Fatal("no HOME accepted")
	}
}
