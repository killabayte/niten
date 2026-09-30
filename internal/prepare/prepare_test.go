package prepare

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/niten/internal/config"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/plan"
	"github.com/killabayte/niten/internal/testutil"
	"github.com/killabayte/niten/internal/workspace"
)

// env is one prepare test world: a rebuilt fixture repository, a plan library outside it,
// a private store, a scripted shogun and marker executables standing in for the models.
type env struct {
	t        *testing.T
	root     string
	repo     string
	lib      string
	store    string
	shogun   string
	shogLog  string
	markers  string
	cfg      *config.Loaded
	planPath string
}

func newEnv(t *testing.T, fixture, mode string) *env {
	t.Helper()
	testutil.IsolateGit(t)
	root := t.TempDir()
	e := &env{t: t, root: root, lib: filepath.Join(root, "library"), store: filepath.Join(root, "state", "niten"),
		shogLog: filepath.Join(root, "shogun.log"), markers: filepath.Join(root, "markers")}
	e.repo = testutil.FixtureRepo(t, root)
	os.MkdirAll(e.markers, 0o755)
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	e.shogun = os.Getenv("NITEN_TEST_SHOGUN")
	if e.shogun == "" || mode != testutil.ShogunValid {
		e.shogun = testutil.FakeShogun(t, bin, mode, e.shogLog)
	}
	cfg := config.Defaults()
	cfg.ShogunCommand = e.shogun
	cfg.StoreDir = e.store
	cfg.ClaudeCommand = testutil.Marker(t, bin, "claude", filepath.Join(e.markers, "claude"))
	cfg.CodexCommand = testutil.Marker(t, bin, "codex", filepath.Join(e.markers, "codex"))
	e.cfg = &config.Loaded{Config: cfg}
	if fixture != "" {
		e.planPath = testutil.CopyFixture(t, fixture, e.lib)
	}
	return e
}

func (e *env) opts() Options {
	return Options{PlanPath: e.planPath, Repos: map[string]string{"repo-1": e.repo}, Config: e.cfg, Version: "test",
		Now: func() time.Time { return time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC) }}
}

func (e *env) prepare(o Options) (*Result, *Failure) {
	e.t.Helper()
	res, err := Prepare(context.Background(), o)
	if err == nil {
		return res, nil
	}
	var f *Failure
	if !errors.As(err, &f) {
		e.t.Fatalf("non-failure error: %v", err)
	}
	return nil, f
}

func (e *env) ok(o Options) *Result {
	e.t.Helper()
	res, f := e.prepare(o)
	if f != nil {
		e.t.Fatalf("prepare refused: %v", f)
	}
	return res
}

// refused asserts a failure with the exit code, reason and a detail substring, and that
// no run (not even a staging directory) is left in the store.
func (e *env) refused(o Options, exit contract.ExitCode, reason, detail string) *Failure {
	e.t.Helper()
	res, f := e.prepare(o)
	if f == nil {
		e.t.Fatalf("prepare succeeded (run %s), want %s", res.RunID, reason)
	}
	if f.Exit != exit || f.Reason != reason || !strings.Contains(strings.Join(f.Details, "\n"), detail) {
		e.t.Fatalf("got exit %d %s %q, want exit %d %s containing %q", f.Exit, f.Reason, f.Details, exit, reason, detail)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.store, "runs")); len(entries) != 0 {
		e.t.Fatalf("a refused prepare left %v in the store", entries)
	}
	return f
}

func (e *env) edit(file, old, new string) {
	e.t.Helper()
	p := filepath.Join(e.lib, file)
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), old) {
		e.t.Fatalf("%s does not contain %q", file, old)
	}
	os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644)
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			out[p] = plan.Digest(b) + fi.ModTime().String()
		}
		return nil
	})
	return out
}

