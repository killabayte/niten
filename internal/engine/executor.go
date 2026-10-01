package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/attempt"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/holders"
	"github.com/killabayte/niten/internal/provider"
	"github.com/killabayte/niten/internal/store"
	"github.com/killabayte/niten/internal/workspace"
)

// turnID names a turn; it is also the attempt id.
func (e *Engine) turnID(unit, kind string) string {
	return fmt.Sprintf("t%03d-%s-%s", len(e.st.Turns)+1, strings.ToLower(unit), strings.ReplaceAll(kind, "_", "-"))
}

func executorKind(u *StepView) string {
	switch {
	case u.ID == FinalUnit:
		return KindFinalRepair
	case u.Candidate == nil:
		return KindImplement
	}
	return KindRepair
}

// executorRequest is the adapter request of every executor attempt; recovery
// parses saved streams with the same one.
func (e *Engine) executorRequest(settings string) (provider.ClaudeRequest, error) {
	schema, err := contract.Bundle(contract.DocExecutorTurn)
	if err != nil {
		return provider.ClaudeRequest{}, err
	}
	bash, err := e.allowedBash()
	if err != nil {
		return provider.ClaudeRequest{}, err
	}
	return provider.ClaudeRequest{Model: e.exec.Model, Effort: e.exec.Effort, Schema: schema, Settings: settings, AllowedBash: bash,
		MaxBudgetUSD: e.c.Limits.ClaudeMaxBudgetUSD,
		Validate:     func(b []byte) error { return contract.ValidateDocument(contract.DocExecutorTurn, b) }}, nil
}

// executorTurn runs one executor invocation for a unit and processes it.
func (e *Engine) executorTurn(ctx context.Context, u *StepView) (*Outcome, error) {
	if out, err := e.budget(); out != nil || err != nil {
		return out, err
	}
	if out, err := e.cleanResidue(ctx, u); out != nil || err != nil {
		return out, err
	}
	kind := executorKind(u)
	id := e.turnID(u.ID, kind)
	if err := e.clearUnrecorded(id); err != nil {
		return nil, err
	}
	prompt, err := e.executorPrompt(ctx, u, kind)
	if err != nil {
		return nil, err
	}
	// The turn is in the journal before any of its roots exist, so a crash
	// while they are prepared never makes the next turn reuse its id.
	if err := e.emit(evTurn, TurnView{ID: id, Role: string(contract.RoleExecutor), Kind: kind, Unit: u.ID, Base: e.st.Head,
		PacketRef: "attempts/" + id + "/prompt", PacketSHA: digest(prompt)}); err != nil {
		return nil, err
	}
	scratch, control := filepath.Join(e.work, "executor", id), filepath.Join(e.work, "control", id)
	for _, d := range []string{scratch, control} {
		if err := newDir(d); err != nil {
			return nil, err
		}
	}
	settings, _, err := e.renderSettings(control, scratch, e.denyPatterns(u))
	if err != nil {
		return nil, err
	}
	req, err := e.executorRequest(settings)
	if err != nil {
		return nil, err
	}
	env, stripped, err := e.childEnv(scratch)
	if err != nil {
		return nil, err
	}
	e.logf("%s: %s by the executor (%s)", u.ID, kind, id)
	res, perr, err := e.attempt(ctx, attempt.Spec{ID: id, Role: string(contract.RoleExecutor), Bin: e.exec.Bin, Args: provider.ClaudeArgs(req),
		Env: env, Stripped: stripped, Dir: e.clone.Work, Stdin: prompt, Deadline: e.deadline(),
		Parse: func(so, se string, o provider.Outcome) (*provider.Result, *provider.Error) {
			return provider.ParseClaude(so, se, o, req)
		}})
	if err != nil {
		return nil, err
	}
	return e.processExecutor(context.WithoutCancel(ctx), e.st.Turn(id), res, perr)
}

