package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/store"
	"github.com/killabayte/niten/internal/workspace"
)

func ev(seq int64, at, typ string, data any) store.Event {
	b, _ := json.Marshal(data)
	return store.Event{Seq: seq, Time: at, Type: typ, Data: b}
}

// Active time accrues only inside sessions: waiting in needs_input or paused
// between sessions costs nothing.
func TestActiveTimeExcludesWaiting(t *testing.T) {
	s := newState("r", "c", "p", "base", []string{"S-001"}, Limits{})
	for _, e := range []store.Event{
		ev(1, "2026-10-01T10:00:00Z", evSession, sessionData{}),
		ev(2, "2026-10-01T10:00:10Z", evRunState, runStateData{State: contract.RunRunning}),
		ev(3, "2026-10-01T10:05:00Z", evRunState, runStateData{State: contract.RunNeedsInput, Reason: "step_gate"}),
		ev(4, "2026-10-01T12:00:00Z", evSession, sessionData{}),
		ev(5, "2026-10-01T12:01:00Z", evRunState, runStateData{State: contract.RunDone}),
	} {
		if err := s.apply(e); err != nil {
			t.Fatal(err)
		}
	}
	if s.ActiveMS != (6 * 60 * 1000) {
		t.Fatalf("active %d ms, want 6 minutes", s.ActiveMS)
	}
}