func TestPrepareRealPlan(t *testing.T) {
	e := newEnv(t, "fast-stats", testutil.ShogunValid)
	libBefore, repoBefore := snapshot(t, e.lib), snapshot(t, e.repo)
	res := e.ok(e.opts())
	c := res.Contract

	if c.Kind != plan.ContractKind || c.Plan.Revision != 1 || c.Plan.ManifestSource != "sidecar" || c.Plan.GrammarVersion != plan.GrammarVersion {
		t.Fatalf("identity %+v", c.Plan)
	}
	if strings.Join(c.Order, ",") != "S-001,S-002" || c.Repos[0].BaseCommit != testutil.FixtureHead || c.Repos[0].BoundBy != "flag" || c.Repos[0].Role != "write" {
		t.Fatalf("order %v, repo %+v", c.Order, c.Repos[0])
	}
	for _, ch := range c.Checks {
		if ch.CheckSpec != nil || ch.Owner != "niten" {
			t.Fatalf("check %s: spec %v owner %s", ch.ScopedID, ch.CheckSpec, ch.Owner)
		}
	}
	for _, cr := range c.Criteria {
		if cr.Owner != "niten" {
			t.Fatalf("criterion %s owner %s: no criterion becomes human without the user", cr.ID, cr.Owner)
		}
	}
	if c.Limits.MaxInvocations != 24 || c.Limits.MaxAheadSteps != 0 || c.Policy.ToolNetwork != "deny" || c.GatePerStep {
		t.Fatalf("limits %+v policy %+v", c.Limits, c.Policy)
	}
	if c.PlanDigest != plan.PlanDigest(c.Plan.BodySHA256, c.Plan.ReceiptSHA256, c.Plan.ManifestSHA256) {
		t.Fatal("plan digest")
	}
	if c.ShogunVerify.Result != "valid" || c.ShogunVerify.ExitCode != 0 || c.ShogunVerify.BinarySHA256 == "" {
		t.Fatalf("verify record %+v", c.ShogunVerify)
	}

	// The stored run: private, complete, and the contract digest in state.json matches.
	for _, f := range []string{"contract.json", "config.json", "state.json", "inputs/plan.md", "inputs/plan.approval.json", "inputs/plan.manifest.json"} {
		fi, err := os.Stat(filepath.Join(res.RunDir, f))
		if err != nil || fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s: %v %v", f, err, fi.Mode())
		}
	}
	var state map[string]any
	sb, _ := os.ReadFile(filepath.Join(res.RunDir, "state.json"))
	cb, _ := os.ReadFile(filepath.Join(res.RunDir, "contract.json"))
	json.Unmarshal(sb, &state)
	if state["state"] != "prepared" || state["contract_sha256"] != plan.Digest(cb) || state["run_id"] != res.RunID {
		t.Fatalf("state %v", state)
	}
	var back plan.Contract
	if err := json.Unmarshal(cb, &back); err != nil || back.SemanticsDigest != c.SemanticsDigest || back.Checks[0].CheckSpec != nil {
		t.Fatalf("contract does not round-trip: %v", err)
	}
	if !strings.Contains(string(cb), `"check_spec": null`) {
		t.Fatal("check_spec is not recorded as null")
	}

	// shogun verified the private copy, not the source file; nothing else was started.
	if e.shogun != os.Getenv("NITEN_TEST_SHOGUN") {
		log, _ := os.ReadFile(e.shogLog)
		if !strings.Contains(string(log), "verify --require-manifest "+res.RunDir[:len(res.RunDir)-len(res.RunID)]) || strings.Contains(string(log), e.lib) {
			t.Fatalf("shogun log:\n%s", log)
		}
	}
	if m, _ := os.ReadDir(e.markers); len(m) != 0 {
		t.Fatalf("a model command was started: %v", m)
	}
	// The source plan and the repository are untouched.
	if after := snapshot(t, e.lib); len(after) != len(libBefore) {
		t.Fatal("files appeared in the plan library")
	} else {
		for k, v := range libBefore {
			if after[k] != v {
				t.Fatalf("%s changed", k)
			}
		}
	}
	for k, v := range repoBefore {
		if snapshot(t, e.repo)[k] != v {
			t.Fatalf("%s changed", k)
		}
	}
	if s, err := workspace.Inspect(context.Background(), e.repo, nil); err != nil || s.Dirty() {
		t.Fatalf("repository after prepare: %+v %v", s, err)
	}
}

