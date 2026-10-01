//go:build darwin

package verify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/store"
	"github.com/killabayte/niten/internal/testutil"
	"github.com/killabayte/niten/internal/verify/sandbox"
	"github.com/killabayte/niten/internal/workspace"
)

type world struct {
	v     *Verifier
	clone *workspace.Clone
	run   *store.Run
	head  string
}

func goroot(t *testing.T) string {
	t.Helper()
	root := runtime.GOROOT()
	if _, err := os.Stat(filepath.Join(root, "bin", "go")); err != nil {
		out, err := exec.Command("go", "env", "GOROOT").Output()
		if err != nil {
			t.Skip("no go toolchain")
		}
		root = strings.TrimSpace(string(out))
	}
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newWorld(t *testing.T) *world {
	t.Helper()
	testutil.IsolateGit(t)
	base := t.TempDir()
	src := filepath.Join(base, "demo")
	os.MkdirAll(src, 0o755)
	files := map[string]string{
		"go.mod":    "module example.com/demo\n\ngo 1.26\n",
		"a.go":      "package demo\n\nfunc A() int { return 1 }\n",
		"a_test.go": "package demo\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tif A() != 1 {\n\t\tt.Fatal(\"A\")\n\t}\n}\n",
	}
	for p, c := range files {
		os.WriteFile(filepath.Join(src, p), []byte(c), 0o644)
	}
	testutil.Git(t, src, "init", "-q")
	testutil.Git(t, src, "add", "-A")
	testutil.Git(t, src, "commit", "-qm", "base")
	head := testutil.Git(t, src, "rev-parse", "HEAD")
	clone, err := workspace.CreateClone(context.Background(), src, head, filepath.Join(base, "work", "src"), filepath.Join(base, "gitdir"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := store.NewRunID(time.Now())
	st, _ := s.Stage(id)
	st.Write("state.json", []byte("{}"), 0o600)
	st.Commit()
	run, _, err := s.OpenRun(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run.Close() })
	backend, err := sandbox.New(filepath.Join(base, "profiles"))
	if err != nil {
		t.Skip("no sandbox backend:", err)
	}
	return &world{clone: clone, run: run, head: head, v: &Verifier{Backend: backend, Clone: clone, Store: run,
		Root: filepath.Join(base, "verify"), Toolchains: []string{goroot(t)}, ToolVersion: runtime.Version()}}
}

// candidate commits the given test file content as a new candidate.
func (w *world) candidate(t *testing.T, testBody string) workspace.Candidate {
	t.Helper()
	ctx := context.Background()
	os.WriteFile(filepath.Join(w.clone.Work, "a_test.go"), []byte(testBody), 0o644)
	ins, err := w.clone.Inspect(ctx, workspace.Rules{Targets: []string{"a_test.go"}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := w.clone.Commit(ctx, ins, "candidate", time.Now())
	if errors.Is(err, workspace.ErrNoChanges) {
		head, _ := w.clone.Head(ctx)
		tree, _ := w.clone.TreeOf(ctx, head)
		return workspace.Candidate{Commit: head, Tree: tree}
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func test(body string) string {
	return "package demo\n\nimport (\n\t\"os\"\n\t\"testing\"\n\t\"time\"\n)\n\nvar _ = os.Getpid\nvar _ = time.Now\n\nfunc TestA(t *testing.T) {\n" + body + "\n}\n"
}

func req(c workspace.Candidate, chk Check) Request {
	return Request{Check: chk, Candidate: c, PlanDigest: strings.Repeat("a", 64), ContractDigest: strings.Repeat("b", 64)}
}

func goTest(timeout time.Duration) Check {
	return Check{ID: "go-tests", Argv: []string{"go", "test", "-count=1", "./..."}, Cwd: ".", Timeout: timeout}
}

func TestPassingCheckIsRecordedAfterSeal(t *testing.T) {
	w := newWorld(t)
	c := w.candidate(t, test(`	if A() != 1 { t.Fatal("A") }`))
	chk := goTest(3 * time.Minute)
	chk.Expected.StdoutContains = []string{"ok"}
	res, err := w.v.Run(context.Background(), req(c, chk))
	if err != nil {
		t.Fatal(err)
	}
	ev := res.Evidence
	if ev.Status != contract.CheckPassed || !ev.SourcesUnchanged || ev.ExitCode == nil || *ev.ExitCode != 0 || len(res.Reasons) != 0 {
		t.Fatalf("evidence %+v reasons %v", ev, res.Reasons)
	}
	if ev.Key.CandidateCommit != c.Commit || ev.Key.CandidateTree != c.Tree || !filepath.IsAbs(ev.Argv[0]) {
		t.Fatalf("key/argv %+v %v", ev.Key, ev.Argv)
	}
	// The roots were sealed: the attempt paths are gone, the sealed ones hold the copy.
	if _, err := os.Stat(filepath.Join(w.v.Root, res.AttemptID, "src")); !os.IsNotExist(err) {
		t.Fatal("the attempt source root was not sealed")
	}
	if _, err := os.Stat(filepath.Join(res.Sealed.SourceRoot, "go.mod")); err != nil {
		t.Fatalf("sealed copy: %v", err)
	}
	b, err := w.run.ReadArtifact(res.EvidenceRef, res.EvidenceDigest)
	if err != nil || contract.ValidateRecord(contract.RecordCheck, b) != nil {
		t.Fatalf("stored evidence: %v", err)
	}
	if out, err := w.run.ReadArtifact(ev.StdoutRef, ev.StdoutSHA256); err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("stored stdout %q %v", out, err)
	}
	if _, err := w.run.ReadArtifact(ev.StderrRef, ev.StderrSHA256); err != nil {
		t.Fatalf("stored stderr does not match its recorded digest: %v", err)
	}
	w.run.Close()
	s, _ := store.Open(filepath.Dir(filepath.Dir(w.run.Dir)))
	r, events, err := s.OpenRun(w.run.ID)
	if err != nil || len(events) != 1 || events[0].Type != "check.recorded" {
		t.Fatalf("events %+v %v", events, err)
	}
	r.Close()
	// Equal environments give equal keys across attempts with different roots.
	w2 := newWorld(t)
	c2 := w2.candidate(t, test(`	if A() != 1 { t.Fatal("A") }`))
	res2, err := w2.v.Run(context.Background(), req(c2, chk))
	if err != nil || res2.Evidence.Key.EnvironmentDigest != ev.Key.EnvironmentDigest || res2.Evidence.Key.CheckSpecDigest != ev.Key.CheckSpecDigest {
		t.Fatalf("environment digest is not stable: %v", err)
	}
}

func TestCheckOutcomes(t *testing.T) {
	w := newWorld(t)
	for name, tc := range map[string]struct {
		body    string
		timeout time.Duration
		expect  func(*Check)
		status  contract.CheckStatus
		reason  string
	}{
		"failing test":         {body: `	t.Fatal("boom")`, timeout: 3 * time.Minute, status: contract.CheckFailed, reason: "exit code"},
		"test edits the code":  {body: `	os.WriteFile("a.go", []byte("package demo\nfunc A() int { return 1 }\n"), 0o644)`, timeout: 3 * time.Minute, status: contract.CheckInvalidated, reason: "a.go: content changed"},
		"stdout expectation":   {body: ``, timeout: 3 * time.Minute, expect: func(c *Check) { c.Expected.StdoutContains = []string{"processed 10 objects"} }, status: contract.CheckFailed, reason: "stdout contains"},
		"timeout":              {body: `	time.Sleep(time.Minute)`, timeout: 4 * time.Second, status: contract.CheckFailed, reason: "finished within the timeout"},
		"wrong exit code want": {body: ``, timeout: 3 * time.Minute, expect: func(c *Check) { two := 2; c.Expected.ExitCode = &two }, status: contract.CheckFailed, reason: "exit code"},
	} {
		t.Run(name, func(t *testing.T) {
			c := w.candidate(t, test(tc.body))
			chk := goTest(tc.timeout)
			if tc.expect != nil {
				tc.expect(&chk)
			}
			res, err := w.v.Run(context.Background(), req(c, chk))
			if err != nil {
				t.Fatal(err)
			}
			if res.Evidence.Status != tc.status || !strings.Contains(strings.Join(res.Reasons, "\n"), tc.reason) {
				t.Fatalf("status %s reasons %v, want %s with %q", res.Evidence.Status, res.Reasons, tc.status, tc.reason)
			}
			if tc.status == contract.CheckInvalidated && res.Evidence.SourcesUnchanged {
				t.Fatal("sources_unchanged must be false")
			}
		})
	}
}

// A process the check did not start holds the new roots: the sandbox refuses
// the run, nothing is killed, and the evidence is unknown, never passed.
func TestForeignHolderMakesTheEvidenceUnknown(t *testing.T) {
	w := newWorld(t)
	c := w.candidate(t, test(`	time.Sleep(2 * time.Second)`))
	w.v.newID = func() string { return "check-foreign" }
	src := filepath.Join(w.v.Root, "check-foreign", "src")
	started := make(chan *exec.Cmd, 1)
	go func() {
		for i := 0; i < 500; i++ {
			if _, err := os.Stat(filepath.Join(src, "go.mod")); err == nil {
				o := exec.Command("/bin/sleep", "30")
				o.Dir = src
				o.Start()
				started <- o
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		started <- nil
	}()
	res, err := w.v.Run(context.Background(), req(c, goTest(3*time.Minute)))
	observer := <-started
	if observer == nil {
		t.Fatal("the observer did not start")
	}
	defer observer.Process.Kill()
	if err != nil {
		t.Fatal(err)
	}
	if res.Evidence.Status != contract.CheckUnknown || res.Evidence.SourcesUnchanged || !strings.Contains(strings.Join(res.Reasons, " "), "outside the attempt's process group") {
		t.Fatalf("evidence %+v reasons %v", res.Evidence, res.Reasons)
	}
	if observer.ProcessState != nil {
		t.Fatal("the foreign holder was killed")
	}
}

// When the store cannot take the evidence, nothing is recorded: no event, and
// the caller gets an error instead of a result it could act on.
func TestStoreFailureRecordsNothing(t *testing.T) {
	w := newWorld(t)
	c := w.candidate(t, test(``))
	os.Chmod(w.run.Dir, 0o500)
	defer os.Chmod(w.run.Dir, 0o700)
	if res, err := w.v.Run(context.Background(), req(c, goTest(3*time.Minute))); err == nil {
		t.Fatalf("a result was returned although the store refused it: %+v", res)
	}
	os.Chmod(w.run.Dir, 0o700)
	w.run.Close()
	s, _ := store.Open(filepath.Dir(filepath.Dir(w.run.Dir)))
	r, events, err := s.OpenRun(w.run.ID)
	if err != nil || len(events) != 0 {
		t.Fatalf("events after a refused write: %+v %v", events, err)
	}
	r.Close()
}

func TestRootsAreNeverReused(t *testing.T) {
	w := newWorld(t)
	c := w.candidate(t, test(``))
	w.v.newID = func() string { return "check-fixed" }
	if _, err := w.v.Run(context.Background(), req(c, goTest(3*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := w.v.Run(context.Background(), req(c, goTest(3*time.Minute))); err == nil || !strings.Contains(err.Error(), "new roots") {
		t.Fatalf("a second attempt reused the roots: %v", err)
	}
	if _, err := w.v.Run(context.Background(), req(c, Check{ID: "x", Argv: []string{"sh", "-c", "true"}, Cwd: ".", Timeout: time.Second})); err == nil {
		t.Fatal("a command outside the toolchains was resolved")
	}
	if _, err := w.v.Run(context.Background(), req(c, Check{ID: "x", Argv: []string{"go", "version"}, Cwd: "../..", Timeout: time.Second})); err == nil {
		t.Fatal("a cwd outside the repository was accepted")
	}
}

// Review regression (P2): a check that writes a new source file (here the
// implementation the test needs) must not pass; the new file invalidates the
// evidence unless it is a declared output.
func TestCheckThatAddsSourcesIsInvalidated(t *testing.T) {
	w := newWorld(t)
	script := "#!/bin/sh\nset -eu\nprintf 'package demo\\nfunc B() int { return 1 }\\n' > generated.go\n" + w.v.Toolchains[0] + "/bin/go test -count=1 ./...\n"
	os.WriteFile(filepath.Join(w.clone.Work, "check.sh"), []byte(script), 0o644)
	c := w.candidate(t, test(`	if B() != 1 { t.Fatal("B") }`))
	chk := Check{ID: "generated-implementation", Argv: []string{"/bin/sh", "./check.sh"}, Cwd: ".", Timeout: 3 * time.Minute}
	res, err := w.v.Run(context.Background(), req(c, chk))
	if err != nil {
		t.Fatal(err)
	}
	if res.Evidence.Status != contract.CheckInvalidated || res.Evidence.SourcesUnchanged || !strings.Contains(strings.Join(res.Reasons, " "), "generated.go: added by the check") {
		t.Fatalf("status=%s sources_unchanged=%v reasons=%v", res.Evidence.Status, res.Evidence.SourcesUnchanged, res.Reasons)
	}
	// A declared output is allowed.
	cov := Check{ID: "coverage", Argv: []string{"go", "test", "-count=1", "-coverprofile=cover.out", "./..."}, Cwd: ".", Timeout: 3 * time.Minute, Outputs: []string{"cover.out"}}
	c2 := w.candidate(t, test(``))
	res2, err := w.v.Run(context.Background(), req(c2, cov))
	if err != nil || res2.Evidence.Status != contract.CheckPassed {
		t.Fatalf("declared output: %v %+v %v", err, res2.Evidence, res2.Reasons)
	}
}

// Review regression (P2, round 2): a check that edits code inside an ident
// keyword is invalidated, end to end.
func TestIdentMutationInvalidatesTheEvidence(t *testing.T) {
	w := newWorld(t)
	for name, body := range map[string]string{
		".gitattributes": "a.go ident\n",
		"a.go":           "package demo\nconst Value = \"$Id$\"\nfunc A() int { return 1 }\n",
		"check.sh":       "#!/bin/sh\nset -eu\nprintf 'package demo\\nconst Value = \"$Id: altered implementation $\"\\nfunc A() int { return 1 }\\n' > a.go\n" + w.v.Toolchains[0] + "/bin/go test -count=1 ./...\n",
	} {
		os.WriteFile(filepath.Join(w.clone.Work, name), []byte(body), 0o644)
	}
	c := w.candidate(t, test(`	if Value != "$Id: altered implementation $" { t.Fatal(Value) }`))
	res, err := w.v.Run(context.Background(), req(c, Check{ID: "ident", Argv: []string{"/bin/sh", "./check.sh"}, Cwd: ".", Timeout: 3 * time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Evidence.Status != contract.CheckInvalidated || res.Evidence.SourcesUnchanged {
		t.Fatalf("status=%s sources_unchanged=%v reasons=%v", res.Evidence.Status, res.Evidence.SourcesUnchanged, res.Reasons)
	}
}
