package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/attempt"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/holders"
	"github.com/killabayte/niten/internal/plan"
)

// ReceiptKind identifies execution.json.
const ReceiptKind = "niten.execution_receipt"

// startFinal opens the final stage on the last accepted candidate: the whole
// change from the base, every criterion, every check.
func (e *Engine) startFinal(ctx context.Context) error {
	head := e.st.Head
	tree, err := e.clone.TreeOf(ctx, head)
	if err != nil {
		return err
	}
	f := e.st.Final
	cmp, err := e.clone.Compare(ctx, e.st.Base, head, e.rules(f))
	if err != nil {
		return err
	}
	just := map[string]string{}
	for _, s := range e.st.Steps {
		for p, r := range s.Justifications {
			just[p] = r
		}
	}
	var last *TurnView
	for _, t := range e.st.Turns {
		if t.Role == string(contract.RoleExecutor) && t.Outcome == OutcomeApplied {
			last = t
		}
	}
	if last == nil {
		return fmt.Errorf("the final stage has no executor turn to date its record")
	}
	cid := contract.CandidateID{Commit: head, Tree: tree, PlanDigest: e.c.PlanDigest, Generation: 1}
	rec, err := e.candidateRecordAt(f, cid, cmp, just, "candidates/final-"+short(head)+".json", turnTime(last))
	if err != nil {
		return err
	}
	e.logf("final: the whole change %s..%s", short(e.st.Base), short(head))
	return e.emit(evFinalStarted, transition{Unit: FinalUnit, Candidate: rec, Step: &stepStateData{State: contract.StepCandidate, Start: e.st.Base, Problems: []string{}}})
}

// Receipt is execution.json: the evidence of one local execution, not a
// cryptographic attestation of the environment.
type Receipt struct {
	SchemaVersion   int               `json:"schema_version"`
	Kind            string            `json:"kind"`
	Version         int               `json:"version"`
	Status          contract.RunState `json:"status"`
	RunID           string            `json:"run_id"`
	CreatedAt       string            `json:"created_at"`
	Plan            plan.Identity     `json:"plan"`
	PlanDigest      string            `json:"plan_digest"`
	ContractDigest  string            `json:"contract_digest"`
	SemanticsDigest string            `json:"semantics_digest"`
	Repository      receiptRepo       `json:"repository"`
	Final           receiptFinal      `json:"final"`
	Steps           []receiptStep     `json:"steps"`
	Criteria        []receiptCrit     `json:"criteria"`
	Checks          []CheckOutcome    `json:"checks"`
	FinalReview     ReviewView        `json:"final_review"`
	Findings        []*FindingView    `json:"findings"`
	OffTarget       []receiptOff      `json:"off_target"`
	PendingExternal []string          `json:"pending_external"`
	Attestations    []AttestationView `json:"attestations"`
	Limits          receiptLimits     `json:"limits"`
	Turns           []*TurnView       `json:"turns"`
	Models          receiptModels     `json:"models"`
	Artifacts       []artifactRef     `json:"artifacts"`
}

type receiptRepo struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	BaseCommit string `json:"base_commit"`
	BaseTree   string `json:"base_tree"`
}

type receiptFinal struct {
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
	Branch string `json:"branch"`
	Clone  string `json:"clone"`
	GitDir string `json:"gitdir"`
}

type receiptStep struct {
	ID         string `json:"id"`
	AcceptedAt string `json:"accepted_at"`
	Review     string `json:"review_turn"`
	Evidence   string `json:"evidence_digest"`
	Repairs    int    `json:"repairs"`
	Reviews    int    `json:"reviews"`
}

type receiptCrit struct {
	ID        string   `json:"id"`
	Owner     string   `json:"owner"`
	Mandatory bool     `json:"mandatory"`
	Status    string   `json:"status"` // covered, attested, pending_external, not_mandatory
	Evidence  []string `json:"evidence"`
}

type receiptOff struct {
	Path        string `json:"path"`
	Reason      string `json:"reason_claimed"`
	Disposition string `json:"disposition"`
	Review      string `json:"review_turn"`
}

type receiptLimits struct {
	Effective   Limits        `json:"effective"`
	History     []LimitChange `json:"history"`
	Invocations int           `json:"invocations_used"`
	ActiveMS    int64         `json:"active_ms"`
}