// clearUnrecorded removes roots left under the id of a turn the journal does
// not have: a crash before the turn was recorded. No attempt can have run
// there (attempts start only after their turn is recorded), and a root that
// any process still holds is refused, never removed.
func (e *Engine) clearUnrecorded(id string) error {
	if e.st.Turn(id) != nil {
		return fmt.Errorf("turn %s is already in the journal", id)
	}
	for _, root := range []string{filepath.Join(e.work, "executor", id), filepath.Join(e.work, "control", id), e.reviewBase(id)} {
		if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		pids, err := holders.List([]string{root})
		if err != nil {
			return fmt.Errorf("the leftover %s cannot be checked: %w", root, err)
		}
		if len(pids) > 0 {
			return fmt.Errorf("the leftover %s of an unrecorded turn is held by processes %v", root, pids)
		}
		if err := os.RemoveAll(root); err != nil {
			return err
		}
		e.logf("removed the leftover %s of an unrecorded turn", root)
	}
	return nil
}

// attempt runs one model attempt with a heartbeat while it runs.
func (e *Engine) attempt(ctx context.Context, s attempt.Spec) (*provider.Result, *provider.Error, error) {
	done := make(chan struct{})
	start := e.o.Now()
	go func() {
		t := time.NewTicker(e.o.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				e.logf("  %s running for %s", s.ID, e.o.Now().Sub(start).Round(time.Second))
			}
		}
	}()
	res, perr, err := e.runner.Run(ctx, s)
	close(done)
	if err == nil && e.crashAfter != nil {
		err = e.crashAfter(s.ID)
	}
	return res, perr, err
}

// rules are the inspection rules of a unit: the hard policy, the unit's
// targets and the metadata fingerprint taken after the coordinator's last commit.
func (e *Engine) rules(u *StepView) workspace.Rules {
	r := workspace.Rules{Protected: e.c.Policy.ProtectedPaths, Instruction: e.c.Policy.InstructionPaths, Metadata: e.st.Metadata}
	for _, t := range e.c.Targets {
		if u.ID != FinalUnit && t.StepID != u.ID {
			continue
		}
		r.Targets = append(r.Targets, t.Path)
		if t.Instruction != "" && t.Operation != "inspect" {
			r.InstructionTargets = append(r.InstructionTargets, t.Path)
		}
	}
	return r
}

// denyPatterns are the policy patterns the executor's file tools are denied:
// every protected path, and the instruction paths unless the unit targets an
// instruction file. A deny rule cannot be lifted for one file, so a targeted
// instruction file leaves the instruction patterns to the coordinator's
// inspection, which still rejects any untargeted instruction change.
func (e *Engine) denyPatterns(u *StepView) []string {
	out := slices.Clone(e.c.Policy.ProtectedPaths)
	if len(e.rules(u).InstructionTargets) == 0 {
		out = append(out, e.c.Policy.InstructionPaths...)
	}
	return out
}

// cleanResidue makes sure the next executor starts from the last candidate:
// anything left in the worktree is kept as a rejected snapshot and removed.
func (e *Engine) cleanResidue(ctx context.Context, u *StepView) (*Outcome, error) {
	ins, err := e.clone.Inspect(ctx, e.rules(u))
	if err != nil {
		return nil, err
	}
	if len(ins.Changes) == 0 && len(ins.Violations) == 0 {
		if len(ins.Ignored) == 0 {
			return nil, nil
		}
		// Ignored files (build outputs of the last session) are not part of any
		// candidate; they are removed so the next session starts clean.
		if err := e.clone.Restore(ctx); err != nil {
			return nil, err
		}
		fp, err := e.clone.MetadataFingerprint()
		if err != nil {
			return nil, err
		}
		return nil, e.emit(evResidue, transition{Unit: u.ID, Rejected: &rejectedData{Violations: []string{}, Metadata: fp}})
	}
	rej, out, err := e.discard(ctx, ins, fmt.Sprintf("residue-%03d", len(e.st.Turns)+1), e.o.Now())
	if out != nil || err != nil {
		return out, err
	}
	return nil, e.emit(evResidue, transition{Unit: u.ID, Rejected: rej})
}

