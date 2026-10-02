package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/attempt"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/holders"
	"github.com/killabayte/niten/internal/plan"
	"github.com/killabayte/niten/internal/store"
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
	// The final stage makes no commit: the metadata must be the journal's.
	if rec.Metadata != e.st.Metadata {
		return fmt.Errorf("%w: the git metadata changed outside the coordinator before the final stage", ErrIntegrity)
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
	// Certificates are the live profile certificates the run's sessions ran under.
	Certificates []certRef `json:"certificates"`
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
	for _, u := range append(slices.Clone(e.st.Steps), f) {
		gate(u.PendingDiscard == nil, "%s has an unfinished discard %v", u.ID, u.PendingDiscard)
		if e.clone == nil {
			continue
		}
		for _, snap := range u.Rejected {
			ref, commit, _ := strings.Cut(snap, "@")
			cur, err := e.clone.Rejected(ctx, strings.TrimPrefix(ref, "refs/niten/rejected/"))
			gate(err == nil && cur == commit, "the rejected snapshot %s is not kept at %s", ref, commit)
		}
	}
	if e.clone != nil {
		h, err := e.clone.Head(ctx)
		gate(err == nil && h == head, "the clone's branch is not at the head candidate")
		tree, err := e.clone.TreeOf(ctx, head)
		gate(err == nil && f.Candidate != nil && tree == f.Candidate.Tree, "the head tree differs from the checked one")
		ins, err := e.clone.Inspect(ctx, e.rules(f))
		gate(err == nil && len(ins.Changes) == 0 && len(ins.Violations) == 0, "the worktree differs from the head candidate")
	}
	for _, c := range sessionCertificates(e.events) {
		if c.Skipped {
			continue
		}
		raw, err := os.ReadFile(c.Path)
		gate(err == nil && digest(raw) == c.SHA256, "the live certificate %s changed or is missing", c.Path)
	}
	pids, err := holders.List([]string{e.work})
	gate(err == nil && len(pids) == 0, "processes still hold the run's work area: %v %v", pids, err)
	covered := e.finalCoverage(e.st)
	crit, pending := e.criteriaStatus(e.st, covered)
	for _, c := range crit {
		gate(c.Status != "uncovered", "criterion %s has no evidence", c.ID)
	}
	_, err = e.verifyArtifacts(e.events)
	gate(err == nil, "an artifact does not match its digest: %v", err)
	if len(failed) > 0 {
		return e.stop(contract.RunPaused, "final_gate", failed, contract.ExitRejected)
	}
	status := contract.RunDone
	if len(pending) > 0 {
		status = contract.RunImplemented
	}
	if err := e.saveReceipt(status); err != nil {
		return nil, err
	}
	if status == contract.RunImplemented {
		return e.stop(status, "pending_external", pending, contract.ExitImplemented)
	}
	return e.stop(status, "", nil, contract.ExitOK)
}

// finalCoverage is the criteria the approving final review checked.
func (e *Engine) finalCoverage(s *State) map[string]bool {
	covered := map[string]bool{}
	if f := s.Final.Review; f != nil {
		var rr contract.ReviewResult
		if err := e.message(fmt.Sprintf("m-%s-01", f.Turn), &rr); err == nil {
			for _, c := range rr.Coverage.CriterionIDsChecked {
				covered[c] = true
			}
		}
	}
	return covered
}

// decisiveSeq is the last event the receipt depends on: the final acceptance,
// or a later attestation. Events after it (a new session after a crash, a limit
// raise) do not change the receipt, so a receipt rebuilt after a crash in the
// middle of saving it has the same bytes.
func (e *Engine) decisiveSeq() int64 {
	seq := e.st.Final.AcceptedSeq
	for _, a := range e.st.Attestations {
		if a.Seq > seq {
			seq = a.Seq
		}
	}
	return seq
}