type receiptModels struct {
	Executor modelFacts `json:"executor"`
	Reviewer modelFacts `json:"reviewer"`
}

type modelFacts struct {
	Requested string   `json:"requested"`
	Effort    string   `json:"effort_requested"`
	Reported  []string `json:"reported"`
	// EffortReported is unknown for Claude, which does not report effort.
	EffortReported string `json:"effort_reported"`
}

type artifactRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha256"`
}

// finish is the final gate. done (or implemented) is recorded only after the
// receipt is saved and every condition holds.
func (e *Engine) finish(ctx context.Context) (*Outcome, error) {
	var failed []string
	gate := func(ok bool, format string, a ...any) {
		if !ok {
			failed = append(failed, fmt.Sprintf(format, a...))
		}
	}
	cb, err := e.run.ReadArtifact("contract.json", e.contractSHA)
	gate(err == nil && len(cb) > 0, "the execution contract changed since preparation")
	gate(e.verifyInputs() == nil, "the stored plan inputs changed since preparation")
	f := e.st.Final
	head := e.st.Head
	gate(f.Candidate != nil && f.Candidate.Commit == head, "the final stage is not on the head candidate")
	gate(f.Checks != nil && f.Checks.Candidate == head && f.Checks.Passed, "the final checks did not pass on the head candidate")
	gate(f.Review != nil && f.Review.Accepts && f.Review.Candidate == head && f.Checks != nil && f.Review.Evidence == f.Checks.Digest,
		"the final review did not approve exactly the head candidate and its evidence")
	for _, s := range e.st.Steps {
		gate(s.State == contract.StepAccepted, "step %s is not accepted", s.ID)
	}
	for _, x := range e.st.Findings {
		gate(!(x.State == contract.FindingOpen && (x.Severity == contract.SeverityBlocker || x.Severity == contract.SeverityMajor)),
			"%s finding %s is open", x.Severity, x.FindingID)
	}
	for _, t := range e.st.Turns {
		gate(t.Outcome != "", "turn %s was never processed", t.ID)
	}
	if e.clone != nil {
		h, err := e.clone.Head(ctx)
		gate(err == nil && h == head, "the clone's branch is not at the head candidate")
		tree, err := e.clone.TreeOf(ctx, head)
		gate(err == nil && f.Candidate != nil && tree == f.Candidate.Tree, "the head tree differs from the checked one")
		ins, err := e.clone.Inspect(ctx, e.rules(f))
		gate(err == nil && len(ins.Changes) == 0 && len(ins.Violations) == 0, "the worktree differs from the head candidate")
	}
	pids, err := holders.List([]string{e.work})
	gate(err == nil && len(pids) == 0, "processes still hold the run's work area: %v %v", pids, err)
	covered := map[string]bool{}
	if f.Review != nil {
		var rr contract.ReviewResult
		if err := e.message(fmt.Sprintf("m-%s-01", f.Review.Turn), &rr); err == nil {
			for _, c := range rr.Coverage.CriterionIDsChecked {
				covered[c] = true
			}
		}
	}
	crit, pending := e.criteriaStatus(covered)
	for _, c := range crit {
		gate(c.Status != "uncovered", "criterion %s has no evidence", c.ID)
	}
	arts, err := e.verifyArtifacts()
	gate(err == nil, "an artifact does not match its digest: %v", err)
	if len(failed) > 0 {
		return e.stop(contract.RunPaused, "final_gate", failed, contract.ExitRejected)
	}
	status := contract.RunDone
	if len(pending) > 0 {
		status = contract.RunImplemented
	}
	if err := e.saveReceipt(status, crit, pending, arts); err != nil {
		return nil, err
	}
	if status == contract.RunImplemented {
		return e.stop(status, "pending_external", pending, contract.ExitImplemented)
	}
	return e.stop(status, "", nil, contract.ExitOK)
}

