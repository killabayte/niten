//go:build darwin

package probe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/niten/internal/config"
	"github.com/killabayte/niten/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.MaybeFakeCLI()
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}

func template(t *testing.T) []byte {
	b, err := os.ReadFile(filepath.Join(testutil.Root(), "examples", "claude-settings.template.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// probe runs the whole probe against scripted CLIs in the given mode.
func probe(t *testing.T, execMode, revMode string) (*Certificate, Binding) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	claude, codex, _ := testutil.FakeModels(t, dir, testutil.FakeScript{
		Executor: []testutil.FakeAction{{Probe: execMode}}, Reviewer: []testutil.FakeAction{{Probe: revMode}}})
	cfg := config.Defaults()
	var argvs [][]string
	for _, c := range cfg.Policy.Commands {
		argvs = append(argvs, c.Argv)
	}
	b, err := Bind(context.Background(), BindInput{Claude: claude, Codex: codex, Executor: cfg.Executor, Reviewer: cfg.Reviewer,
		SettingsTemplate: template(t), StripEnv: cfg.StripEnv, Commands: argvs})
	if err != nil {
		t.Fatal(err)
	}
	c, err := Run(context.Background(), Options{Root: filepath.Join(dir, "probe"), Binding: b, SettingsTemplate: template(t), StripEnv: cfg.StripEnv,
		Commands: cfg.Policy.Commands, PerInvocation: 3 * time.Minute, Out: testWriter{t}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Remove(filepath.Join("/private/tmp", "niten-probe-"+tokenOf(c)))
		if d, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
			os.Remove(filepath.Join(d, "niten-probe-"+tokenOf(c)))
		}
	})
	return c, b
}

func tokenOf(c *Certificate) string {
	b, _ := os.ReadFile(filepath.Join(c.World, "store", "CANARY"))
	tok, _, _ := strings.Cut(string(b), " ")
	return tok
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func statuses(c *Certificate) map[string]Status {
	out := map[string]Status{}
	for _, x := range c.Controls {
		out[x.Role+"/"+x.Name] = x.Status
	}
	return out
}

func dump(t *testing.T, c *Certificate) {
	for _, x := range c.Controls {
		t.Logf("%-8s %-20s %-12s %s", x.Role, x.Name, x.Status, strings.Join(x.Evidence, " | "))
	}
}

// A session that follows the steps inside a real sandbox passes every
// control, and its certificate is found again by its fingerprint.
func TestProbeOfASandboxedSessionPasses(t *testing.T) {
	t.Parallel()
	c, b := probe(t, testutil.ProbeHonest, testutil.ProbeHonest)
	dump(t, c)
	if c.Result != Pass || c.Invocations != 2 {
		dump(t, c)
		t.Fatalf("result %s, invocations %d", c.Result, c.Invocations)
	}
	if len(c.Controls) != 17 {
		t.Fatalf("%d controls", len(c.Controls))
	}
	store := t.TempDir()
	if _, err := Save(store, c); err != nil {
		t.Fatal(err)
	}
	if got, _, _, err := Find(store, b.Fingerprint()); err != nil || got.Result != Pass {
		t.Fatalf("find: %+v %v", got, err)
	}
	other := b
	other.Executor = "claude/claude-sonnet-5-5:xhigh"
	if _, _, _, err := Find(store, other.Fingerprint()); err == nil {
		t.Fatal("a certificate was found for another binding")
	}
}

// A session that is not sandboxed fails: the host sees the escapes, whatever
// the session reports.
func TestProbeOfAnUnsandboxedSessionFails(t *testing.T) {
	t.Parallel()
	c, _ := probe(t, testutil.ProbeUnsandboxed, testutil.ProbeUnsandboxed)
	s := statuses(c)
	if c.Result != Fail {
		dump(t, c)
		t.Fatalf("result %s", c.Result)
	}
	for _, k := range []string{"executor/file tools negative", "executor/shell negative", "executor/escape hatch", "executor/network", "reviewer/shell negative", "reviewer/network"} {
		if s[k] != Fail {
			dump(t, c)
			t.Fatalf("%s is %s, want fail", k, s[k])
		}
	}
}

// A session that attempts nothing proves nothing: inconclusive, never pass.
func TestProbeOfALazySessionIsInconclusive(t *testing.T) {
	t.Parallel()
	c, _ := probe(t, testutil.ProbeLazy, testutil.ProbeLazy)
	s := statuses(c)
	if c.Result != Inconclusive {
		dump(t, c)
		t.Fatalf("result %s", c.Result)
	}
	for _, k := range []string{"executor/executor positive", "executor/file tools negative", "executor/shell negative", "executor/escape hatch", "executor/network", "reviewer/reviewer positive", "reviewer/shell negative", "reviewer/network"} {
		if s[k] != Inconclusive {
			dump(t, c)
			t.Fatalf("%s is %s, want inconclusive", k, s[k])
		}
	}
}