// Awkward content, a required input, an instruction target, an unscoped verification and
// a command-looking expectation that must stay text.
func TestPrepareTrickyPlanNeedsItsInput(t *testing.T) {
	pwned := "/tmp/niten-pwned"
	_, statErr := os.Stat(pwned)
	e := newEnv(t, "fast-tricky", testutil.ShogunValid)
	e.refused(e.opts(), contract.ExitNeedsInput, ReasonNeedsInput, "required_input_missing: in-1")

	wrong := filepath.Join(e.root, "spec-wrong.md")
	os.WriteFile(wrong, []byte("# Spec\n\nsomething else\n"), 0o644)
	o := e.opts()
	o.Inputs = map[string]string{"in-1": wrong}
	e.refused(o, contract.ExitNeedsInput, ReasonNeedsInput, "required_input_mismatch: in-1")

	right := filepath.Join(e.root, "spec.md")
	os.WriteFile(right, []byte("# Spec\n\nThe output must list a|b.\n"), 0o644)
	o.Inputs = map[string]string{"in-1": right}
	res := e.ok(o)
	c := res.Contract
	if len(c.Inputs.Execution) != 1 || !c.Inputs.Execution[0].Required || c.Inputs.Execution[0].ReferencedBy[0] != "S-001/V-002" {
		t.Fatalf("execution inputs %+v", c.Inputs.Execution)
	}
	if b, err := os.ReadFile(filepath.Join(res.RunDir, "inputs", "execution", "in-1")); err != nil || !strings.Contains(string(b), "a|b") {
		t.Fatalf("stored input: %v", err)
	}
	var sawNote bool
	for _, n := range res.Notes {
		sawNote = sawNote || strings.Contains(n, "instruction path AGENTS.md")
	}
	var instr string
	for _, tp := range c.Targets {
		if tp.Path == "AGENTS.md" {
			instr = tp.Instruction
		}
	}
	if !sawNote || instr != "AGENTS.md" {
		t.Fatalf("instruction target not flagged: %v %q", res.Notes, instr)
	}
	for _, id := range []string{"S-001/V-001", "S-002/V-001", "S-003/V-001", "S-002/V-002"} {
		found := false
		for _, ch := range c.Checks {
			found = found || ch.ScopedID == id
		}
		if !found {
			t.Fatalf("check %s missing", id)
		}
	}
	if _, err := os.Stat(pwned); statErr != nil && err == nil {
		t.Fatal("text from the plan was executed")
	}
	// Policy and targets stay separate: no target became a write root or policy entry.
	for _, root := range c.Policy.WriteRoots {
		if strings.Contains(root, "a.go") || strings.Contains(root, "AGENTS") {
			t.Fatalf("a target leaked into the policy: %v", c.Policy.WriteRoots)
		}
	}
}

func TestPrepareSelfContainedPlanNeedsNoArchive(t *testing.T) {
	e := newEnv(t, "fast-reference", testutil.ShogunValid)
	c := e.ok(e.opts()).Contract
	if len(c.Inputs.Execution) != 0 || len(c.Inputs.Planning) != 1 || c.Inputs.Planning[0].ID != "in-1" || c.Inputs.Planning[0].Role != "reference" {
		t.Fatalf("inputs %+v", c.Inputs)
	}
}

func TestPrepareFastAndThoroughSameContract(t *testing.T) {
	a := newEnv(t, "fast-min", testutil.ShogunValid)
	ca := a.ok(a.opts()).Contract
	b := newEnv(t, "thorough-min", testutil.ShogunValid)
	cb := b.ok(b.opts()).Contract
	if ca.SemanticsDigest != cb.SemanticsDigest || ca.PlanDigest == cb.PlanDigest {
		t.Fatalf("semantics %s/%s plan %s/%s", ca.SemanticsDigest, cb.SemanticsDigest, ca.PlanDigest, cb.PlanDigest)
	}
	ja, _ := json.Marshal([]any{ca.Criteria, ca.Checks, ca.Targets, ca.Order})
	jb, _ := json.Marshal([]any{cb.Criteria, cb.Checks, cb.Targets, cb.Order})
	if string(ja) != string(jb) {
		t.Fatalf("execution plans differ:\n%s\n%s", ja, jb)
	}
}