// criteriaStatus decides every criterion: coordinator-owned ones are covered by
// the approving final review and the passed final checks; human-owned ones, and
// criteria traced to a human-owned verification, need a passed attestation.
func (e *Engine) criteriaStatus(covered map[string]bool) ([]receiptCrit, []string) {
	humanVerif := map[string]bool{}
	for _, c := range e.c.Checks {
		if c.Owner == string(contract.OwnerHuman) {
			humanVerif[c.ScopedID] = true
		}
	}
	external := map[string]bool{}
	for _, r := range e.doc.Traceability {
		for _, v := range r.Verifications {
			if humanVerif[v] {
				external[r.CriterionID] = true
			}
		}
	}
	attested := map[string]bool{}
	for _, a := range e.st.Attestations {
		if a.Candidate == e.st.Head && a.Result == string(contract.AttestationPassed) {
			attested[a.Criterion] = true
		}
	}
	var out []receiptCrit
	var pending []string
	for _, c := range e.c.Criteria {
		rc := receiptCrit{ID: c.ID, Owner: c.Owner, Mandatory: c.Mandatory, Evidence: []string{}}
		needsHuman := c.Owner == string(contract.OwnerHuman) || external[c.ID]
		switch {
		case needsHuman && attested[c.ID]:
			rc.Status = "attested"
			for _, a := range e.st.Attestations {
				if a.Criterion == c.ID && a.Candidate == e.st.Head {
					rc.Evidence = append(rc.Evidence, a.Ref)
				}
			}
		case needsHuman && c.Mandatory:
			rc.Status = "pending_external"
			pending = append(pending, c.ID)
		case !c.Mandatory && !covered[c.ID]:
			rc.Status = "not_mandatory"
		case covered[c.ID]:
			rc.Status = "covered"
			if e.st.Final.Review != nil {
				rc.Evidence = append(rc.Evidence, "turn:"+e.st.Final.Review.Turn)
			}
			if ch := e.st.Final.Checks; ch != nil {
				for _, r := range ch.Results {
					spec := e.specByID(r.ID)
					if spec != nil && (slices.Contains(spec.CriterionIDs, c.ID) || e.tracesTo(spec, c.ID)) {
						rc.Evidence = append(rc.Evidence, r.EvidenceRef)
					}
				}
			}
		default:
			rc.Status = "uncovered"
		}
		out = append(out, rc)
	}
	return out, pending
}

func (e *Engine) specByID(id string) *CheckSpec {
	for _, u := range append(slices.Clone(e.st.Steps), e.st.Final) {
		for i := range u.Specs {
			if u.Specs[i].ID == id {
				return &u.Specs[i]
			}
		}
	}
	return nil
}

// tracesTo reports whether a check proves a verification the plan traces to the criterion.
func (e *Engine) tracesTo(spec *CheckSpec, criterion string) bool {
	for _, r := range e.doc.Traceability {
		if r.CriterionID != criterion {
			continue
		}
		for _, v := range r.Verifications {
			if slices.Contains(spec.VerificationIDs, v) {
				return true
			}
		}
	}
	return false
}