// discard keeps a snapshot of the worktree under refs/niten/rejected and
// restores the worktree to the last candidate. Tampered git metadata stops the
// run instead: no git command may read a worktree whose metadata a model changed.
func (e *Engine) discard(ctx context.Context, ins *workspace.Inspection, name string, at time.Time) (*rejectedData, *Outcome, error) {
	if ins.Tree == "" {
		out, err := e.stop(contract.RunFailed, "clone_tampered", ins.Violations, contract.ExitFormat)
		return nil, out, err
	}
	rej := &rejectedData{Violations: ins.Violations}
	if rej.Violations == nil {
		rej.Violations = []string{}
	}
	snap, err := e.clone.SaveRejected(ctx, ins, name, at)
	if err != nil {
		return nil, nil, err
	}
	rej.Snapshot = "refs/niten/rejected/" + name + "@" + snap
	if err := e.clone.Restore(ctx); err != nil {
		return nil, nil, err
	}
	if rej.Metadata, err = e.clone.MetadataFingerprint(); err != nil {
		return nil, nil, err
	}
	return rej, nil, nil
}

// stopForClass maps a failed attempt to the state the run waits in.
func (e *Engine) stopForClass(perr *provider.Error) (*Outcome, error) {
	detail := []string{perr.Error()}
	switch perr.Class {
	case provider.ClassRateLimit:
		return e.stop(contract.RunPaused, "rate_limit", detail, contract.ExitPaused)
	case provider.ClassCanceled:
		return e.stop(contract.RunPaused, "interrupted", detail, contract.ExitInterrupted)
	case provider.ClassTimeout:
		return e.stop(contract.RunPaused, "timeout", detail, contract.ExitPaused)
	case provider.ClassProtocol, provider.ClassConfig:
		return e.stop(contract.RunFailed, string(perr.Class), detail, contract.ExitFormat)
	case provider.ClassTransport, provider.ClassRefusal:
		return e.stop(contract.RunPaused, string(perr.Class), detail, contract.ExitPaused)
	}
	return e.stop(contract.RunPaused, "invalid_result", detail, contract.ExitPaused)
}

// turnTime is the deterministic time of a turn's records: when it started.
func turnTime(t *TurnView) time.Time {
	at, err := time.Parse(time.RFC3339Nano, t.StartedAt)
	if err != nil {
		return time.Unix(0, 0).UTC()
	}
	return at
}

