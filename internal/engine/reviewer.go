package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"

	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/killabayte/niten/internal/attempt"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/provider"
	"github.com/killabayte/niten/internal/workspace"
)

// Limits of the evidence a reviewer may leave in its evidence directory.
const (
	maxEvidenceFiles = 32
	maxEvidenceBytes = 1 << 20
	maxPacketStream  = 64 << 10
)

// reviewView is the coordinator's view of what a review covers.
type reviewView struct {
	start     string
	commit    string
	cmp       *workspace.Inspection
	diff      []byte
	tests     []testFlag
	required  []string // mandatory coordinator-owned criteria the approval must cover
	refs      refs
	evidence  map[string]string // evidence/<name> -> store ref
	firstPass bool
}

func (e *Engine) unitStart(u *StepView) string {
	if u.ID == FinalUnit {
		return e.st.Base
	}
	return u.Start
}

// requiredCriteria are the mandatory, coordinator-owned criteria of a unit.
func (e *Engine) requiredCriteria(u *StepView) []string {
	carried := e.unitCriteria(u)
	var out []string
	for _, c := range e.c.Criteria {
		if c.Mandatory && c.Owner == string(contract.OwnerNiten) && slices.Contains(carried, c.ID) {
			out = append(out, c.ID)
		}
	}
	return out
}

func (e *Engine) reviewContext(ctx context.Context, u *StepView) (*reviewView, error) {
	rv := &reviewView{start: e.unitStart(u), commit: u.Candidate.Commit, required: e.requiredCriteria(u), evidence: map[string]string{}}
	var err error
	if rv.cmp, err = e.clone.Compare(ctx, rv.start, rv.commit, e.rules(u)); err != nil {
		return nil, err
	}
	if rv.diff, err = e.clone.Diff(ctx, rv.start, rv.commit); err != nil {
		return nil, err
	}
	rv.tests = testChanges(rv.cmp.Changes, rv.diff)
	if rv.refs, err = e.refsFor(ctx, u, rv.commit, rv.cmp); err != nil {
		return nil, err
	}
	rv.firstPass = u.Reviews == 0
	return rv, nil
}

// reviewerRequest is the adapter request of a reviewer attempt in its directory.
func (e *Engine) reviewerRequest(base string) provider.CodexRequest {
	return provider.CodexRequest{Model: e.rev.Model, Effort: e.rev.Effort, SchemaPath: filepath.Join(base, "control", "schema.json"),
		LastPath: filepath.Join(base, "control", "last.json"), Launcher: filepath.Join(base, "launcher"),
		Validate: func(b []byte) error { return contract.ValidateDocument(contract.DocReviewerTurn, b) }}
}

func (e *Engine) reviewBase(id string) string { return filepath.Join(e.work, "review", id) }

// reviewerTurn runs one reviewer invocation on a fresh copy of the candidate.
func (e *Engine) reviewerTurn(ctx context.Context, u *StepView) (*Outcome, error) {
	if out, err := e.budget(); out != nil || err != nil {
		return out, err
	}
	if u.Checks == nil || u.Checks.Candidate != u.Candidate.Commit || !u.Checks.Passed {
		return nil, fmt.Errorf("%s is reviewing without passed checks of its candidate", u.ID)
	}
	kind := KindReview
	if u.ID == FinalUnit {
		kind = KindFinalReview
	}
	id := e.turnID(u.ID, kind)
	base := e.reviewBase(id)
	launcher := filepath.Join(base, "launcher")
	for _, d := range []string{base, filepath.Join(base, "control"), launcher, filepath.Join(launcher, "packet", "checks"), filepath.Join(launcher, "evidence")} {
		if err := newDir(d); err != nil {
			return nil, err
		}
	}
	if err := e.clone.Materialize(ctx, u.Candidate.Commit, filepath.Join(launcher, "source")); err != nil {
		return nil, err
	}
	rv, err := e.reviewContext(ctx, u)
	if err != nil {
		return nil, err
	}
	if err := e.writePacket(launcher, u, rv); err != nil {
		return nil, err
	}
	req := e.reviewerRequest(base)
	schema, err := contract.Bundle(contract.DocReviewerTurn)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(req.SchemaPath, schema, 0o600); err != nil {
		return nil, err
	}
	if err := provider.CheckFreshOutput(req.LastPath); err != nil {
		return nil, err
	}
	prompt, err := e.reviewerPrompt(u, kind, rv)
	if err != nil {
		return nil, err
	}
	env, stripped, err := e.childEnv(filepath.Join(launcher, "scratch"), provider.CodexRustLog)
	if err != nil {
		return nil, err
	}
	if err := e.emit(evTurn, TurnView{ID: id, Role: string(contract.RoleReviewer), Kind: kind, Unit: u.ID, Candidate: u.Candidate.Commit,
		Evidence: u.Checks.Digest, PacketRef: "attempts/" + id + "/prompt", PacketSHA: digest(prompt)}); err != nil {
		return nil, err
	}
	e.logf("%s: %s of %s (%s)", u.ID, kind, short(u.Candidate.Commit), id)
	res, perr, err := e.attempt(ctx, attempt.Spec{ID: id, Role: string(contract.RoleReviewer), Bin: e.rev.Bin, Args: provider.CodexArgs(req),
		Env: env, Stripped: stripped, Dir: launcher, Stdin: prompt, Deadline: e.deadline(),
		Parse: func(so, se string, o provider.Outcome) (*provider.Result, *provider.Error) {
			return provider.ParseCodex(so, se, o, req)
		}})
	if err != nil {
		return nil, err
	}
	return e.processReviewer(context.WithoutCancel(ctx), e.st.Turn(id), res, perr)
}

