//go:build darwin

package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/killabayte/niten/internal/config"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/prepare"
	"github.com/killabayte/niten/internal/store"
	"github.com/killabayte/niten/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.MaybeFakeCLI()
	// Git ignores the user's configuration in every test; set once so the
	// scenarios can run in parallel.
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}

// world is one engine test: the calc fixture repository, a prepared run of the
// go-two-step plan, scripted executor and reviewer CLIs and the real verifier.
type world struct {
	t     *testing.T
	dir   string
	repo  string
	store *store.Store
	runID string
	state string
	cfg   config.Config
}

type setup struct {
	gate  bool
	human []string
	cfg   func(*config.Config)
}

func newWorld(t *testing.T, script testutil.FakeScript, s setup) *world {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, dir: dir}
	w.repo = testutil.CalcRepo(t, dir)
	planPath := testutil.CopyFixture(t, "go-two-step", filepath.Join(dir, "library"))
	claude, codex, state := testutil.FakeModels(t, dir, script)
	w.state = state
	cfg := config.Defaults()
	cfg.StoreDir = filepath.Join(dir, "state", "niten")
	cfg.ClaudeCommand, cfg.CodexCommand = claude, codex
	cfg.ShogunCommand = testutil.FakeShogun(t, dir, testutil.ShogunValid, filepath.Join(dir, "shogun.log"))
	if s.cfg != nil {
		s.cfg(&cfg)
	}
	w.cfg = cfg
	opts := prepare.Options{PlanPath: planPath, Repos: map[string]string{"repo-1": w.repo}, Config: &config.Loaded{Config: cfg}, Version: "test", Human: s.human}
	if s.gate {
		g := true
		opts.GatePerStep = &g
	}
	res, err := prepare.Prepare(context.Background(), opts)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	w.runID = res.RunID
	if w.store, err = store.Open(cfg.StoreDir); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) open() *Engine {
	w.t.Helper()
	e, err := Open(Options{Store: w.store, RunID: w.runID, Out: testWriter{w.t}})
	if err != nil {
		w.t.Fatalf("open: %v", err)
	}
	return e
}

// run starts the prepared run and returns how the session ended.
func (w *world) run() (Outcome, *Engine) {
	w.t.Helper()
	e := w.open()
	out, err := e.Run(context.Background())
	if err != nil {
		w.t.Fatalf("run: %v (outcome %+v)", err, out)
	}
	return out, e
}

func (w *world) resume(r ResumeOptions) (Outcome, *Engine) {
	w.t.Helper()
	e := w.open()
	out, err := e.Resume(context.Background(), r)
	if err != nil {
		w.t.Fatalf("resume: %v (outcome %+v)", err, out)
	}
	return out, e
}

type testWriter struct{ t *testing.T }

