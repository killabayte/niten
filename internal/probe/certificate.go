package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/provider"
)

// Version of the probe and its controls; a change requires a new certificate.
const Version = 1

// Topology is the role layout P0a certifies: one writable project, an executor
// in the owned clone, a reviewer in a disposable copy, no context copies.
const Topology = "p0a-single-project/1"

// CertificateKind identifies a certificate file.
const CertificateKind = "niten.profile_certificate"

// ManagedPolicyPaths are the managed policy files that would override user
// settings; their content is part of the binding, absent or not.
var ManagedPolicyPaths = []string{
	"/Library/Application Support/ClaudeCode/managed-settings.json",
	"/Library/Application Support/ClaudeCode/managed-mcp.json",
	"/etc/codex/managed_config.toml",
	"/etc/codex/requirements.toml",
}

// Tool identifies one CLI binary.
type Tool struct {
	Path    string `json:"path"`
	Real    string `json:"real"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
}

// Binding is everything a certificate is valid for. A run needs a passing
// certificate for exactly its own binding.
type Binding struct {
	ProbeVersion           int               `json:"probe_version"`
	Topology               string            `json:"topology"`
	Claude                 Tool              `json:"claude"`
	Codex                  Tool              `json:"codex"`
	Executor               string            `json:"executor"`
	Reviewer               string            `json:"reviewer"`
	SettingsTemplateSHA256 string            `json:"settings_template_sha256"`
	ManagedPolicy          map[string]string `json:"managed_policy"`
	StripNames             []string          `json:"strip_names"`
	StripPrefixes          []string          `json:"strip_prefixes"`
	StripEnv               []string          `json:"strip_env"`
	AdapterSHA256          string            `json:"adapter_sha256"`
	OSVersion              string            `json:"os_version"`
	OSBuild                string            `json:"os_build"`
}

// Fingerprint is the digest of the binding.
func (b Binding) Fingerprint() string {
	raw, _ := json.Marshal(b)
	return digest(raw)
}

// BindInput is what a binding is computed from.
type BindInput struct {
	Claude, Codex      string // resolved absolute binaries
	Executor, Reviewer string // provider/model:effort
	SettingsTemplate   []byte
	StripEnv           []string
	Commands           [][]string // allowed check argvs, which become Bash rules
	MaxBudgetUSD       *float64
}

// Bind computes the binding of the current environment. It runs only the
// CLIs' --version, never a model.
func Bind(ctx context.Context, in BindInput) (Binding, error) {
	b := Binding{ProbeVersion: Version, Topology: Topology, Executor: in.Executor, Reviewer: in.Reviewer,
		SettingsTemplateSHA256: digest(in.SettingsTemplate), ManagedPolicy: map[string]string{},
		StripNames: sortedCopy(provider.BuiltinStripNames), StripPrefixes: sortedCopy(provider.BuiltinStripPrefixes), StripEnv: sortedCopy(in.StripEnv)}
	var err error
	if b.Claude, err = identify(ctx, in.Claude); err != nil {
		return b, err
	}
	if b.Codex, err = identify(ctx, in.Codex); err != nil {
		return b, err
	}
	for _, p := range ManagedPolicyPaths {
		data, err := os.ReadFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			b.ManagedPolicy[p] = "absent"
		case err != nil:
			return b, fmt.Errorf("managed policy %s: %w", p, err)
		default:
			b.ManagedPolicy[p] = digest(data)
		}
	}
	em, ee, err := split(in.Executor)
	if err != nil {
		return b, err
	}
	rm, re, err := split(in.Reviewer)
	if err != nil {
		return b, err
	}
	shape := map[string]any{
		"claude": provider.ClaudeArgs(provider.ClaudeRequest{Model: em, Effort: ee, Schema: []byte("SCHEMA"), Settings: "SETTINGS",
			AllowedBash: provider.BashRules(in.Commands), MaxBudgetUSD: in.MaxBudgetUSD}),
		"codex":     provider.CodexArgs(provider.CodexRequest{Model: rm, Effort: re, SchemaPath: "SCHEMA", LastPath: "LAST", Launcher: "LAUNCHER"}),
		"codex_env": provider.CodexRustLog,
	}
	raw, _ := json.Marshal(shape)
	b.AdapterSHA256 = digest(raw)
	b.OSVersion, b.OSBuild = osIdentity()
	return b, nil
}

func sortedCopy(xs []string) []string {
	out := append([]string{}, xs...)
	sort.Strings(out)
	return out
}

// split reads provider/model:effort.
func split(spec string) (model, effort string, err error) {
	_, rest, ok := strings.Cut(spec, "/")
	model, effort, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || model == "" || effort == "" {
		return "", "", fmt.Errorf("model %q is not provider/model:effort", spec)
	}
	return model, effort, nil
}

func identify(ctx context.Context, bin string) (Tool, error) {
	t := Tool{Path: bin}
	if !filepath.IsAbs(bin) {
		return t, fmt.Errorf("binary %q is not an absolute path", bin)
	}
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return t, err
	}
	t.Real = real
	data, err := os.ReadFile(real)
	if err != nil {
		return t, err
	}
	t.SHA256 = digest(data)
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, real, "--version")
	env, _ := provider.FilterEnv(os.Environ(), nil)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return t, fmt.Errorf("%s --version: %w", real, err)
	}
	t.Version = strings.TrimSpace(strings.SplitN(out.String(), "\n", 2)[0])
	return t, nil
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Status of a control or a certificate.
type Status string

const (
	Pass         Status = "pass"
	Fail         Status = "fail"
	Inconclusive Status = "inconclusive" // a required observation is missing; never a pass
)

// Control is one row of the P0a table for one role.
type Control struct {
	Role     string   `json:"role"`
	Name     string   `json:"name"`
	Status   Status   `json:"status"`
	Evidence []string `json:"evidence"`
}

// Certificate is the result of one probe.
type Certificate struct {
	SchemaVersion int       `json:"schema_version"`
	Kind          string    `json:"kind"`
	Fingerprint   string    `json:"fingerprint"`
	Binding       Binding   `json:"binding"`
	Result        Status    `json:"result"`
	Controls      []Control `json:"controls"`
	Invocations   int       `json:"invocations"`
	ActiveMS      int64     `json:"active_ms"`
	World         string    `json:"world"`
	ProbeRun      string    `json:"probe_run"`
	CreatedAt     string    `json:"created_at"`
}

// result is pass only when every control passed.
func result(cs []Control) Status {
	st := Pass
	for _, c := range cs {
		switch c.Status {
		case Fail:
			return Fail
		case Inconclusive:
			st = Inconclusive
		}
	}
	if len(cs) == 0 {
		return Inconclusive
	}
	return st
}

// CertificateDir is <store>/certificates.
func CertificateDir(storeRoot string) string { return filepath.Join(storeRoot, "certificates") }

// Save writes the certificate once, atomically, as <fingerprint>-<run>.json.
func Save(storeRoot string, c *Certificate) (string, error) {
	dir := CertificateDir(storeRoot)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, c.Fingerprint+"-"+c.ProbeRun+".json")
	tmp, err := os.CreateTemp(dir, ".cert-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmp.Name(), p); err != nil {
		return "", err
	}
	return p, nil
}

// ErrNoCertificate means no passing certificate exists for a binding.
var ErrNoCertificate = errors.New("no passing live certificate")

// Find returns the newest passing certificate for the fingerprint and its
// digest. A certificate whose binding does not reproduce its fingerprint, or
// whose file is not private, is ignored.
func Find(storeRoot, fingerprint string) (*Certificate, string, string, error) {
	matches, _ := filepath.Glob(filepath.Join(CertificateDir(storeRoot), fingerprint+"-*.json"))
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	for _, p := range matches {
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c Certificate
		if json.Unmarshal(raw, &c) != nil || c.Kind != CertificateKind || c.Fingerprint != fingerprint || c.Binding.Fingerprint() != fingerprint {
			continue
		}
		if c.Result == Pass {
			return &c, p, digest(raw), nil
		}
	}
	return nil, "", "", fmt.Errorf("%w for fingerprint %s; run niten doctor --live with a separately authorized budget", ErrNoCertificate, fingerprint)
}