// processExecutor applies one finished executor attempt. Processing is
// idempotent: after a crash the same saved result yields the same candidate,
// artifacts and event.
func (e *Engine) processExecutor(ctx context.Context, t *TurnView, res *provider.Result, perr *provider.Error) (*Outcome, error) {
	u := e.st.Unit(t.Unit)
	at := turnTime(t)
	if perr != nil {
		ins, err := e.clone.Inspect(ctx, e.rules(u))
		if err != nil {
			return nil, err
		}
		pd := processedData{Turn: t.ID, Outcome: OutcomeFailed, Class: string(perr.Class), Reasons: []string{perr.Msg}}
		if len(ins.Changes) > 0 || len(ins.Violations) > 0 || len(ins.Ignored) > 0 {
			rej, out, err := e.discard(ctx, ins, t.ID, at)
			if out != nil || err != nil {
				return out, err
			}
			pd.Rejected = rej
		}
		if err := e.emit(evProcessed, pd); err != nil {
			return nil, err
		}
		return e.stopForClass(perr)
	}
	if u.State != contract.StepImplementing || t.Base != e.st.Head {
		return nil, e.emit(evProcessed, processedData{Turn: t.ID, Outcome: OutcomeStale, Reasons: []string{"the unit moved on before the result was processed"}})
	}
	var turn contract.ExecutorTurn
	if err := json.Unmarshal(res.Payload, &turn); err != nil {
		return nil, fmt.Errorf("decode a validated executor turn: %w", err)
	}
	ins, err := e.clone.Inspect(ctx, e.rules(u))
	if err != nil {
		return nil, err
	}
	reject := func(reason string, reasons []string) (*Outcome, error) {
		rej, out, err := e.discard(ctx, ins, t.ID, at)
		if out != nil || err != nil {
			return out, err
		}
		if err := e.emit(evProcessed, processedData{Turn: t.ID, Outcome: OutcomeRejected, Reasons: reasons, Reported: res.Reported.Model,
			transition: transition{Rejected: rej}}); err != nil {
			return nil, err
		}
		return e.stop(contract.RunPaused, reason, reasons, contract.ExitPaused)
	}
	if len(ins.Violations) > 0 {
		return reject("policy_violation", ins.Violations)
	}
	cand, changed, err := e.prepareCandidate(ctx, ins, t, at)
	if err != nil {
		return nil, err
	}
	cmp, err := e.clone.Compare(ctx, u.Start, cand.Commit, e.rules(u))
	if err != nil {
		return nil, err
	}
	v, err := e.validateExecutor(ctx, u, t, &turn, cand, cmp)
	if err != nil {
		return nil, err
	}
	if len(v.reasons) > 0 {
		return reject("invalid_result", v.reasons)
	}
	disputed := false
	for _, r := range turn.Responses {
		disputed = disputed || r.Disposition == contract.ResponseDisputed
	}
	pd := processedData{Turn: t.ID, Outcome: OutcomeApplied, Reported: res.Reported.Model, Specs: v.specs, Justifications: map[string]string{}}
	for _, j := range turn.Candidate.OffTargetJustifications {
		pd.Justifications[j.Path] = j.Reason
	}
	cid := contract.CandidateID{Commit: cand.Commit, Tree: cand.Tree, PlanDigest: e.c.PlanDigest, Generation: 1}
	msgs, findings, questions, err := e.executorMessages(t, u, &turn, cid)
	if err != nil {
		return nil, err
	}
	pd.Messages, pd.Findings, pd.Questions = msgs, findings, questions
	problems := slices.Clone(v.problems)
	unchanged := u.Candidate != nil && u.Candidate.Commit == cand.Commit
	switch {
	case unchanged && !disputed:
		problems = append(problems, "the repair changed no code and disputed no finding")
		pd.Step = &stepStateData{State: contract.StepChangesRequested, Repairs: u.Repairs, Problems: problems}
	case unchanged:
		// A dispute without a code change goes back to the reviewer for the same candidate.
		next := contract.StepCandidate
		if u.Checks != nil && u.Checks.Candidate == cand.Commit && u.Checks.Passed && len(v.specs) == 0 {
			next = contract.StepReviewing
		}
		pd.Step = &stepStateData{State: next, Repairs: u.Repairs, Problems: problems}
	default:
		if changed {
			if err := e.clone.Advance(ctx, cand); err != nil {
				return nil, err
			}
		}
		rec, err := e.candidateRecord(u, cid, cmp, pd.Justifications, t)
		if err != nil {
			return nil, err
		}
		pd.Candidate = rec
		pd.Step = &stepStateData{State: contract.StepCandidate, Repairs: u.Repairs, Problems: problems}
	}
	if err := e.emit(evProcessed, pd); err != nil {
		return nil, err
	}
	if unchanged && !disputed {
		return e.stop(contract.RunPaused, "no_progress", problems, contract.ExitPaused)
	}
	if len(questions) > 0 {
		return e.askQuestions(questions)
	}
	return nil, nil
}

// prepareCandidate writes the commit of the inspected snapshot without moving
// the branch; a snapshot equal to HEAD is HEAD itself: the unchanged candidate,
// or, after a crash between Advance and the journal event, the prepared one.
func (e *Engine) prepareCandidate(ctx context.Context, ins *workspace.Inspection, t *TurnView, at time.Time) (workspace.Candidate, bool, error) {
	cand, err := e.clone.Prepare(ctx, ins, "niten: "+t.ID, at)
	if errors.Is(err, workspace.ErrNoChanges) {
		parent, perr := e.clone.Parent(ctx, ins.Head)
		if perr != nil {
			return workspace.Candidate{}, false, perr
		}
		return workspace.Candidate{Commit: ins.Head, Tree: ins.Tree, Parent: parent}, false, nil
	}
	if err != nil {
		return workspace.Candidate{}, false, err
	}
	return cand, true, nil
}