// writePacket puts the diff and the coordinator's check evidence next to the
// reviewer's copy. The reviewer may change them; the coordinator never reads them back.
func (e *Engine) writePacket(launcher string, u *StepView, rv *reviewView) error {
	if err := os.WriteFile(filepath.Join(launcher, "packet", "diff.patch"), rv.diff, 0o600); err != nil {
		return err
	}
	for _, r := range u.Checks.Results {
		name := strings.ReplaceAll(r.ID, "/", "__")
		ev, err := e.run.ReadArtifact(r.EvidenceRef, r.EvidenceSHA)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(launcher, "packet", "checks", name+".json"), ev, 0o600); err != nil {
			return err
		}
		dir := strings.TrimSuffix(r.EvidenceRef, "/evidence.json")
		for _, s := range []string{"stdout", "stderr"} {
			b, err := e.run.ReadArtifact(dir+"/"+s, "")
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(launcher, "packet", "checks", name+"."+s), tail(b, maxPacketStream), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func tail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append([]byte("[... truncated ...]\n"), b[len(b)-n:]...)
}

// collectEvidence stores the regular files the reviewer left in its evidence
// directory, without following links, as review artifacts.
func (e *Engine) collectEvidence(t *TurnView, rv *reviewView) ([]string, error) {
	dir := filepath.Join(e.reviewBase(t.ID), "launcher", "evidence")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var notes []string
	n := 0
	for _, de := range entries {
		fi, err := os.Lstat(filepath.Join(dir, de.Name()))
		if err != nil {
			return nil, err
		}
		switch {
		case !fi.Mode().IsRegular():
			notes = append(notes, "evidence/"+de.Name()+" is not a regular file and was not collected")
			continue
		case fi.Size() > maxEvidenceBytes || n >= maxEvidenceFiles:
			notes = append(notes, "evidence/"+de.Name()+" exceeds the evidence limits and was not collected")
			continue
		}
		b, err := readNoFollow(filepath.Join(dir, de.Name()), maxEvidenceBytes)
		if err != nil {
			return nil, err
		}
		ref := "reviews/" + t.ID + "/evidence/" + de.Name()
		if _, err := e.writeOrReuse(ref, b); err != nil {
			return nil, err
		}
		rv.evidence["evidence/"+de.Name()] = ref
		rv.refs.exact["evidence/"+de.Name()] = true
		n++
	}
	return notes, nil
}

// processReviewer applies one finished reviewer attempt.
func (e *Engine) processReviewer(ctx context.Context, t *TurnView, res *provider.Result, perr *provider.Error) (*Outcome, error) {
	u := e.st.Unit(t.Unit)
	if perr != nil {
		if err := e.emit(evProcessed, processedData{Turn: t.ID, Outcome: OutcomeFailed, Class: string(perr.Class), Reasons: []string{perr.Msg}}); err != nil {
			return nil, err
		}
		return e.stopForClass(perr)
	}
	if u.State != contract.StepReviewing || u.Candidate == nil || u.Candidate.Commit != t.Candidate || u.Checks == nil || u.Checks.Digest != t.Evidence {
		return nil, e.emit(evProcessed, processedData{Turn: t.ID, Outcome: OutcomeStale, Reported: res.Reported.Model,
			Reasons: []string{fmt.Sprintf("the review is bound to %s with evidence %s; %s has moved on", short(t.Candidate), short(t.Evidence), u.ID)}})
	}
	var rt contract.ReviewerTurn
	if err := json.Unmarshal(res.Payload, &rt); err != nil {
		return nil, fmt.Errorf("decode a validated reviewer turn: %w", err)
	}
	rv, err := e.reviewContext(ctx, u)
	if err != nil {
		return nil, err
	}
	notes, err := e.collectEvidence(t, rv)
	if err != nil {
		return nil, err
	}
	v := e.validateReviewer(u, t, &rt, rv)
	if len(v.reasons) > 0 {
		if err := e.emit(evProcessed, processedData{Turn: t.ID, Outcome: OutcomeRejected, Reasons: v.reasons, Reported: res.Reported.Model}); err != nil {
			return nil, err
		}
		return e.stop(contract.RunPaused, "invalid_result", v.reasons, contract.ExitPaused)
	}
	pd, accepts, err := e.applyReview(u, t, &rt, rv, v)
	if err != nil {
		return nil, err
	}
	pd.Reported = res.Reported.Model
	pd.Review.Reasons = append(pd.Review.Reasons, notes...)
	if err := e.emit(evProcessed, pd); err != nil {
		return nil, err
	}
	switch {
	case rt.Review.Verdict == contract.VerdictBlocked:
		return e.stop(contract.RunNeedsInput, "review_blocked", []string{rt.Review.Summary}, contract.ExitNeedsInput)
	case len(pd.Questions) > 0:
		return e.askQuestions(pd.Questions)
	}
	if accepts {
		e.logf("%s: accepted at %s", u.ID, short(u.Candidate.Commit))
	} else {
		e.logf("%s: changes requested: %s", u.ID, strings.Join(pd.Review.Reasons, "; "))
	}
	return nil, nil
}

// validateReviewer resolves the reviewer's claims. An approval must name the
// reviewed commit and cover every required criterion, every changed path,
// every off-target edit and every flagged test change.
func (e *Engine) validateReviewer(u *StepView, t *TurnView, rt *contract.ReviewerTurn, rv *reviewView) *validation {
	v := &validation{}
	if rt.ReviewedCommit != t.Candidate {
		v.bad("the review names commit %s, but this attempt reviewed %s", rt.ReviewedCommit, t.Candidate)
	}
	crit := e.unitCriteria(u)
	known := func(ids []string, what string) {
		for _, c := range ids {
			if !slices.Contains(crit, c) {
				v.bad("%s names criterion %s, which %s does not carry", what, c, u.ID)
			}
		}
	}
	cov := rt.Review.Coverage
	known(cov.CriterionIDsChecked, "the coverage")
	diff := rv.cmp.Paths()
	for _, p := range cov.PathsReviewed {
		if !rv.refs.paths[p] {
			v.bad("the coverage lists %s, which is neither in the candidate nor in its diff", p)
		}
	}
	offDone := map[string]bool{}
	for _, d := range cov.OffTargetDispositions {
		if !slices.Contains(rv.cmp.OffTarget, d.Path) {
			v.bad("an off-target disposition names %s, which is not an off-target change", d.Path)
		}
		offDone[d.Path] = true
	}
	flagged := map[string]bool{}
	for _, f := range rv.tests {
		flagged[f.Path] = true
	}
	assessed := map[string]bool{}
	for _, a := range rt.TestAssessments {
		if !slices.Contains(diff, a.Path) {
			v.bad("a test assessment names %s, which the diff does not change", a.Path)
		}
		assessed[a.Path] = true
	}
	newIDs := map[string]bool{}
	for _, f := range rt.Findings {
		known(f.CriterionIDs, "finding "+f.FindingID)
		if newIDs[f.FindingID] {
			v.bad("finding %s is reported twice", f.FindingID)
		}
		newIDs[f.FindingID] = true
		if old := e.st.Finding(f.FindingID); old != nil && !sameFinding(old.Finding, f) {
			v.bad("finding %s already exists with different content; new findings need new ids", f.FindingID)
		}
		if !rv.refs.paths[f.Location.Path] {
			v.bad("finding %s points at %s, which is neither in the candidate nor in its diff", f.FindingID, f.Location.Path)
		}
		for _, ref := range f.EvidenceRefs {
			if !rv.refs.resolves(ref) {
				v.bad("finding %s cites %q, which resolves to no evidence, message or path", f.FindingID, ref)
			}
		}
	}
	for _, d := range rt.Review.FindingDispositions {
		old := e.st.Finding(d.FindingID)
		switch {
		case old == nil && !newIDs[d.FindingID]:
			v.bad("a disposition names %s, which is not a finding", d.FindingID)
		case old != nil && u.ID != FinalUnit && old.Unit != u.ID:
			v.bad("a disposition names %s, a finding of %s; this review covers %s", d.FindingID, old.Unit, u.ID)
		}
	}
	for _, c := range rt.CheckRequests {
		known(c.CriterionIDs, "a check request")
		e.checkProposal(u, c.Proposal, string(contract.RoleReviewer), v)
	}
	if rt.Review.Verdict == contract.VerdictApprove {
		for _, c := range rv.required {
			if !slices.Contains(cov.CriterionIDsChecked, c) {
				v.bad("the approval does not cover criterion %s", c)
			}
		}
		for _, p := range diff {
			if !slices.Contains(cov.PathsReviewed, p) {
				v.bad("the approval does not cover the changed path %s", p)
			}
		}
		for _, p := range rv.cmp.OffTarget {
			if !offDone[p] {
				v.bad("the approval has no disposition for the off-target edit %s", p)
			}
		}
		for p := range flagged {
			if !assessed[p] {
				v.bad("the approval does not assess the flagged test change %s", p)
			}
		}
	}
	sort.Strings(v.reasons)
	return v
}

func sameFinding(a, b contract.Finding) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// applyReview builds the review's transition: messages, ledger updates, the
// accepted check requests and the acceptance decision.
func (e *Engine) applyReview(u *StepView, t *TurnView, rt *contract.ReviewerTurn, rv *reviewView, v *validation) (processedData, bool, error) {
	cid := *u.Candidate
	pd := processedData{Turn: t.ID, Outcome: OutcomeApplied, Specs: v.specs}
	ledger := map[string]FindingView{}
	for _, f := range e.st.Findings {
		ledger[f.FindingID] = cloneFinding(f)
	}
	changed := map[string]bool{}
	n := 0
	add := func(kind contract.MessageKind, payload any) (string, error) {
		n++
		m, err := e.envelope(t, n, kind, contract.RoleReviewer, e.unitSteps(u), cid, nil, payload)
		if err != nil {
			return "", err
		}
		pd.Messages = append(pd.Messages, m)
		return m.ID, nil
	}
	reviewMsg, err := add(contract.KindReviewResult, rt.Review)
	if err != nil {
		return pd, false, err
	}
	for _, f := range rt.Findings {
		id, err := add(contract.KindFinding, f)
		if err != nil {
			return pd, false, err
		}
		if _, exists := ledger[f.FindingID]; exists {
			continue // the same finding again: no state change
		}
		ledger[f.FindingID] = FindingView{Finding: f, Unit: u.ID, State: contract.FindingOpen, OpenedBy: id, OpenedAt: cid.Commit, UpdatedBy: id, Responses: []ResponseView{}}
		changed[f.FindingID] = true
	}
	for _, d := range rt.Review.FindingDispositions {
		f := ledger[d.FindingID]
		if f.State != d.State {
			f.State, f.UpdatedBy = d.State, reviewMsg
			ledger[d.FindingID] = f
			changed[d.FindingID] = true
		}
	}
	for _, c := range rt.CheckRequests {
		if _, err := add(contract.KindCheckRequest, c); err != nil {
			return pd, false, err
		}
	}
	for _, q := range rt.Questions {
		id, err := add(contract.KindQuestion, q)
		if err != nil {
			return pd, false, err
		}
		if q.NeededDecision {
			pd.Questions = append(pd.Questions, QuestionView{ID: id, From: string(contract.RoleReviewer), Text: q.Text})
		}
	}
	ids := make([]string, 0, len(changed))
	for id := range changed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		pd.Findings = append(pd.Findings, ledger[id])
	}
	var why, others []string
	if rt.Review.Verdict != contract.VerdictApprove {
		why = append(why, fmt.Sprintf("the reviewer's verdict is %s: %s", rt.Review.Verdict, rt.Review.Summary))
	}
	for _, f := range ledger {
		if f.State == contract.FindingOpen && (f.Severity == contract.SeverityBlocker || f.Severity == contract.SeverityMajor) && (u.ID == FinalUnit || f.Unit == u.ID) {
			others = append(others, fmt.Sprintf("%s finding %s is open: %s", f.Severity, f.FindingID, f.DefectScenario))
		}
	}
	for _, d := range rt.Review.Coverage.OffTargetDispositions {
		if d.Disposition == contract.OffTargetReject {
			others = append(others, fmt.Sprintf("the off-target edit %s was rejected: %s", d.Path, d.Reason))
		}
	}
	for _, a := range rt.TestAssessments {
		if a.Assessment == "weakens" {
			others = append(others, fmt.Sprintf("the test change in %s weakens the tests: %s", a.Path, a.Reason))
		}
	}
	sort.Strings(others)
	why = append(why, others...)
	accepts := len(why) == 0
	pd.Review = &ReviewView{Turn: t.ID, Candidate: cid.Commit, Evidence: t.Evidence, Verdict: rt.Review.Verdict, Accepts: accepts, Reasons: append(why, v.problems...)}
	switch {
	case rt.Review.Verdict == contract.VerdictBlocked:
	case accepts:
		pd.Accepted = &acceptedData{Candidate: cid.Commit, Review: t.ID, Evidence: t.Evidence}
	default:
		pd.Step = &stepStateData{State: contract.StepChangesRequested, Repairs: u.Repairs, Problems: append(slices.Clone(why), v.problems...)}
	}
	return pd, accepts, nil
}