// criteriaStatus decides every criterion: coordinator-owned ones are covered by
// the approving final review and the passed final checks; human-owned ones, and
// criteria traced to a human-owned verification, need a passed attestation.
func (e *Engine) criteriaStatus(s *State, covered map[string]bool) ([]receiptCrit, []string) {
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
	for _, a := range s.Attestations {
		if a.Candidate == s.Head && a.Result == string(contract.AttestationPassed) {
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
			for _, a := range s.Attestations {
				if a.Criterion == c.ID && a.Candidate == s.Head {
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
			if s.Final.Review != nil {
				rc.Evidence = append(rc.Evidence, "turn:"+s.Final.Review.Turn)
			}
			if ch := s.Final.Checks; ch != nil {
				for _, r := range ch.Results {
					spec := specByID(s, r.ID)
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

func specByID(s *State, id string) *CheckSpec {
	for _, u := range append(slices.Clone(s.Steps), s.Final) {
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

// verifyArtifacts re-reads every artifact the events reference with its
// recorded digest: candidate records, messages, check evidence (every recorded
// check) and the streams each evidence record binds, attempt prompts (which
// must equal their turn's packet), streams, outcomes and results of finished
// and recovered attempts, collected reviewer evidence, answers, attestations,
// receipts, the configuration and the stored inputs. A reference without a
// digest is an error, never skipped.
func (e *Engine) verifyArtifacts(events []store.Event) ([]artifactRef, error) {
	seen := map[string]bool{}
	var out []artifactRef
	var unbound []string
	add := func(ref, sha string) {
		switch {
		case ref == "" && sha == "":
		case ref == "" || sha == "":
			unbound = append(unbound, fmt.Sprintf("ref %q with digest %q", ref, sha))
		case !seen[ref]:
			seen[ref] = true
			out = append(out, artifactRef{Ref: ref, SHA: sha})
		}
	}
	var evidence []CheckOutcome
	packets := map[string]string{}
	for _, ev := range events {
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
			for _, c := range d.Collected {
				add(c.Ref, c.SHA)
			}
			if d.Checks != nil {
				evidence = append(evidence, d.Checks.Results...)
			}
		case evTurn:
			var d TurnView
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, err
			}
			packets[d.ID] = d.PacketSHA
		case attempt.EvIntent:
			var d struct {
				ID        string `json:"id"`
				PromptRef string `json:"prompt_ref"`
				PromptSHA string `json:"prompt_sha256"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, err
			}
			if want, ok := packets[d.ID]; ok && want != d.PromptSHA {
				return nil, fmt.Errorf("attempt %s sent a prompt with digest %s, its turn recorded %s", d.ID, d.PromptSHA, want)
			}
			add(d.PromptRef, d.PromptSHA)
		case attempt.EvRecovered:
			var d struct {
				ResultRef string `json:"result_ref"`
				ResultSHA string `json:"result_sha256"`
				StdoutRef string `json:"stdout_ref"`
				StdoutSHA string `json:"stdout_sha256"`
				StderrRef string `json:"stderr_ref"`
				StderrSHA string `json:"stderr_sha256"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, err
			}
			if d.ResultRef == "" || d.ResultSHA == "" {
				return nil, fmt.Errorf("event %d records a recovered attempt without its result digest", ev.Seq)
			}
			add(d.ResultRef, d.ResultSHA)
			add(d.StdoutRef, d.StdoutSHA)
			add(d.StderrRef, d.StderrSHA)
		case "check.recorded":
			var d struct {
				CheckID string `json:"check_id"`
				Ref     string `json:"evidence_ref"`
				SHA     string `json:"evidence_sha256"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, err
			}
			evidence = append(evidence, CheckOutcome{ID: d.CheckID, EvidenceRef: d.Ref, EvidenceSHA: d.SHA})
		case attempt.EvFinished:
			var d struct {
				OutcomeRef string `json:"outcome_ref"`
				OutcomeSHA string `json:"outcome_sha256"`
				ResultRef  string `json:"result_ref"`
				ResultSHA  string `json:"result_sha256"`
				StdoutRef  string `json:"stdout_ref"`
				StdoutSHA  string `json:"stdout_sha256"`
				StderrRef  string `json:"stderr_ref"`
				StderrSHA  string `json:"stderr_sha256"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, err
			}
			add(d.OutcomeRef, d.OutcomeSHA)
			add(d.ResultRef, d.ResultSHA)
			add(d.StdoutRef, d.StdoutSHA)
			add(d.StderrRef, d.StderrSHA)
		case evAttestation:
			var d AttestationView
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, err
			}
			add(d.Ref, d.SHA)
		case evGateReleased:
			var d gateReleasedData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, err
			}
			add(d.AnswerRef, d.AnswerSHA)
		case evAnswer:
			var d answerData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, err
			}
			add(d.Ref, d.SHA)
		case evReceipt:
			var d ReceiptView
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return nil, err
			}
			add(d.Ref, d.SHA)
			add(d.MDRef, d.MDSHA)
		}
	}
	for _, r := range evidence {
		if seen[r.EvidenceRef] {
			continue
		}
		if r.EvidenceRef == "" || r.EvidenceSHA == "" {
			return nil, fmt.Errorf("check %s is recorded without its evidence digest", r.ID)
		}
		add(r.EvidenceRef, r.EvidenceSHA)
		_, ev, err := e.evidence(r)
		if err != nil {
			return nil, err
		}
		if ev.StdoutSHA256 == "" || ev.StderrSHA256 == "" {
			return nil, fmt.Errorf("%s records no digest of its streams", r.EvidenceRef)
		}
		add(ev.StdoutRef, ev.StdoutSHA256)
		add(ev.StderrRef, ev.StderrSHA256)
	}
	add(e.c.Inputs.Plan.Stored, e.c.Inputs.Plan.SHA256)
	add(e.c.Inputs.Receipt.Stored, e.c.Inputs.Receipt.SHA256)
	add(e.c.Inputs.Manifest.Stored, e.c.Inputs.Manifest.SHA256)
	add("contract.json", e.contractSHA)
	add("config.json", firstSession(events).ConfigSHA256)
	for _, in := range e.c.Inputs.Execution {
		add(in.Stored, in.SHA256)
	}
	for _, in := range e.c.Repos[0].Instructions {
		add(in.Stored, in.SHA256)
	}
	if cfg := firstSession(events).ConfigSHA256; cfg == "" {
		unbound = append(unbound, "config.json has no digest in the first session")
	}
	if len(unbound) > 0 {
		sort.Strings(unbound)
		return nil, fmt.Errorf("the journal references artifacts without a digest: %s", strings.Join(unbound, "; "))
	}
	for _, a := range out {
		if _, err := e.run.ReadArtifact(a.Ref, a.SHA); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// saveReceipt writes the receipt as a new immutable version, makes it the
// current execution.json and execution.md, and records it. The receipt is
// built from the journal folded up to the decisive event, never from the clock
// or from later events, so rebuilding it after a crash gives the same bytes.
func (e *Engine) saveReceipt(status contract.RunState) error {
	snap, events, err := e.snapshot(e.decisiveSeq())
	if err != nil {
		return err
	}
	crit, pending := e.criteriaStatus(snap, e.finalCoverage(snap))
	arts, err := e.verifyArtifacts(events)
	if err != nil {
		return err
	}
	f := snap.Final
	repo := e.c.Repos[0]
	r := Receipt{SchemaVersion: 1, Kind: ReceiptKind, Version: len(snap.Receipts) + 1, Status: status, RunID: snap.RunID,
		CreatedAt: events[len(events)-1].Time, Plan: e.c.Plan, PlanDigest: e.c.PlanDigest, ContractDigest: e.contractSHA,
		SemanticsDigest: e.c.SemanticsDigest, Repository: receiptRepo{ID: repo.ID, Path: repo.Path, BaseCommit: repo.BaseCommit, BaseTree: repo.BaseTree},
		Final:    receiptFinal{Commit: f.Candidate.Commit, Tree: f.Candidate.Tree, Branch: "niten", Clone: snap.CloneWork, GitDir: snap.CloneGitDir},
		Criteria: crit, Checks: f.Checks.Results, FinalReview: *f.Review, Findings: snap.Findings, PendingExternal: nonNil(pending),
		Attestations: snap.Attestations, Turns: snap.Turns, Artifacts: arts, Certificates: sessionCertificates(events),
		Limits: receiptLimits{Effective: snap.Limits, History: snap.LimitHistory, Invocations: snap.Invocations, ActiveMS: snap.ActiveMS}}
	for _, s := range snap.Steps {
		rs := receiptStep{ID: s.ID, AcceptedAt: s.AcceptedAt, Repairs: s.Repairs, Reviews: s.Reviews}
		if s.Review != nil {
			rs.Review, rs.Evidence = s.Review.Turn, s.Review.Evidence
		}
		r.Steps = append(r.Steps, rs)
	}
	var rr contract.ReviewResult
	if err := e.message(fmt.Sprintf("m-%s-01", f.Review.Turn), &rr); err == nil {
		for _, d := range rr.Coverage.OffTargetDispositions {
			r.OffTarget = append(r.OffTarget, receiptOff{Path: d.Path, Reason: justification(snap, d.Path), Disposition: string(d.Disposition), Review: f.Review.Turn})
		}
	}
	r.OffTarget = nonNil(r.OffTarget)
	r.Models.Executor = modelFacts{Requested: e.exec.Model, Effort: e.exec.Effort, EffortReported: "unknown", Reported: []string{}}
	r.Models.Reviewer = modelFacts{Requested: e.rev.Model, Effort: e.rev.Effort, EffortReported: e.rev.Effort, Reported: []string{}}
	for _, t := range snap.Turns {
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
	mdSHA, err := e.writeOrReuse(base+".md", md)
	if err != nil {
		return err
	}
	rv := ReceiptView{Status: string(status), Ref: base + ".json", SHA: sha, MDRef: base + ".md", MDSHA: mdSHA, Candidate: r.Final.Commit}
	// A crash after receipt.saved but before the run state: the same receipt
	// is already recorded and is not recorded twice.
	if !slices.Contains(e.st.Receipts, rv) {
		if err := e.emit(evReceipt, rv); err != nil {
			return err
		}
	}
	if _, err := e.run.Replace("execution.json", b); err != nil {
		return err
	}
	_, err = e.run.Replace("execution.md", md)
	return err
}

// sessionCertificates lists the distinct certificates the sessions recorded.
func sessionCertificates(events []store.Event) []certRef {
	out := []certRef{}
	for _, ev := range events {
		if ev.Type != evSession {
			continue
		}
		var d sessionData
		if json.Unmarshal(ev.Data, &d) != nil || d.Certificate == nil {
			continue
		}
		if !slices.Contains(out, *d.Certificate) {
			out = append(out, *d.Certificate)
		}
	}
	return out
}

func justification(s *State, path string) string {
	for _, u := range s.Steps {
		if r, ok := u.Justifications[path]; ok {
			return r
		}
	}
	if r, ok := s.Final.Justifications[path]; ok {
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

// completeExternal finishes an implemented run after new attestations,
// without any model call: once nothing is pending, the whole final gate runs
// again on the unchanged candidate and a new receipt version records done.
func (e *Engine) completeExternal(ctx context.Context) (Outcome, error) {
	for _, a := range e.st.Attestations {
		if a.Candidate == e.st.Head && a.Result == string(contract.AttestationFailed) {
			return Outcome{State: e.st.State, Reason: "attestation_failed", Detail: []string{fmt.Sprintf("%s was attested as failed; a new code change needs a new revision", a.Criterion)}, Exit: contract.ExitRejected}, nil
		}
	}
	if _, pending := e.criteriaStatus(e.st, map[string]bool{}); len(pending) > 0 {
		return Outcome{State: e.st.State, Reason: "pending_external", Detail: pending, Exit: contract.ExitImplemented}, nil
	}
	out, err := e.finish(ctx)
	if err != nil {
		return e.failure(err)
	}
	return *out, nil
}