// candidateRecord writes the coordinator's record of a candidate: the unit's
// cumulative changes since its start and its off-target edits.
func (e *Engine) candidateRecord(u *StepView, cid contract.CandidateID, cmp *workspace.Inspection, just map[string]string, t *TurnView) (*candidateData, error) {
	return e.candidateRecordAt(u, cid, cmp, just, "candidates/"+t.ID+".json", turnTime(t))
}

func (e *Engine) candidateRecordAt(u *StepView, cid contract.CandidateID, cmp *workspace.Inspection, just map[string]string, ref string, at time.Time) (*candidateData, error) {
	merged := map[string]string{}
	for p, r := range u.Justifications {
		merged[p] = r
	}
	for p, r := range just {
		merged[p] = r
	}
	rec := contract.CandidateRecord{SchemaVersion: contract.SchemaVersion, CandidateID: cid, ParentCandidateID: u.Candidate,
		StepIDs: e.unitSteps(u), ActualChangedPaths: cmp.Paths(), OffTargetChanges: []contract.OffTargetChange{}, HardPolicyViolations: []string{},
		CreatedAt: at.UTC().Format(time.RFC3339)}
	if rec.ActualChangedPaths == nil {
		rec.ActualChangedPaths = []string{}
	}
	for _, p := range cmp.OffTarget {
		oc := contract.OffTargetChange{Path: p, Disposition: contract.OffTargetPending}
		if r, ok := merged[p]; ok {
			r := r
			oc.ReasonClaimed = &r
		}
		rec.OffTargetChanges = append(rec.OffTargetChanges, oc)
	}
	b, err := json.MarshalIndent(rec, "", " ")
	if err != nil {
		return nil, err
	}
	if err := contract.ValidateRecord(contract.RecordCandidate, b); err != nil {
		return nil, fmt.Errorf("candidate record: %w", err)
	}
	sha, err := e.writeOrReuse(ref, append(b, '\n'))
	if err != nil {
		return nil, err
	}
	fp, err := e.clone.MetadataFingerprint()
	if err != nil {
		return nil, err
	}
	return &candidateData{Candidate: cid, Ref: ref, SHA: sha, Metadata: fp}, nil
}

// unitSteps are the plan steps a unit carries.
func (e *Engine) unitSteps(u *StepView) []string {
	if u.ID != FinalUnit {
		return []string{u.ID}
	}
	return slices.Clone(e.c.Order)
}

// executorMessages turns a valid executor turn into envelopes: the candidate
// announcement, one response per finding and one message per question.
func (e *Engine) executorMessages(t *TurnView, u *StepView, turn *contract.ExecutorTurn, cid contract.CandidateID) ([]messageData, []FindingView, []QuestionView, error) {
	var msgs []messageData
	var findings []FindingView
	var questions []QuestionView
	n := 0
	add := func(kind contract.MessageKind, replyTo *string, payload any) (string, error) {
		n++
		m, err := e.envelope(t, n, kind, contract.RoleExecutor, e.unitSteps(u), cid, replyTo, payload)
		if err != nil {
			return "", err
		}
		msgs = append(msgs, m)
		return m.ID, nil
	}
	if _, err := add(contract.KindCandidateReady, nil, turn.Candidate); err != nil {
		return nil, nil, nil, err
	}
	for _, r := range turn.Responses {
		f := e.st.Finding(r.FindingID)
		reply := f.OpenedBy
		id, err := add(contract.KindResponse, &reply, r)
		if err != nil {
			return nil, nil, nil, err
		}
		nf := cloneFinding(f)
		nf.Responses = append(nf.Responses, ResponseView{Message: id, Disposition: r.Disposition, Candidate: cid.Commit})
		findings = append(findings, nf)
	}
	for _, q := range turn.Questions {
		id, err := add(contract.KindQuestion, nil, q)
		if err != nil {
			return nil, nil, nil, err
		}
		if q.NeededDecision {
			questions = append(questions, QuestionView{ID: id, From: string(contract.RoleExecutor), Text: q.Text})
		}
	}
	return msgs, findings, questions, nil
}