// Mutable frontmatter keys and the execution log are outside the approval.
func TestPrepareIgnoresTheExecutionLog(t *testing.T) {
	e := newEnv(t, "fast-min", testutil.ShogunValid)
	before := e.ok(e.opts()).Contract
	e2 := newEnv(t, "fast-min", testutil.ShogunValid)
	e2.edit("plan.md", "status: planned", "status: in_progress")
	e2.edit("plan.md", "| S-001 | todo | — | — |", "| S-001 | in_progress | 2026-09-30 / Niten | run pending |")
	after := e2.ok(e2.opts()).Contract
	if after.SemanticsDigest != before.SemanticsDigest || after.Plan.BodySHA256 != before.Plan.BodySHA256 || after.Inputs.Plan.SHA256 == before.Inputs.Plan.SHA256 {
		t.Fatal("the execution log changed the plan identity, or the file digest did not record the edit")
	}
}

func TestPrepareIntegrity(t *testing.T) {
	t.Run("changed body caught independently", func(t *testing.T) {
		e := newEnv(t, "fast-min", testutil.ShogunValid)
		e.edit("plan.md", "1. edit a.go", "1. edit a.go and delete everything")
		if e.shogun == os.Getenv("NITEN_TEST_SHOGUN") {
			e.refused(e.opts(), contract.ExitRejected, ReasonPlanChanged, "body changed")
		} else {
			e.refused(e.opts(), contract.ExitRejected, ReasonPlanChanged, "approved body digest")
		}
	})
	t.Run("shogun says changed", func(t *testing.T) {
		e := newEnv(t, "fast-min", testutil.ShogunChanged)
		e.refused(e.opts(), contract.ExitRejected, ReasonPlanChanged, "body changed")
	})
	t.Run("pre-S0 shogun", func(t *testing.T) {
		e := newEnv(t, "fast-min", testutil.ShogunOld)
		e.refused(e.opts(), contract.ExitFormat, ReasonShogunVerify, "S0 manifest sidecar is required")
	})
	t.Run("shogun missing", func(t *testing.T) {
		e := newEnv(t, "fast-min", testutil.ShogunValid)
		e.cfg.Config.ShogunCommand = "shogun-that-does-not-exist"
		e.refused(e.opts(), contract.ExitFormat, ReasonShogunVerify, "not found in PATH")
	})
	t.Run("corrupt receipt", func(t *testing.T) {
		e := newEnv(t, "fast-min", testutil.ShogunValid)
		os.WriteFile(filepath.Join(e.lib, "plan.approval.json"), []byte(`{"schema_version": 1`), 0o644)
		if e.shogun == os.Getenv("NITEN_TEST_SHOGUN") {
			e.refused(e.opts(), contract.ExitFormat, ReasonShogunVerify, "unverifiable")
		} else {
			e.refused(e.opts(), contract.ExitFormat, ReasonInvalidFormat, "receipt")
		}
	})
	t.Run("manifest missing", func(t *testing.T) {
		e := newEnv(t, "fast-min", testutil.ShogunValid)
		os.Remove(filepath.Join(e.lib, "plan.manifest.json"))
		e.refused(e.opts(), contract.ExitFormat, ReasonManifestMissing, "--shogun-run")
	})
	t.Run("manifest bound to another base", func(t *testing.T) {
		e := newEnv(t, "fast-min", testutil.ShogunValid)
		p := filepath.Join(e.lib, "plan.manifest.json")
		b, _ := os.ReadFile(p)
		m, _ := plan.DecodeManifest(b)
		m.Repos[0].Head = strings.Repeat("c", 40)
		m.Repos[0].Fingerprint = m.Repos[0].ComputeFingerprint()
		m.Fingerprint = m.ComputeFingerprint()
		b, _ = json.MarshalIndent(m, "", " ")
		os.WriteFile(p, b, 0o644)
		want := "does not match the receipt"
		if e.shogun == os.Getenv("NITEN_TEST_SHOGUN") {
			want = "does not match the receipt's manifest digest"
			e.refused(e.opts(), contract.ExitRejected, ReasonPlanChanged, want)
			return
		}
		e.refused(e.opts(), contract.ExitRejected, ReasonManifestMismatch, want)
	})
	t.Run("symlinked plan", func(t *testing.T) {
		e := newEnv(t, "fast-min", testutil.ShogunValid)
		link := filepath.Join(e.lib, "link.md")
		os.Symlink(e.planPath, link)
		for _, s := range []string{"approval.json", "manifest.json"} {
			b, _ := os.ReadFile(filepath.Join(e.lib, "plan."+s))
			os.WriteFile(filepath.Join(e.lib, "link."+s), b, 0o644)
		}
		o := e.opts()
		o.PlanPath = link
		e.refused(o, contract.ExitFormat, ReasonInputFile, "symlink")
	})
}

