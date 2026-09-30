package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/testutil"
)

// cliWorld writes a config file for a rebuilt fixture repository and a scripted shogun.
func cliWorld(t *testing.T, fixture string) (plan, repo, cfg, store string) {
	t.Helper()
	testutil.IsolateGit(t)
	root := t.TempDir()
	repo = testutil.FixtureRepo(t, root)
	plan = testutil.CopyFixture(t, fixture, filepath.Join(root, "library"))
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	shogun := testutil.FakeShogun(t, bin, testutil.ShogunValid, filepath.Join(root, "shogun.log"))
	store = filepath.Join(root, "state", "niten")
	cfg = filepath.Join(root, "niten.toml")
	os.WriteFile(cfg, []byte("shogun_command = \""+shogun+"\"\nstore_dir = \""+store+"\"\n"), 0o600)
	old := getenv
	home := filepath.Join(root, "home")
	getenv = func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	t.Cleanup(func() { getenv = old })
	return plan, repo, cfg, store
}

func TestPrepareCLI(t *testing.T) {
	planPath, repo, cfg, store := cliWorld(t, "fast-stats")
	// Flags may follow the plan path.
	code, out, errb := exec("prepare", planPath, "--repo", "repo-1="+repo, "--config", cfg)
	if code != contract.ExitOK || !strings.HasPrefix(out, "prepared run ") || !strings.Contains(out, "S-001 → S-002") || errb != "" {
		t.Fatalf("prepare: code %d\n%s\n%s", code, out, errb)
	}
	code, out, _ = exec("prepare", "--json", "--config", cfg, "--repo", "repo-1="+repo, "--gate-per-step", planPath)
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != contract.ExitOK || res["status"] != "prepared" {
		t.Fatalf("json: code %d %v\n%s", code, err, out)
	}
	runDir, _ := res["run_dir"].(string)
	if canon, _ := filepath.EvalSymlinks(store); !strings.HasPrefix(runDir, canon+string(filepath.Separator)) {
		t.Fatalf("run dir %s outside the store %s", runDir, store)
	}
	var c struct {
		GatePerStep bool `json:"gate_per_step"`
	}
	b, _ := os.ReadFile(filepath.Join(runDir, "contract.json"))
	if json.Unmarshal(b, &c); !c.GatePerStep {
		t.Fatal("--gate-per-step was not recorded")
	}
}

func TestPrepareCLIRefusals(t *testing.T) {
	planPath, repo, cfg, _ := cliWorld(t, "fast-measure")
	code, out, errb := exec("prepare", planPath, "--repo", "repo-1="+repo, "--config", cfg)
	if code != contract.ExitNeedsInput || out != "" || !strings.Contains(errb, "needs input (needs_input)") || !strings.Contains(errb, "--human S-001/V-001") || !strings.Contains(errb, "no run was created") {
		t.Fatalf("measure: code %d\n%s\n%s", code, out, errb)
	}
	code, out, _ = exec("prepare", "--json", planPath, "--repo", "repo-1="+repo, "--config", cfg)
	var res map[string]any
	json.Unmarshal([]byte(out), &res)
	if code != contract.ExitNeedsInput || res["status"] != "needs_input" || res["reason"] != "needs_input" {
		t.Fatalf("json refusal: %d %s", code, out)
	}
	if code, _, _ := exec("prepare", planPath, "--repo", "repo-1="+repo, "--config", cfg, "--human", "S-001/V-001"); code != contract.ExitOK {
		t.Fatalf("with a human owner: %d", code)
	}
	for name, args := range map[string][]string{
		"no plan":         {"prepare"},
		"two plans":       {"prepare", planPath, planPath, "--config", cfg},
		"bad pair":        {"prepare", planPath, "--repo", "repo-1"},
		"duplicate pair":  {"prepare", planPath, "--repo", "repo-1=a", "--repo", "repo-1=b"},
		"missing config":  {"prepare", planPath, "--config", filepath.Join(t.TempDir(), "absent.toml")},
		"unknown flag":    {"prepare", planPath, "--force"},
		"not a plan file": {"prepare", cfg, "--config", cfg},
	} {
		if code, _, _ := exec(args...); code != contract.ExitFormat {
			t.Errorf("%s: code %d, want 2", name, code)
		}
	}
}

func TestParseInterspersed(t *testing.T) {
	fsArgs := [][]string{{"a", "--x", "1", "b"}, {"--x", "1", "a", "b"}, {"a", "--", "--x", "b"}}
	want := []string{"a|b", "a|b", "a|--x|b"}
	for i, args := range fsArgs {
		fs := newTestFlagSet()
		pos, err := parseInterspersed(fs, args)
		if err != nil || strings.Join(pos, "|") != want[i] {
			t.Errorf("%v: %q %v, want %q", args, pos, err, want[i])
		}
	}
}

func newTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("x", "", "")
	return fs
}