func cloneFinding(f *FindingView) FindingView {
	c := *f
	c.Responses = slices.Clone(f.Responses)
	return c
}

// envelope stores one message: the coordinator sets every field but the
// payload, and the envelope must pass the contract's own rules.
func (e *Engine) envelope(t *TurnView, n int, kind contract.MessageKind, from contract.Role, steps []string, cid contract.CandidateID, replyTo *string, payload any) (messageData, error) {
	pb, err := json.Marshal(payload)
	if err != nil {
		return messageData{}, err
	}
	id := fmt.Sprintf("m-%s-%02d", t.ID, n)
	env := contract.Envelope{SchemaVersion: contract.SchemaVersion, RunID: e.st.RunID, Sequence: len(e.st.Messages) + n, MessageID: id,
		ReplyTo: replyTo, AttemptID: t.ID, StepIDs: steps, CandidateID: cid, From: from, To: contract.RoleCoordinator, Kind: kind, Payload: pb}
	b, err := json.MarshalIndent(env, "", " ")
	if err != nil {
		return messageData{}, err
	}
	if _, err := contract.ValidateEnvelope(b); err != nil {
		return messageData{}, fmt.Errorf("message %s: %w", id, err)
	}
	ref := "messages/" + id + ".json"
	sha, err := e.writeOrReuse(ref, append(b, '\n'))
	if err != nil {
		return messageData{}, err
	}
	return messageData{ID: id, Kind: kind, From: from, Ref: ref, SHA: sha}, nil
}

// askQuestions stops the run until the user answers the blocking questions.
func (e *Engine) askQuestions(qs []QuestionView) (*Outcome, error) {
	var detail []string
	for _, q := range qs {
		detail = append(detail, fmt.Sprintf("%s (%s): %s", q.ID, q.From, q.Text))
	}
	return e.stop(contract.RunNeedsInput, "question", detail, contract.ExitNeedsInput)
}

// writeOrReuse writes a write-once artifact, or accepts an identical one an
// interrupted processing already wrote. Different bytes are corruption.
func (e *Engine) writeOrReuse(ref string, data []byte) (string, error) {
	sha, err := e.run.WriteArtifact(ref, data, 0o600)
	if err == nil {
		return sha, nil
	}
	existing, rerr := e.run.ReadArtifact(ref, "")
	if rerr != nil {
		return "", err
	}
	if !bytes.Equal(existing, data) {
		return "", fmt.Errorf("%w: %s exists with different content", store.ErrCorrupt, ref)
	}
	return digest(existing), nil
}

// Validation of model claims.

type validation struct {
	reasons  []string // invented or contradictory references: the turn is invalid
	problems []string // policy refusals the repair must address
	specs    []CheckSpec
}

func (v *validation) bad(format string, a ...any) {
	v.reasons = append(v.reasons, fmt.Sprintf(format, a...))
}

var reCheckID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// unitCriteria are the criterion ids a unit carries.
func (e *Engine) unitCriteria(u *StepView) []string {
	if u.ID == FinalUnit {
		var out []string
		for _, c := range e.c.Criteria {
			out = append(out, c.ID)
		}
		return out
	}
	return slices.Clone(e.doc.Step(u.ID).CriterionIDs)
}

// unitVerifications are the scoped verification ids a unit carries.
func (e *Engine) unitVerifications(u *StepView) []string {
	var out []string
	for _, c := range e.c.Checks {
		if u.ID == FinalUnit || c.StepID == u.ID {
			out = append(out, c.ScopedID)
		}
	}
	return out
}

// refs resolves references a model makes: coordinator refs it was given,
// recorded messages, collected evidence, and paths of the candidate or of the
// unit's diff, optionally with a line or a line range.
type refs struct {
	exact map[string]bool
	paths map[string]bool
}

var reLines = regexp.MustCompile(`^[0-9]+(-[0-9]+)?$`)