// The legacy run directory can stand in for a missing sidecar, but never silently wins
// over a sidecar that differs from it.
func TestPrepareLegacyRunManifest(t *testing.T) {
	e := newEnv(t, "fast-min", testutil.ShogunValid)
	run := filepath.Join(e.root, "shogun-run")
	os.MkdirAll(run, 0o700)
	side := filepath.Join(e.lib, "plan.manifest.json")
	b, _ := os.ReadFile(side)
	os.WriteFile(filepath.Join(run, "manifest.json"), b, 0o600)
	o := e.opts()
	o.ShogunRun = run
	if c := e.ok(o).Contract; c.Plan.ManifestSource != "sidecar" {
		t.Fatalf("identical sidecar and run manifest: source %s", c.Plan.ManifestSource)
	}
	os.WriteFile(filepath.Join(run, "manifest.json"), []byte(strings.Replace(string(b), `"version": 1`, `"version":  1`, 1)), 0o600)
	os.RemoveAll(filepath.Join(e.store, "runs"))
	e.refused(o, contract.ExitRejected, ReasonManifestConflict, "differ")
	os.WriteFile(filepath.Join(run, "manifest.json"), b, 0o600)
	os.Remove(side)
	if c := e.ok(o).Contract; c.Plan.ManifestSource != "shogun_run" || c.ShogunVerify.Result != "valid" {
		t.Fatalf("legacy manifest: %+v", c.Plan)
	}
}

func TestPrepareDrift(t *testing.T) {
	for name, change := range map[string]func(e *env){
		"new commit": func(e *env) {
			os.WriteFile(filepath.Join(e.repo, "b.go"), []byte("package a\n"), 0o644)
			testutil.Git(t, e.repo, "add", "-A")
			testutil.Git(t, e.repo, "commit", "-qm", "later")
		},
		"untracked file": func(e *env) { os.WriteFile(filepath.Join(e.repo, "notes.txt"), []byte("x"), 0o644) },
		"tracked edit":   func(e *env) { os.WriteFile(filepath.Join(e.repo, "a.go"), []byte("package b\n"), 0o644) },
		"staged addition": func(e *env) {
			os.WriteFile(filepath.Join(e.repo, "c.go"), []byte("x"), 0o644)
			testutil.Git(t, e.repo, "add", "c.go")
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, "fast-min", testutil.ShogunValid)
			change(e)
			e.refused(e.opts(), contract.ExitRejected, ReasonDrift, "no longer matches the approved planning base")
		})
	}
	// The plan's own triplet inside the repository and Shogun's run directories are not drift.
	e := newEnv(t, "fast-min", testutil.ShogunValid)
	e.lib = filepath.Join(e.repo, "docs", "plans")
	e.planPath = testutil.CopyFixture(t, "fast-min", e.lib)
	os.MkdirAll(filepath.Join(e.repo, ".shogun", "runs"), 0o755)
	os.WriteFile(filepath.Join(e.repo, ".shogun", "runs", "x.json"), []byte("{}"), 0o644)
	e.ok(e.opts())
	// But the manifest's own exclude list is not honored for other paths.
	os.RemoveAll(filepath.Join(e.store, "runs"))
	os.WriteFile(filepath.Join(e.repo, "docs", "plans", "other.md"), []byte("x"), 0o644)
	e.refused(e.opts(), contract.ExitRejected, ReasonDrift, "docs/plans/other.md")
}