// verifyArtifacts re-reads every artifact the journal references with its digest.
func (e *Engine) verifyArtifacts() ([]artifactRef, error) {
	seen := map[string]bool{}
	var out []artifactRef
	add := func(ref, sha string) {
		if ref != "" && sha != "" && !seen[ref] {
			seen[ref] = true
			out = append(out, artifactRef{Ref: ref, SHA: sha})
		}
	}
	for _, ev := range e.events {
		switch ev.Type {
		case evProcessed, evChecks, evFinalStarted, evResidue, evStepState:
			var d processedData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, err
			}
			if d.Candidate != nil {
				add(d.Candidate.Ref, d.Candidate.SHA)
			}
			for _, m := range d.Messages {
				add(m.Ref, m.SHA)
			}
			if d.Checks != nil {
				for _, r := range d.Checks.Results {
					add(r.EvidenceRef, r.EvidenceSHA)
				}
			}
		case attempt.EvFinished:
			var d struct {
				OutcomeRef string `json:"outcome_ref"`
				OutcomeSHA string `json:"outcome_sha256"`
				ResultRef  string `json:"result_ref"`
				ResultSHA  string `json:"result_sha256"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, err
			}
			add(d.OutcomeRef, d.OutcomeSHA)
			add(d.ResultRef, d.ResultSHA)
		case evAttestation:
			var d AttestationView
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, err
			}
			add(d.Ref, d.SHA)
		}
	}
	add(e.c.Inputs.Plan.Stored, e.c.Inputs.Plan.SHA256)
	add(e.c.Inputs.Receipt.Stored, e.c.Inputs.Receipt.SHA256)
	add(e.c.Inputs.Manifest.Stored, e.c.Inputs.Manifest.SHA256)
	add("contract.json", e.contractSHA)
	for _, a := range out {
		if _, err := e.run.ReadArtifact(a.Ref, a.SHA); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// saveReceipt writes the receipt as a new immutable version, makes it the
// current execution.json and execution.md, and records it.
func (e *Engine) saveReceipt(status contract.RunState, crit []receiptCrit, pending []string, arts []artifactRef) error {
	f := e.st.Final
	repo := e.c.Repos[0]
	r := Receipt{SchemaVersion: 1, Kind: ReceiptKind, Version: len(e.st.Receipts) + 1, Status: status, RunID: e.st.RunID,
		CreatedAt: e.o.Now().UTC().Format(time.RFC3339), Plan: e.c.Plan, PlanDigest: e.c.PlanDigest, ContractDigest: e.contractSHA,
		SemanticsDigest: e.c.SemanticsDigest, Repository: receiptRepo{ID: repo.ID, Path: repo.Path, BaseCommit: repo.BaseCommit, BaseTree: repo.BaseTree},
		Final:    receiptFinal{Commit: f.Candidate.Commit, Tree: f.Candidate.Tree, Branch: "niten", Clone: e.st.CloneWork, GitDir: e.st.CloneGitDir},
		Criteria: crit, Checks: f.Checks.Results, FinalReview: *f.Review, Findings: e.st.Findings, PendingExternal: nonNil(pending),
		Attestations: e.st.Attestations, Turns: e.st.Turns, Artifacts: arts,
		Limits: receiptLimits{Effective: e.st.Limits, History: e.st.LimitHistory, Invocations: e.st.Invocations, ActiveMS: e.st.ActiveMS}}
	for _, s := range e.st.Steps {
		rs := receiptStep{ID: s.ID, AcceptedAt: s.AcceptedAt, Repairs: s.Repairs, Reviews: s.Reviews}
		if s.Review != nil {
			rs.Review, rs.Evidence = s.Review.Turn, s.Review.Evidence
		}
		r.Steps = append(r.Steps, rs)
	}
	var rr contract.ReviewResult
	if err := e.message(fmt.Sprintf("m-%s-01", f.Review.Turn), &rr); err == nil {
		for _, d := range rr.Coverage.OffTargetDispositions {
			r.OffTarget = append(r.OffTarget, receiptOff{Path: d.Path, Reason: e.justification(d.Path), Disposition: string(d.Disposition), Review: f.Review.Turn})
		}
	}
	r.OffTarget = nonNil(r.OffTarget)
	r.Models.Executor = modelFacts{Requested: e.exec.Model, Effort: e.exec.Effort, EffortReported: "unknown", Reported: []string{}}
	r.Models.Reviewer = modelFacts{Requested: e.rev.Model, Effort: e.rev.Effort, EffortReported: e.rev.Effort, Reported: []string{}}
	for _, t := range e.st.Turns {
		if t.Reported == "" {
			continue
		}
		m := &r.Models.Reviewer
		if t.Role == string(contract.RoleExecutor) {
			m = &r.Models.Executor
		}
		if !slices.Contains(m.Reported, t.Reported) {
			m.Reported = append(m.Reported, t.Reported)
		}
	}
	b, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	md := []byte(renderReport(&r))
	base := fmt.Sprintf("receipts/%d-%s", r.Version, status)
	sha, err := e.writeOrReuse(base+".json", b)
	if err != nil {
		return err
	}
	if _, err := e.writeOrReuse(base+".md", md); err != nil {
		return err
	}
	if err := e.emit(evReceipt, ReceiptView{Status: string(status), Ref: base + ".json", SHA: sha, MDRef: base + ".md", Candidate: r.Final.Commit}); err != nil {
		return err
	}
	if _, err := e.run.Replace("execution.json", b); err != nil {
		return err
	}
	_, err = e.run.Replace("execution.md", md)
	return err
}

func (e *Engine) justification(path string) string {
	for _, s := range e.st.Steps {
		if r, ok := s.Justifications[path]; ok {
			return r
		}
	}
	if r, ok := e.st.Final.Justifications[path]; ok {
		return r
	}
	return ""
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

// renderReport is the human-readable execution.md.
func renderReport(r *Receipt) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("# Niten execution %s: %s\n\n", r.RunID, r.Status)
	w("Plan %s revision %d: %s\n\n", r.Plan.PlanID, r.Plan.Revision, r.Plan.Title)
	w("- Repository %s at %s, base %s\n- Final commit %s (tree %s) on branch %s of the run's clone\n- Plan digest %s, contract digest %s\n\n",
		r.Repository.ID, r.Repository.Path, r.Repository.BaseCommit, r.Final.Commit, r.Final.Tree, r.Final.Branch, r.PlanDigest, r.ContractDigest)
	w("## Steps\n\n| Step | Accepted at | Repairs | Reviews |\n|---|---|---|---|\n")
	for _, s := range r.Steps {
		w("| %s | %s | %d | %d |\n", s.ID, s.AcceptedAt, s.Repairs, s.Reviews)
	}
	w("\n## Criteria\n\n| Criterion | Owner | Status |\n|---|---|---|\n")
	for _, c := range r.Criteria {
		w("| %s | %s | %s |\n", c.ID, c.Owner, c.Status)
	}
	w("\n## Final checks\n\n")
	for _, c := range r.Checks {
		w("- %s: %s (%s)\n", c.ID, c.Status, c.EvidenceRef)
	}
	w("\n## Findings\n\n")
	if len(r.Findings) == 0 {
		w("None.\n")
	}
	for _, f := range r.Findings {
		w("- %s %s %s (%s): %s\n", f.FindingID, f.Severity, f.State, f.Location.Path, f.DefectScenario)
	}
	if len(r.OffTarget) > 0 {
		w("\n## Off-target edits\n\n")
		for _, o := range r.OffTarget {
			w("- %s: %s (claimed: %q)\n", o.Path, o.Disposition, o.Reason)
		}
	}
	if len(r.PendingExternal) > 0 {
		w("\n## Pending external\n\n%s\n", strings.Join(r.PendingExternal, ", "))
	}
	w("\n## Limits\n\n- Invocations used %d of %d\n- Active time %s of %s\n", r.Limits.Invocations, r.Limits.Effective.MaxInvocations,
		(time.Duration(r.Limits.ActiveMS) * time.Millisecond).Round(time.Second), r.Limits.Effective.MaxActiveTime)
	for _, h := range r.Limits.History {
		w("- %s raised from %s to %s\n", h.Field, h.Previous, h.New)
	}
	w("\n## Models\n\n- Executor requested %s (%s), reported %s, effort reported %s\n- Reviewer requested %s (%s), reported %s\n",
		r.Models.Executor.Requested, r.Models.Executor.Effort, strings.Join(r.Models.Executor.Reported, ", "), r.Models.Executor.EffortReported,
		r.Models.Reviewer.Requested, r.Models.Reviewer.Effort, strings.Join(r.Models.Reviewer.Reported, ", "))
	w("\nThis receipt is evidence of a local execution and of artifact integrity, not a cryptographic attestation of the environment.\n")
	return b.String()
}

// completeExternal re-runs the final gate of an implemented run after new
// attestations, without any model call.
func (e *Engine) completeExternal() (Outcome, error) {
	for _, a := range e.st.Attestations {
		if a.Candidate == e.st.Head && a.Result == string(contract.AttestationFailed) {
			return Outcome{State: e.st.State, Reason: "attestation_failed", Detail: []string{fmt.Sprintf("%s was attested as failed; a new code change needs a new revision", a.Criterion)}, Exit: contract.ExitRejected}, nil
		}
	}
	covered := map[string]bool{}
	var rr contract.ReviewResult
	if f := e.st.Final.Review; f != nil {
		if err := e.message(fmt.Sprintf("m-%s-01", f.Turn), &rr); err == nil {
			for _, c := range rr.Coverage.CriterionIDsChecked {
				covered[c] = true
			}
		}
	}
	crit, pending := e.criteriaStatus(covered)
	if len(pending) > 0 {
		return Outcome{State: e.st.State, Reason: "pending_external", Detail: pending, Exit: contract.ExitImplemented}, nil
	}
	arts, err := e.verifyArtifacts()
	if err != nil {
		return e.failure(err)
	}
	if err := e.saveReceipt(contract.RunDone, crit, nil, arts); err != nil {
		return e.failure(err)
	}
	out, err := e.stop(contract.RunDone, "", nil, contract.ExitOK)
	if err != nil {
		return e.failure(err)
	}
	return *out, nil
}