// Test changes.

// testFlag is a test change the reviewer must assess explicitly.
type testFlag struct {
	Path string `json:"path"`
	Why  string `json:"why"`
}

var (
	reTestFile  = regexp.MustCompile(`(^|/)([^/]+_test\.go|test_[^/]+\.py|[^/]+_test\.py|[^/]+\.(test|spec)\.[jt]sx?)$`)
	reSkip      = regexp.MustCompile(`\bt\.Skip(Now|f)?\(|\btesting\.Short\(\)|^\s*//\s*(go:build|\+build)\s+ignore\b|@pytest\.mark\.skip|unittest\.skip|\b(xit|xdescribe|xtest)\(|\.(skip|only)\(`)
	reTestDecl  = regexp.MustCompile(`^\s*(func\s+(Test|Benchmark|Fuzz|Example)\w*|def\s+test_\w+|(it|test|describe)\()`)
	reDiffStart = regexp.MustCompile(`^diff --git a/(.+) b/(.+)$`)
)

func isTestPath(p string) bool {
	return reTestFile.MatchString(p) || strings.HasPrefix(p, "testdata/") || strings.Contains(p, "/testdata/")
}

// testChanges flags deleted test files, removed test declarations, added skips
// and changed test data in a diff. It is a mechanical focus list, not a verdict.
func testChanges(changes []workspace.Change, diff []byte) []testFlag {
	added, removed := map[string][]string{}, map[string][]string{}
	cur := ""
	sc := bufio.NewScanner(bytes.NewReader(diff))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		l := sc.Text()
		if m := reDiffStart.FindStringSubmatch(l); m != nil {
			cur = m[2]
			continue
		}
		switch {
		case strings.HasPrefix(l, "+++ ") || strings.HasPrefix(l, "--- "):
		case strings.HasPrefix(l, "+"):
			added[cur] = append(added[cur], l[1:])
		case strings.HasPrefix(l, "-"):
			removed[cur] = append(removed[cur], l[1:])
		}
	}
	var out []testFlag
	for _, ch := range changes {
		if !isTestPath(ch.Path) {
			continue
		}
		var why []string
		if ch.Status == "D" {
			why = append(why, "the test file is deleted")
		}
		if strings.Contains(ch.Path, "testdata/") && ch.Status != "A" {
			why = append(why, "test data (expected inputs or outputs) changed")
		}
		for _, l := range added[ch.Path] {
			if reSkip.MatchString(l) {
				why = append(why, "adds a skip: "+strings.TrimSpace(l))
			}
		}
		for _, l := range removed[ch.Path] {
			if reTestDecl.MatchString(l) {
				why = append(why, "removes a test: "+strings.TrimSpace(l))
			}
		}
		if len(why) > 0 {
			out = append(out, testFlag{Path: ch.Path, Why: strings.Join(why, "; ")})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// readNoFollow reads a regular file without following a final symlink.
func readNoFollow(p string, max int64) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > max {
		return nil, &fs.PathError{Op: "read", Path: p, Err: fs.ErrInvalid}
	}
	b := make([]byte, fi.Size())
	if _, err := f.ReadAt(b, 0); err != nil && fi.Size() > 0 {
		return nil, err
	}
	return b, nil
}