func TestPrepareScope(t *testing.T) {
	for fixture, want := range map[string]string{
		"fast-dirty":     "planned with local changes",
		"fast-multirepo": "exactly one writable repository",
		"fast-nongit":    "non-git directory",
	} {
		t.Run(fixture, func(t *testing.T) {
			e := newEnv(t, fixture, testutil.ShogunValid)
			e.refused(e.opts(), contract.ExitFormat, ReasonUnsupported, want)
		})
	}
	t.Run("unknown repo id", func(t *testing.T) {
		e := newEnv(t, "fast-min", testutil.ShogunValid)
		o := e.opts()
		o.Repos = map[string]string{"repo-9": e.repo}
		e.refused(o, contract.ExitFormat, ReasonUnknownRef, "no repository repo-9")
	})
	t.Run("unknown input id", func(t *testing.T) {
		e := newEnv(t, "fast-min", testutil.ShogunValid)
		o := e.opts()
		o.Inputs = map[string]string{"in-7": e.planPath}
		e.refused(o, contract.ExitFormat, ReasonUnknownRef, "no input in-7")
	})
	t.Run("manifest locator binding", func(t *testing.T) {
		e := newEnv(t, "fast-min", testutil.ShogunValid)
		o := e.opts()
		o.Repos = nil
		e.refused(o, contract.ExitFormat, ReasonRepo, "repo-1")
		// The root is not bound by the digest: pointing it at the checkout keeps the plan valid.
		p := filepath.Join(e.lib, "plan.manifest.json")
		b, _ := os.ReadFile(p)
		var m map[string]any
		json.Unmarshal(b, &m)
		m["repos"].([]any)[0].(map[string]any)["root"] = e.repo
		b, _ = json.MarshalIndent(m, "", " ")
		os.WriteFile(p, b, 0o644)
		res := e.ok(o)
		if res.Contract.Repos[0].BoundBy != "manifest_locator" || !strings.Contains(strings.Join(res.Notes, " "), "from the manifest locator") {
			t.Fatalf("binding %+v notes %v", res.Contract.Repos[0], res.Notes)
		}
	})
	t.Run("store inside the repository", func(t *testing.T) {
		e := newEnv(t, "fast-min", testutil.ShogunValid)
		e.cfg.Config.StoreDir = filepath.Join(e.repo, ".niten-store")
		e.store = e.cfg.Config.StoreDir
		e.refused(e.opts(), contract.ExitFormat, ReasonStore, "must not contain each other")
	})
}

func TestPrepareRejectsUnsafeOrAmbiguousPlans(t *testing.T) {
	for fixture, tc := range map[string]struct {
		exit   contract.ExitCode
		reason string
		detail string
	}{
		"fast-protected":      {contract.ExitFormat, ReasonProtectedTarget, ".claude/settings.json"},
		"fast-traversal":      {contract.ExitFormat, ReasonContract, "path escapes the repository"},
		"fast-multiline":      {contract.ExitFormat, ReasonInvalidFormat, "plan file line"},
		"fast-forged-heading": {contract.ExitFormat, ReasonInvalidFormat, `unexpected heading "## Steps"`},
		"fast-forged-step":    {contract.ExitFormat, ReasonInvalidFormat, "S-777"},
	} {
		t.Run(fixture, func(t *testing.T) {
			e := newEnv(t, fixture, testutil.ShogunValid)
			e.refused(e.opts(), tc.exit, tc.reason, tc.detail)
		})
	}
}

// Human ownership is an explicit, pre-run user setting; measure is not silently converted.
func TestPrepareOwners(t *testing.T) {
	e := newEnv(t, "fast-measure", testutil.ShogunValid)
	e.refused(e.opts(), contract.ExitNeedsInput, ReasonNeedsInput, "unsupported_method_measure: S-001/V-001")
	o := e.opts()
	o.Human = []string{"S-001/V-001", "R-001.C1"}
	g := true
	o.GatePerStep = &g
	c := e.ok(o).Contract
	if c.Checks[0].Owner != "human" || c.Criteria[0].Owner != "human" || !c.GatePerStep {
		t.Fatalf("owners %+v %+v gate %v", c.Checks, c.Criteria, c.GatePerStep)
	}
	o.Human = []string{"R-009.C1"}
	os.RemoveAll(filepath.Join(e.store, "runs"))
	e.refused(o, contract.ExitFormat, ReasonUnknownRef, "no such criterion")
}