func (r refs) resolves(ref string) bool {
	if r.exact[ref] {
		return true
	}
	p := ref
	if i := strings.LastIndex(ref, ":"); i > 0 && reLines.MatchString(ref[i+1:]) {
		p = ref[:i]
	}
	return r.paths[p]
}

// refsFor collects what a model of this unit may cite for this candidate.
func (e *Engine) refsFor(ctx context.Context, u *StepView, commit string, cmp *workspace.Inspection) (refs, error) {
	r := refs{exact: map[string]bool{}, paths: map[string]bool{}}
	for _, m := range e.st.Messages {
		r.exact[m] = true
	}
	for _, x := range e.packetRefs(u) {
		r.exact[x] = true
	}
	paths, err := e.clone.Paths(ctx, commit)
	if err != nil {
		return r, err
	}
	for _, p := range paths {
		r.paths[p] = true
	}
	for _, p := range cmp.Paths() {
		r.paths[p] = true
	}
	return r, nil
}

// packetRefs are the store refs of the coordinator evidence a unit's packets
// show: the evidence and streams of its latest checks.
func (e *Engine) packetRefs(u *StepView) []string {
	var out []string
	if u.Checks == nil {
		return out
	}
	for _, r := range u.Checks.Results {
		dir := strings.TrimSuffix(r.EvidenceRef, "/evidence.json")
		out = append(out, r.EvidenceRef, dir+"/stdout", dir+"/stderr")
	}
	return out
}

// validateExecutor resolves every claim of an executor turn against the
// coordinator's own view of the candidate.
func (e *Engine) validateExecutor(ctx context.Context, u *StepView, t *TurnView, turn *contract.ExecutorTurn, cand workspace.Candidate, cmp *workspace.Inspection) (*validation, error) {
	v := &validation{}
	steps := e.unitSteps(u)
	for _, s := range turn.Candidate.Steps {
		if !slices.Contains(steps, s) {
			v.bad("the candidate claims step %s, which this turn was not assigned", s)
		}
	}
	diff := map[string]bool{}
	for _, p := range cmp.Paths() {
		diff[p] = true
	}
	for _, p := range turn.Candidate.ChangedPathsClaimed {
		if !diff[p] {
			v.bad("the candidate claims %s changed, but the coordinator's diff of %s does not contain it", p, u.ID)
		}
	}
	for _, j := range turn.Candidate.OffTargetJustifications {
		if !slices.Contains(cmp.OffTarget, j.Path) {
			v.bad("the candidate justifies %s as an off-target edit, but it is not an off-target change of %s", j.Path, u.ID)
		}
	}
	r, err := e.refsFor(ctx, u, cand.Commit, cmp)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range turn.Candidate.ProposedChecks {
		if seen[p.ID] {
			v.bad("check %s is proposed twice", p.ID)
		}
		seen[p.ID] = true
		e.checkProposal(u, p, string(contract.RoleExecutor), v)
	}
	answered := map[string]bool{}
	for _, f := range turn.Responses {
		fv := e.st.Finding(f.FindingID)
		switch {
		case fv == nil:
			v.bad("response to %s, which is not a finding of this run", f.FindingID)
			continue
		case fv.Unit != u.ID:
			v.bad("response to %s, a finding of %s, not of %s", f.FindingID, fv.Unit, u.ID)
		case fv.State != contract.FindingOpen:
			v.bad("response to %s, which is %s", f.FindingID, fv.State)
		case answered[f.FindingID]:
			v.bad("two responses to %s", f.FindingID)
		}
		answered[f.FindingID] = true
		for _, ref := range f.EvidenceRefs {
			if !r.resolves(ref) {
				v.bad("response to %s cites %q, which resolves to no evidence, message or path", f.FindingID, ref)
			}
		}
	}
	decisions := map[string]bool{}
	for _, d := range e.doc.Decisions {
		decisions[d.ID] = true
	}
	for _, q := range e.st.Questions {
		if q.Answered {
			decisions[q.ID] = true
		}
	}
	for _, d := range turn.Candidate.Handoff.DecisionRefs {
		if !decisions[d] {
			v.bad("the handoff cites decision %q, which is neither a plan decision nor a user answer", d)
		}
	}
	for _, ref := range turn.Candidate.Handoff.EvidenceRefs {
		if !r.resolves(ref) {
			v.bad("the handoff cites %q, which resolves to no evidence, message or path", ref)
		}
	}
	sort.Strings(v.reasons)
	return v, nil
}