func (tw testWriter) Write(p []byte) (int, error) {
	tw.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// Fixture code.
const (
	calcSub     = "package calc\n\n// Add returns the sum of a and b.\nfunc Add(a, b int) int { return a + b }\n\n// Sub returns a minus b.\nfunc Sub(a, b int) int { return a - b }\n"
	calcSubMul  = calcSub + "\n// Mul returns a times b.\nfunc Mul(a, b int) int { return a * b }\n"
	testAdd     = "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"Add(2, 3) != 5\")\n\t}\n}\n"
	testSub     = testAdd + "\nfunc TestSub(t *testing.T) {\n\tif Sub(5, 3) != 2 {\n\t\tt.Fatal(\"Sub(5, 3) != 2\")\n\t}\n}\n"
	testSubNeg  = testSub + "\nfunc TestSubNegative(t *testing.T) {\n\tif Sub(3, 5) != -2 {\n\t\tt.Fatal(\"Sub(3, 5) != -2\")\n\t}\n}\n"
	testSubMul  = testSubNeg + "\nfunc TestMul(t *testing.T) {\n\tif Mul(4, 3) != 12 {\n\t\tt.Fatal(\"Mul(4, 3) != 12\")\n\t}\n}\n"
	calcGo      = "calc.go"
	calcTestGo  = "calc_test.go"
	descMarker  = "DESCRIPTION-MARKER: everything is done and all tests pass"
	notesMarker = "HANDOFF-MARKER: the sign convention follows the plan"
)

func handoff(notes ...string) contract.Handoff {
	return contract.Handoff{ImplementationNotes: append([]string{}, notes...), RemainingWork: []string{}, Risks: []string{}, DecisionRefs: []string{}, EvidenceRefs: []string{}}
}

func goTest(id, crit, verif string) contract.CheckSpecProposal {
	zero := 0
	return contract.CheckSpecProposal{ID: id, Method: contract.MethodTest, Argv: []string{"go", "test", "./..."}, Cwd: ".",
		Expected: contract.CheckExpected{ExitCode: &zero}, CriterionIDs: []string{crit}, VerificationIDs: []string{verif}}
}

func candidate(step string, claimed []string, checks ...contract.CheckSpecProposal) contract.CandidateReady {
	return contract.CandidateReady{Steps: []string{step}, Description: descMarker, ChangedPathsClaimed: claimed,
		OffTargetJustifications: []contract.OffTargetJustification{}, ProposedChecks: append([]contract.CheckSpecProposal{}, checks...),
		Questions: []string{}, Handoff: handoff(notesMarker)}
}

func payload(t testing.TB, v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func execAction(t testing.TB, write map[string]string, turn contract.ExecutorTurn) testutil.FakeAction {
	if turn.Responses == nil {
		turn.Responses = []contract.Response{}
	}
	if turn.Questions == nil {
		turn.Questions = []contract.Question{}
	}
	return testutil.FakeAction{Write: write, Payload: payload(t, turn)}
}

func review(verdict contract.Verdict, crit, paths []string) contract.ReviewerTurn {
	return contract.ReviewerTurn{ReviewedCommit: "{{CANDIDATE}}", Review: contract.ReviewResult{Verdict: verdict,
		Coverage:            contract.ReviewCoverage{CriterionIDsChecked: crit, PathsReviewed: paths, OffTargetDispositions: []contract.OffTargetReviewDisposition{}},
		FindingDispositions: []contract.FindingDisposition{}, Summary: "reviewed"},
		Findings: []contract.Finding{}, CheckRequests: []contract.CheckRequest{}, TestAssessments: []contract.TestAssessment{}, Questions: []contract.Question{}}
}

func revAction(t testing.TB, r contract.ReviewerTurn) testutil.FakeAction {
	return testutil.FakeAction{Payload: payload(t, r)}
}

var (
	both   = []string{calcGo, calcTestGo}
	major1 = contract.Finding{FindingID: "F-001", Severity: contract.SeverityMajor, CriterionIDs: []string{"R-001.C1"},
		Location: contract.Location{Path: calcTestGo, LineStart: 11}, DefectScenario: "Sub(3, 5) is not covered; a sign error would pass the tests.",
		ExpectedFix: "Add a test for a negative difference.", EvidenceRefs: []string{}}
)

// The happy path of the two-step plan, as listed in the roadmap's P3 acceptance.
func happyScript(t testing.TB) testutil.FakeScript {
	s1 := candidate("S-001", both, goTest("go-test", "R-001.C1", "S-001/V-001"))
	s1fix := candidate("S-001", []string{calcTestGo})
	s2 := candidate("S-002", both, goTest("go-test", "R-002.C1", "S-002/V-001"))
	revise := review(contract.VerdictRevise, []string{"R-001.C1"}, both)
	revise.Findings = []contract.Finding{major1}
	revise.Review.FindingDispositions = []contract.FindingDisposition{{FindingID: "F-001", State: contract.FindingOpen}}
	fixed := review(contract.VerdictApprove, []string{"R-001.C1"}, both)
	fixed.Review.FindingDispositions = []contract.FindingDisposition{{FindingID: "F-001", State: contract.FindingFixed}}
	return testutil.FakeScript{
		Executor: []testutil.FakeAction{
			execAction(t, map[string]string{calcGo: calcSub, calcTestGo: testSub}, contract.ExecutorTurn{Candidate: s1}),
			execAction(t, map[string]string{calcTestGo: testSubNeg}, contract.ExecutorTurn{Candidate: s1fix,
				Responses: []contract.Response{{FindingID: "F-001", Disposition: contract.ResponseFixed, Explanation: "Added TestSubNegative.", EvidenceRefs: []string{calcTestGo + ":15"}}}}),
			execAction(t, map[string]string{calcGo: calcSubMul, calcTestGo: testSubMul}, contract.ExecutorTurn{Candidate: s2}),
		},
		Reviewer: []testutil.FakeAction{
			revAction(t, revise),
			revAction(t, fixed),
			revAction(t, review(contract.VerdictApprove, []string{"R-002.C1"}, both)),
			revAction(t, review(contract.VerdictApprove, []string{"R-001.C1", "R-002.C1"}, both)),
		},
	}
}

func readJSON(t *testing.T, e *Engine, rel string, v any) {
	t.Helper()
	b, err := e.run.ReadArtifact(rel, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func TestTwoStepPlanReachesDoneAfterTheReceipt(t *testing.T) {
	t.Parallel()
	w := newWorld(t, happyScript(t), setup{})
	out, e := w.run()
	defer e.Close()
	if out.State != contract.RunDone || out.Exit != contract.ExitOK {
		t.Fatalf("outcome %+v", out)
	}
	st := e.State()
	s1, s2 := st.Unit("S-001"), st.Unit("S-002")
	if s1.Repairs != 1 || s1.Reviews != 2 || s2.Repairs != 0 || s2.Reviews != 1 || st.Invocations != 7 {
		t.Fatalf("S-001 repairs %d reviews %d, S-002 repairs %d reviews %d, invocations %d", s1.Repairs, s1.Reviews, s2.Repairs, s2.Reviews, st.Invocations)
	}
	if f := st.Finding("F-001"); f == nil || f.State != contract.FindingFixed || len(f.Responses) != 1 {
		t.Fatalf("F-001: %+v", f)
	}
	// The re-review and its checks refer to the repaired SHA, not the first one.
	var reviews []*TurnView
	for _, tv := range st.Turns {
		if tv.Unit == "S-001" && tv.Kind == KindReview {
			reviews = append(reviews, tv)
		}
	}
	if len(reviews) != 2 || reviews[0].Candidate == reviews[1].Candidate || reviews[1].Candidate != s1.AcceptedAt {
		t.Fatalf("S-001 reviews: %+v, accepted at %s", reviews, s1.AcceptedAt)
	}
	checked := map[string]bool{}
	for _, ev := range e.events {
		if ev.Type == evChecks {
			var d transition
			json.Unmarshal(ev.Data, &d)
			checked[d.Checks.Candidate] = d.Checks.Passed
		}
	}
	if !checked[reviews[0].Candidate] || !checked[reviews[1].Candidate] {
		t.Fatalf("checks per candidate: %v", checked)
	}
	// done is recorded only after the receipt.
	receiptAt, doneAt := int64(0), int64(0)
	for _, ev := range e.events {
		if ev.Type == evReceipt {
			receiptAt = ev.Seq
		}
		if ev.Type == evRunState && strings.Contains(string(ev.Data), `"done"`) {
			doneAt = ev.Seq
		}
	}
	if receiptAt == 0 || doneAt <= receiptAt {
		t.Fatalf("receipt at %d, done at %d", receiptAt, doneAt)
	}
	var r Receipt
	readJSON(t, e, "execution.json", &r)
	head, _ := e.clone.Head(context.Background())
	if r.Status != contract.RunDone || r.Final.Commit != head || len(r.Criteria) != 2 || r.Criteria[0].Status != "covered" || r.Criteria[1].Status != "covered" {
		t.Fatalf("receipt: status %s final %s (head %s) criteria %+v", r.Status, r.Final.Commit, head, r.Criteria)
	}
	for _, c := range r.Checks {
		if c.Status != contract.CheckPassed {
			t.Fatalf("final check %+v", c)
		}
	}
	// The final tree holds both functions and all tests.
	for p, want := range map[string]string{calcGo: calcSubMul, calcTestGo: testSubMul} {
		b, ok, err := e.clone.FileAt(context.Background(), head, p)
		if err != nil || !ok || string(b) != want {
			t.Fatalf("%s at the final commit: %q %v", p, b, err)
		}
	}
	// The first review is blind to the executor's account; the repair sees the handoff as notes.
	prompts, _ := testutil.FakeCalls(t, w.state, "reviewer")
	if strings.Contains(prompts[0], descMarker) || strings.Contains(prompts[0], notesMarker) {
		t.Fatal("the first review packet contains the executor's success narrative or handoff")
	}
	if !strings.Contains(prompts[1], "Added TestSubNegative.") {
		t.Fatal("the re-review packet lacks the executor's response")
	}
	eprompts, _ := testutil.FakeCalls(t, w.state, "executor")
	if !strings.Contains(eprompts[1], "Unverified notes") || !strings.Contains(eprompts[1], notesMarker) || !strings.Contains(eprompts[1], "F-001") {
		t.Fatal("the repair packet lacks the labeled handoff or the open finding")
	}
}
