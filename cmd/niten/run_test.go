package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.MaybeFakeCLI()
	os.Exit(m.Run())
}

// runWorld prepares the go-two-step plan through the CLI with scripted models.
func runWorld(t *testing.T, script testutil.FakeScript) (cfg, runID, state string) {
	t.Helper()
	testutil.IsolateGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := testutil.CalcRepo(t, root)
	plan := testutil.CopyFixture(t, "go-two-step", filepath.Join(root, "library"))
	claude, codex, state := testutil.FakeModels(t, root, script)
	shogun := testutil.FakeShogun(t, root, testutil.ShogunValid, filepath.Join(root, "shogun.log"))
	cfg = filepath.Join(root, "niten.toml")
	toml := "shogun_command = \"" + shogun + "\"\nstore_dir = \"" + filepath.Join(root, "state", "niten") + "\"\n" +
		"claude_command = \"" + claude + "\"\ncodex_command = \"" + codex + "\"\n"
	os.WriteFile(cfg, []byte(toml), 0o600)
	home := filepath.Join(root, "home")
	old := getenv
	getenv = func(k string) string {
		switch k {
		case "HOME":
			return home
		case "PATH":
			return os.Getenv("PATH")
		}
		return ""
	}
	t.Cleanup(func() { getenv = old })
	code, out, errb := runCLI("prepare", plan, "--repo", "repo-1="+repo, "--config", cfg, "--json")
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != contract.ExitOK {
		t.Fatalf("prepare: %d %v\n%s\n%s", code, err, out, errb)
	}
	return cfg, res["run_id"].(string), state
}

func pl(t *testing.T, v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cliScript(t *testing.T) testutil.FakeScript {
	zero := 0
	h := contract.Handoff{ImplementationNotes: []string{}, RemainingWork: []string{}, Risks: []string{}, DecisionRefs: []string{}, EvidenceRefs: []string{}}
	step := func(id, crit, verif, code, test string) testutil.FakeAction {
		return testutil.FakeAction{Write: map[string]string{"calc.go": code, "calc_test.go": test}, Payload: pl(t, contract.ExecutorTurn{
			Candidate: contract.CandidateReady{Steps: []string{id}, Description: "done", ChangedPathsClaimed: []string{"calc.go", "calc_test.go"},
				OffTargetJustifications: []contract.OffTargetJustification{}, Questions: []string{}, Handoff: h,
				ProposedChecks: []contract.CheckSpecProposal{{ID: "go-test", Method: contract.MethodTest, Argv: []string{"go", "test", "./..."}, Cwd: ".",
					Expected: contract.CheckExpected{ExitCode: &zero}, CriterionIDs: []string{crit}, VerificationIDs: []string{verif}}}},
			Responses: []contract.Response{}, Questions: []contract.Question{}})}
	}
	approve := func(crit ...string) testutil.FakeAction {
		return testutil.FakeAction{Payload: pl(t, contract.ReviewerTurn{ReviewedCommit: "{{CANDIDATE}}", Review: contract.ReviewResult{Verdict: contract.VerdictApprove,
			Coverage:            contract.ReviewCoverage{CriterionIDsChecked: crit, PathsReviewed: []string{"calc.go", "calc_test.go"}, OffTargetDispositions: []contract.OffTargetReviewDisposition{}},
			FindingDispositions: []contract.FindingDisposition{}, Summary: "ok"}, Findings: []contract.Finding{}, CheckRequests: []contract.CheckRequest{},
			TestAssessments: []contract.TestAssessment{}, Questions: []contract.Question{}})}
	}
	sub := "package calc\n\nfunc Add(a, b int) int { return a + b }\n\nfunc Sub(a, b int) int { return a - b }\n"
	mul := sub + "\nfunc Mul(a, b int) int { return a * b }\n"
	tsub := testutil.CalcFiles["calc_test.go"] + "\nfunc TestSub(t *testing.T) {\n\tif Sub(5, 3) != 2 || Sub(3, 5) != -2 {\n\t\tt.Fatal(\"Sub\")\n\t}\n}\n"
	tmul := tsub + "\nfunc TestMul(t *testing.T) {\n\tif Mul(4, 3) != 12 {\n\t\tt.Fatal(\"Mul\")\n\t}\n}\n"
	return testutil.FakeScript{
		Executor: []testutil.FakeAction{step("S-001", "R-001.C1", "S-001/V-001", sub, tsub), step("S-002", "R-002.C1", "S-002/V-001", mul, tmul)},
		Reviewer: []testutil.FakeAction{approve("R-001.C1"), approve("R-002.C1"), approve("R-001.C1", "R-002.C1")},
	}
}

func TestRunStatusResumeCLI(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the verifier sandbox is macOS-only")
	}
	cfg, runID, _ := runWorld(t, cliScript(t))
	code, out, _ := runCLI("status", runID, "--config", cfg)
	if code != contract.ExitOK || !strings.Contains(out, "run "+runID+": prepared") {
		t.Fatalf("status before run: %d\n%s", code, out)
	}
	if code, _, errb := runCLI("resume", runID, "--config", cfg); code != contract.ExitFormat || !strings.Contains(errb, "niten run") {
		t.Fatalf("resume of a prepared run: %d %s", code, errb)
	}
	code, out, errb := runCLI("run", runID, "--config", cfg, "--json")
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != contract.ExitOK || res["state"] != "done" {
		t.Fatalf("run: %d %v\n%s\n%s", code, err, out, errb)
	}
	if !strings.Contains(errb, "S-001: accepted at") || !strings.Contains(errb, "final: accepted at") {
		t.Fatalf("progress lines missing:\n%s", errb)
	}
	code, out, _ = runCLI("status", runID, "--config", cfg)
	if code != contract.ExitOK || !strings.Contains(out, ": done") || !strings.Contains(out, "receipt: receipts/1-done.json (done)") || !strings.Contains(out, "invocations 5 of 24") {
		t.Fatalf("status after run: %d\n%s", code, out)
	}
	code, out, _ = runCLI("status", runID, "--config", cfg, "--json")
	var st map[string]any
	if err := json.Unmarshal([]byte(out), &st); err != nil || code != contract.ExitOK || st["state"] != "done" {
		t.Fatalf("status --json: %d %v", code, err)
	}
	if code, out, _ := runCLI("resume", runID, "--config", cfg); code != contract.ExitOK || !strings.Contains(out, ": done") {
		t.Fatalf("resume of a done run: %d %s", code, out)
	}
	if code, _, errb := runCLI("run", runID, "--config", cfg); code != contract.ExitFormat || !strings.Contains(errb, "not prepared") {
		t.Fatalf("second run: %d %s", code, errb)
	}
}

func TestRunCLIArgumentErrors(t *testing.T) {
	for _, args := range [][]string{{"run"}, {"run", "a", "b"}, {"status"}, {"status", "../escape"}, {"resume"}, {"resume", "x", "--max-time", "soon"}} {
		if code, _, _ := runCLI(args...); code != contract.ExitFormat {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	cfg, runID, _ := runWorld(t, testutil.FakeScript{})
	if code, _, errb := runCLI("status", "20990101-000000-abcdef", "--config", cfg); code != contract.ExitFormat || !strings.Contains(errb, "no run") {
		t.Fatalf("status of a missing run: %d %s", code, errb)
	}
	if code, _, errb := runCLI("resume", runID, "--config", cfg, "--answers", filepath.Join(t.TempDir(), "missing.json")); code != contract.ExitFormat || !strings.Contains(errb, "--answers") {
		t.Fatalf("missing answers file: %d %s", code, errb)
	}
}