// checkProposal validates a proposed check: invented criteria or verifications
// make the turn invalid; a command outside the policy is a refused proposal.
func (e *Engine) checkProposal(u *StepView, p contract.CheckSpecProposal, source string, v *validation) {
	crit, verifs := e.unitCriteria(u), e.unitVerifications(u)
	for _, c := range p.CriterionIDs {
		if !slices.Contains(crit, c) {
			v.bad("check %s names criterion %s, which %s does not carry", p.ID, c, u.ID)
		}
	}
	for _, id := range p.VerificationIDs {
		if !slices.Contains(verifs, id) {
			v.bad("check %s names verification %s, which %s does not carry", p.ID, id, u.ID)
		}
	}
	refuse := func(format string, a ...any) {
		v.problems = append(v.problems, fmt.Sprintf("check %s refused: ", p.ID)+fmt.Sprintf(format, a...))
	}
	switch p.Method {
	case contract.MethodInspect:
		return // assessed by the reviewer, never run
	case contract.MethodMeasure:
		refuse("the measure method is not executable in v0.1")
		return
	}
	if !reCheckID.MatchString(p.ID) {
		refuse("the id must match %s", reCheckID)
		return
	}
	cmds, err := e.policyCommands()
	if err != nil {
		refuse("%v", err)
		return
	}
	var cmd *struct {
		id      string
		timeout time.Duration
	}
	for _, c := range cmds {
		if slices.Equal(c.Argv, p.Argv) {
			cmd = &struct {
				id      string
				timeout time.Duration
			}{c.ID, c.Timeout.D()}
			break
		}
	}
	if cmd == nil {
		refuse("argv %q is not one of the allowed commands", p.Argv)
		return
	}
	cwd := p.Cwd
	if cwd == "" {
		cwd = "."
	}
	if filepath.IsAbs(cwd) || filepath.ToSlash(filepath.Clean(cwd)) != cwd || cwd == ".." || strings.HasPrefix(cwd, "../") {
		refuse("cwd %q must be a clean relative directory", cwd)
		return
	}
	timeout := cmd.timeout
	if p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		if err != nil || d <= 0 || d > cmd.timeout {
			refuse("timeout %q must be positive and at most %s", p.Timeout, cmd.timeout)
			return
		}
		timeout = d
	}
	if re := p.Expected.StdoutRegex; re != "" {
		if _, err := regexp.Compile(re); err != nil {
			refuse("stdout_regex: %v", err)
			return
		}
	}
	spec := CheckSpec{ID: specID(u, source, p.ID), Source: source, CommandID: cmd.id, Argv: slices.Clone(p.Argv), Cwd: cwd,
		Timeout: timeout.String(), Expected: p.Expected, CriterionIDs: p.CriterionIDs, VerificationIDs: p.VerificationIDs}
	if i := slices.IndexFunc(u.Specs, func(o CheckSpec) bool { return o.ID == spec.ID }); i >= 0 {
		if digestJSON(u.Specs[i]) != digestJSON(spec) {
			refuse("%s is already accepted with another definition; an accepted check is never changed or withdrawn", spec.ID)
		}
		return // the same check again: nothing new
	}
	if slices.ContainsFunc(v.specs, func(o CheckSpec) bool { return o.ID == spec.ID }) {
		v.bad("check %s is proposed twice", spec.ID)
		return
	}
	v.specs = append(v.specs, spec)
}

// specID scopes a check to its unit and to the role that proposed it, so one
// role's proposal never names, and never replaces, the other's check.
func specID(u *StepView, source, id string) string {
	if source == string(contract.RoleReviewer) {
		return u.ID + "/review/" + id
	}
	return u.ID + "/" + id
}