func TestReducerRefusesInconsistentEvents(t *testing.T) {
	s := newState("r", "c", "p", "base", []string{"S-001"}, Limits{})
	turn := TurnView{ID: "t001-s-001-implement", Role: "executor", Kind: KindImplement, Unit: "S-001"}
	steps := []store.Event{
		ev(1, "2026-10-01T10:00:00Z", evTurn, turn),
		ev(2, "2026-10-01T10:00:01Z", evProcessed, processedData{Turn: turn.ID, Outcome: OutcomeFailed}),
	}
	for _, e := range steps {
		if err := s.apply(e); err != nil {
			t.Fatal(err)
		}
	}
	for name, e := range map[string]store.Event{
		"processed twice": ev(3, "2026-10-01T10:00:02Z", evProcessed, processedData{Turn: turn.ID, Outcome: OutcomeApplied}),
		"started twice":   ev(3, "2026-10-01T10:00:02Z", evTurn, turn),
		"unknown turn":    ev(3, "2026-10-01T10:00:02Z", evProcessed, processedData{Turn: "t999", Outcome: OutcomeApplied}),
		"unknown unit":    ev(3, "2026-10-01T10:00:02Z", evStepState, transition{Unit: "S-404", Step: &stepStateData{State: contract.StepImplementing}}),
		"closed gate":     ev(3, "2026-10-01T10:00:02Z", evGateReleased, gateReleasedData{Gate: "gate-x"}),
		"bad data":        {Seq: 3, Time: "2026-10-01T10:00:02Z", Type: evTurn, Data: json.RawMessage(`"x"`)},
	} {
		c := *s
		if err := c.apply(e); !errors.Is(err, store.ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTestChangesFlags(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/calc_test.go b/calc_test.go",
		"--- a/calc_test.go",
		"+++ b/calc_test.go",
		"-func TestOld(t *testing.T) {",
		"+\tt.Skip(\"later\")",
		"diff --git a/x/testdata/golden.txt b/x/testdata/golden.txt",
		"-1",
		"+2",
		"diff --git a/calc.go b/calc.go",
		"+\tt.Skip(\"not a test file\")",
		"diff --git a/web/app.spec.ts b/web/app.spec.ts",
		"+  it.only('focus', () => {})",
	}, "\n")
	changes := []workspace.Change{{Path: "calc_test.go", Status: "M"}, {Path: "x/testdata/golden.txt", Status: "M"}, {Path: "calc.go", Status: "M"},
		{Path: "old_test.go", Status: "D"}, {Path: "web/app.spec.ts", Status: "M"}, {Path: "new_test.go", Status: "A"}}
	got := map[string]string{}
	for _, f := range testChanges(changes, []byte(diff)) {
		got[f.Path] = f.Why
	}
	for p, want := range map[string]string{"calc_test.go": "removes a test", "old_test.go": "deleted", "x/testdata/golden.txt": "test data", "web/app.spec.ts": "adds a skip"} {
		if !strings.Contains(got[p], want) {
			t.Errorf("%s: %q, want %q", p, got[p], want)
		}
	}
	if !strings.Contains(got["calc_test.go"], "adds a skip") {
		t.Errorf("calc_test.go: %q", got["calc_test.go"])
	}
	for _, p := range []string{"calc.go", "new_test.go"} {
		if _, ok := got[p]; ok {
			t.Errorf("%s is flagged: %q", p, got[p])
		}
	}
}

func TestRefsResolve(t *testing.T) {
	r := refs{exact: map[string]bool{"m-t001-01": true, "checks/c1/evidence.json": true}, paths: map[string]bool{"calc.go": true, "a:b.go": true}}
	for ref, ok := range map[string]bool{"m-t001-01": true, "checks/c1/evidence.json": true, "calc.go": true, "calc.go:12": true, "calc.go:3-9": true,
		"a:b.go": true, "a:b.go:4": true, "calc.go:x": false, "ghost.go": false, "checks/c2/evidence.json": false, "../calc.go": false} {
		if r.resolves(ref) != ok {
			t.Errorf("%q resolves %v", ref, !ok)
		}
	}
}

func TestParseModel(t *testing.T) {
	m, err := parseModel("claude/claude-opus-5-5:xhigh", "claude", "claude")
	if err != nil || m.Model != "claude-opus-5-5" || m.Effort != "xhigh" {
		t.Fatalf("%+v %v", m, err)
	}
	for _, bad := range []string{"claude-opus-5-5", "claude/opus", "codex/gpt-6-astra:xhigh", "claude/:xhigh"} {
		if _, err := parseModel(bad, "claude", "claude"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The built-in settings template is the example users copy; they must not drift.
func TestSettingsTemplateMatchesTheExample(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "examples", "claude-settings.template.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, defaultSettingsTemplate) {
		t.Fatal("internal/engine/assets/claude-settings.template.json differs from examples/claude-settings.template.json")
	}
}

func TestEvidenceDigestAndGateBindTheCandidate(t *testing.T) {
	r := []CheckOutcome{{ID: "required/go-tests", Status: contract.CheckPassed, EvidenceSHA: strings.Repeat("a", 64)}}
	a := evidenceDigest("c1", r, []string{})
	if a == evidenceDigest("c2", r, []string{}) || a == evidenceDigest("c1", r, []string{"x"}) {
		t.Fatal("the evidence digest ignores the candidate or the problems")
	}
	r2 := []CheckOutcome{{ID: "required/go-tests", Status: contract.CheckPassed, EvidenceSHA: strings.Repeat("b", 64)}}
	if a == evidenceDigest("c1", r2, []string{}) {
		t.Fatal("the evidence digest ignores the evidence")
	}
	u1 := &StepView{ID: "S-001", AcceptedAt: strings.Repeat("1", 40)}
	u2 := &StepView{ID: "S-001", AcceptedAt: strings.Repeat("2", 40)}
	if gateID(u1) == gateID(u2) {
		t.Fatal("a gate does not depend on the accepted candidate")
	}
}

// A crashed session's downtime is not active time: the next session start is
// a boundary, whatever the reducer saw last.
func TestCrashDowntimeIsNotActiveTime(t *testing.T) {
	s := newState("r", "c", "p", "base", []string{"S-001"}, Limits{MaxActiveTime: "10m"})
	for _, e := range []store.Event{
		ev(1, "2026-10-01T00:00:00Z", evSession, sessionData{Command: "run"}),
		ev(2, "2026-10-01T00:00:01Z", evRunState, runStateData{State: contract.RunRunning}),
		// The coordinator crashed here, without a stop event.
		ev(3, "2026-10-01T01:00:01Z", evSession, sessionData{Command: "recover"}),
		ev(4, "2026-10-01T01:00:03Z", evRunState, runStateData{State: contract.RunRunning}),
	} {
		if err := s.apply(e); err != nil {
			t.Fatal(err)
		}
	}
	if s.ActiveMS != 3000 {
		t.Fatalf("active %d ms, want 3000: the crash downtime was counted", s.ActiveMS)
	}
}

// An accepted check keeps its id and definition: a later one under the same
// id is never added in its place.
func TestAcceptedChecksAreNeverReplaced(t *testing.T) {
	u := newUnit("S-001")
	strict := CheckSpec{ID: "S-001/go-test", Expected: contract.CheckExpected{StdoutContains: []string{"ok"}}}
	u.addSpecs([]CheckSpec{strict})
	u.addSpecs([]CheckSpec{{ID: "S-001/go-test"}})
	if len(u.Specs) != 1 || len(u.Specs[0].Expected.StdoutContains) != 1 {
		t.Fatalf("specs %+v", u.Specs)
	}
}
