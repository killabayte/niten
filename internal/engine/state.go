package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/killabayte/niten/internal/attempt"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/store"
)

// Event types the engine writes. attempt.* and check.recorded are written by
// the attempt and verify packages; the reducer counts attempt.started as one
// model invocation and ignores the rest of them. Every state change of a unit
// travels in one event as a transition, so a crash never leaves half of one.
const (
	evSession      = "session.started"
	evClone        = "clone.created"
	evRunState     = "run.state"
	evStepState    = "step.state"
	evTurn         = "turn.started"
	evProcessed    = "turn.processed"
	evChecks       = "checks.evaluated"
	evFinalStarted = "final.started"
	evResidue      = "worktree.restored"
	evGateOpened   = "gate.opened"
	evGateReleased = "gate.released"
	evAnswer       = "answer.recorded"
	evAttestation  = "attestation.recorded"
	evLimits       = "limits.raised"
	evReceipt      = "receipt.saved"
)

// FinalUnit is the unit id of the final stage; it moves through the same
// states as a step.
const FinalUnit = "final"

// Turn kinds.
const (
	KindImplement   = "implement"
	KindRepair      = "repair"
	KindReview      = "review"
	KindFinalReview = "final_review"
	KindFinalRepair = "final_repair"
)

// Turn outcomes recorded by turn.processed.
const (
	OutcomeApplied    = "applied"     // the turn's result changed the state
	OutcomeRejected   = "rejected"    // the result was invalid or violated the policy; nothing of it was applied
	OutcomeFailed     = "failed"      // the attempt failed (transport, payload, rate limit, ...)
	OutcomeStale      = "stale"       // the result belongs to a candidate or state that is no longer current
	OutcomeNotStarted = "not_started" // the process provably never started
	OutcomeUnknown    = "unknown"     // what the attempt did cannot be established
)

// Limits are the effective limits: the contract's, raised only by user events.
type Limits struct {
	MaxInvocations          int    `json:"max_invocations"`
	MaxActiveTime           string `json:"max_active_time"`
	InvocationDeadline      string `json:"invocation_deadline"`
	MaxRepairsPerStep       int    `json:"max_repairs_per_step"`
	FinalReserveInvocations int    `json:"final_reserve_invocations"`
	FinalReserveTime        string `json:"final_reserve_time"`
}

// LimitChange is one recorded raise of a limit.
type LimitChange struct {
	Seq      int64  `json:"seq"`
	Field    string `json:"field"`
	Previous string `json:"previous"`
	New      string `json:"new"`
}

// CheckSpec is a validated, runnable check of a unit: a required check of the
// policy or an accepted proposal. ID is scoped ("S-001/go-test", "required/go-tests").
type CheckSpec struct {
	ID              string                 `json:"id"`
	Source          string                 `json:"source"` // required, executor or reviewer
	CommandID       string                 `json:"command_id"`
	Argv            []string               `json:"argv"`
	Cwd             string                 `json:"cwd"`
	Timeout         string                 `json:"timeout"`
	Expected        contract.CheckExpected `json:"expected"`
	CriterionIDs    []string               `json:"criterion_ids"`
	VerificationIDs []string               `json:"verification_ids"`
}

// CheckOutcome is one check of an evaluation.
type CheckOutcome struct {
	ID          string               `json:"id"`
	Status      contract.CheckStatus `json:"status"`
	EvidenceRef string               `json:"evidence_ref"`
	EvidenceSHA string               `json:"evidence_sha256"`
	Reasons     []string             `json:"reasons,omitempty"`
}

// ChecksView is the latest evaluation of a unit's candidate.
type ChecksView struct {
	Candidate string         `json:"candidate"`
	Digest    string         `json:"evidence_digest"`
	Passed    bool           `json:"passed"`
	Results   []CheckOutcome `json:"results"`
	Problems  []string       `json:"problems"`
}

// ReviewView is the latest applied review of a unit.
type ReviewView struct {
	Turn      string           `json:"turn"`
	Candidate string           `json:"candidate"`
	Evidence  string           `json:"evidence_digest"`
	Verdict   contract.Verdict `json:"verdict"`
	Accepts   bool             `json:"accepts"`
	Reasons   []string         `json:"reasons,omitempty"`
}

// StepView is the state of one step, or of the final stage (ID "final").
type StepView struct {
	ID             string                `json:"id"`
	State          contract.StepState    `json:"state"`
	Start          string                `json:"start,omitempty"`
	Candidate      *contract.CandidateID `json:"candidate,omitempty"`
	CandidateRef   string                `json:"candidate_ref,omitempty"`
	Repairs        int                   `json:"repairs"`
	Reviews        int                   `json:"reviews"`
	Checks         *ChecksView           `json:"checks,omitempty"`
	Review         *ReviewView           `json:"review,omitempty"`
	AcceptedAt     string                `json:"accepted_at,omitempty"`
	AcceptedSeq    int64                 `json:"accepted_seq,omitempty"`
	Pending        string                `json:"pending,omitempty"`
	Specs          []CheckSpec           `json:"check_specs"`
	Justifications map[string]string     `json:"justifications"`
	Problems       []string              `json:"problems"`
	LastExecutor   string                `json:"last_executor_turn,omitempty"`
	Rejected       []string              `json:"rejected_snapshots"`
}

// TurnView is one model invocation as the engine sees it.
type TurnView struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	Kind      string `json:"kind"`
	Unit      string `json:"unit"`
	Candidate string `json:"candidate,omitempty"` // the candidate a review is bound to
	Evidence  string `json:"evidence_digest,omitempty"`
	Base      string `json:"base,omitempty"` // the clone HEAD an executor turn started from
	PacketRef string `json:"packet_ref"`
	PacketSHA string `json:"packet_sha256"`
	Outcome   string `json:"outcome,omitempty"`
	StartedAt string `json:"started_at"`
	Reported  string `json:"reported_model,omitempty"`
}

// FindingView is one finding of the ledger.
type FindingView struct {
	contract.Finding
	Unit      string                `json:"unit"`
	State     contract.FindingState `json:"state"`
	OpenedBy  string                `json:"opened_by"`
	OpenedAt  string                `json:"opened_at_candidate"`
	UpdatedBy string                `json:"updated_by"`
	Responses []ResponseView        `json:"responses"`
}

// ResponseView is an executor response recorded against a finding.
type ResponseView struct {
	Message     string                       `json:"message"`
	Disposition contract.ResponseDisposition `json:"disposition"`
	Candidate   string                       `json:"candidate"`
}

// GateView is the open human step gate.
type GateView struct {
	ID        string `json:"gate_id"`
	Unit      string `json:"step_id"`
	Candidate string `json:"candidate_sha"`
}

// QuestionView is a blocking question a model asked.
type QuestionView struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	Text     string `json:"text"`
	Answered bool   `json:"answered"`
	Answer   string `json:"answer,omitempty"`
}

// AttestationView is a stored attestation.
type AttestationView struct {
	Criterion string `json:"criterion_id"`
	Result    string `json:"result"`
	Candidate string `json:"candidate_sha"`
	Ref       string `json:"ref"`
	SHA       string `json:"sha256"`
	Seq       int64  `json:"seq"`
}

// ReceiptView is one saved receipt version.
type ReceiptView struct {
	Status    string `json:"status"`
	Ref       string `json:"ref"`
	SHA       string `json:"sha256"`
	MDRef     string `json:"md_ref"`
	MDSHA     string `json:"md_sha256"`
	Candidate string `json:"candidate"`
}

// State is the projection of the journal. It is rebuilt by folding the events
// with apply, live and on recovery alike; state.json is a copy for readers.
type State struct {
	SchemaVersion  int               `json:"schema_version"`
	RunID          string            `json:"run_id"`
	State          contract.RunState `json:"state"`
	Reason         string            `json:"reason,omitempty"`
	Detail         []string          `json:"detail,omitempty"`
	ContractSHA256 string            `json:"contract_sha256"`
	PlanDigest     string            `json:"plan_digest"`
	Base           string            `json:"base"`
	Head           string            `json:"head"`
	Metadata       string            `json:"metadata_fingerprint"`
	CloneWork      string            `json:"clone_work,omitempty"`
	CloneGitDir    string            `json:"clone_gitdir,omitempty"`
	Steps          []*StepView       `json:"steps"`
	Final          *StepView         `json:"final"`
	Invocations    int               `json:"invocations"`
	ActiveMS       int64             `json:"active_ms"`
	Limits         Limits            `json:"limits"`
	LimitHistory   []LimitChange     `json:"limit_history"`
	Turns          []*TurnView       `json:"turns"`
	Messages       []string          `json:"messages"`
	Findings       []*FindingView    `json:"findings"`
	Gate           *GateView         `json:"gate,omitempty"`
	Released       []string          `json:"released_gates"`
	Questions      []*QuestionView   `json:"questions"`
	Attestations   []AttestationView `json:"attestations"`
	Receipts       []ReceiptView     `json:"receipts"`
	LastSeq        int64             `json:"last_seq"`
	UpdatedAt      string            `json:"updated_at"`
	sessionOpen    bool
	lastTime       time.Time
}

// newState is the projection right after prepare.
func newState(runID, contractSHA, planDigest, base string, order []string, lim Limits) *State {
	s := &State{SchemaVersion: 1, RunID: runID, State: contract.RunPrepared, ContractSHA256: contractSHA, PlanDigest: planDigest,
		Base: base, Head: base, Limits: lim, LimitHistory: []LimitChange{}, Turns: []*TurnView{}, Messages: []string{}, Findings: []*FindingView{},
		Released: []string{}, Questions: []*QuestionView{}, Attestations: []AttestationView{}, Receipts: []ReceiptView{}}
	for _, id := range order {
		s.Steps = append(s.Steps, newUnit(id))
	}
	s.Final = newUnit(FinalUnit)
	return s
}

func newUnit(id string) *StepView {
	return &StepView{ID: id, State: contract.StepPending, Specs: []CheckSpec{}, Justifications: map[string]string{}, Problems: []string{}, Rejected: []string{}}
}

// addSpecs adds newly accepted checks. An accepted check is never replaced or
// removed: a later proposal under the same id is refused before it gets here.
func (u *StepView) addSpecs(specs []CheckSpec) {
	for _, sp := range specs {
		if !slices.ContainsFunc(u.Specs, func(o CheckSpec) bool { return o.ID == sp.ID }) {
			u.Specs = append(u.Specs, sp)
		}
	}
}

// Unit returns the step with id, or the final stage.
func (s *State) Unit(id string) *StepView {
	if id == FinalUnit {
		return s.Final
	}
	for _, u := range s.Steps {
		if u.ID == id {
			return u
		}
	}
	return nil
}

// Turn returns the turn with id, or nil.
func (s *State) Turn(id string) *TurnView {
	for _, t := range s.Turns {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Finding returns the ledger entry with id, or nil.
func (s *State) Finding(id string) *FindingView {
	for _, f := range s.Findings {
		if f.FindingID == id {
			return f
		}
	}
	return nil
}

// stopping states end a session: no active time accrues while the run waits.
func stopping(st contract.RunState) bool {
	return st != contract.RunRunning && st != contract.RunPrepared
}

// Event payloads.
type (
	sessionData struct {
		Command        string `json:"command"`
		ContractSHA256 string `json:"contract_sha256"`
	}
	cloneData struct {
		Work     string `json:"work"`
		GitDir   string `json:"gitdir"`
		Head     string `json:"head"`
		Metadata string `json:"metadata_fingerprint"`
	}
	runStateData struct {
		State  contract.RunState `json:"state"`
		Reason string            `json:"reason,omitempty"`
		Detail []string          `json:"detail,omitempty"`
	}
	stepStateData struct {
		State    contract.StepState `json:"state"`
		Reason   string             `json:"reason,omitempty"`
		Start    string             `json:"start,omitempty"`
		Repairs  int                `json:"repairs"`
		Problems []string           `json:"problems"`
	}
	candidateData struct {
		Candidate contract.CandidateID `json:"candidate"`
		Ref       string               `json:"ref"`
		SHA       string               `json:"sha256"`
		Metadata  string               `json:"metadata_fingerprint"`
	}
	rejectedData struct {
		Snapshot   string   `json:"snapshot,omitempty"`
		Violations []string `json:"violations"`
		Metadata   string   `json:"metadata_fingerprint"`
	}
	messageData struct {
		ID   string               `json:"message_id"`
		Kind contract.MessageKind `json:"kind"`
		From contract.Role        `json:"from"`
		Ref  string               `json:"ref"`
		SHA  string               `json:"sha256"`
	}
	acceptedData struct {
		Candidate string `json:"candidate"`
		Review    string `json:"review_turn"`
		Evidence  string `json:"evidence_digest"`
	}
	// transition is one atomic change of a unit; every part is optional and
	// applied in field order.
	transition struct {
		Unit      string         `json:"unit"`
		Candidate *candidateData `json:"candidate,omitempty"`
		Rejected  *rejectedData  `json:"rejected,omitempty"`
		Messages  []messageData  `json:"messages,omitempty"`
		Findings  []FindingView  `json:"findings,omitempty"`
		Checks    *ChecksView    `json:"checks,omitempty"`
		Accepted  *acceptedData  `json:"accepted,omitempty"`
		Step      *stepStateData `json:"step,omitempty"`
	}
	processedData struct {
		Turn           string            `json:"turn"`
		Outcome        string            `json:"outcome"`
		Class          string            `json:"class,omitempty"`
		Reasons        []string          `json:"reasons,omitempty"`
		Reported       string            `json:"reported_model,omitempty"`
		Specs          []CheckSpec       `json:"check_specs,omitempty"`
		Justifications map[string]string `json:"justifications,omitempty"`
		Questions      []QuestionView    `json:"questions,omitempty"`
		Review         *ReviewView       `json:"review,omitempty"`
		Collected      []artifactRef     `json:"collected_evidence,omitempty"`
		transition
	}
	gateReleasedData struct {
		Gate      string `json:"gate_id"`
		AnswerRef string `json:"answer_ref"`
		AnswerSHA string `json:"answer_sha256"`
	}
	answerData struct {
		Question string `json:"question_id"`
		Text     string `json:"text"`
		Ref      string `json:"ref"`
		SHA      string `json:"sha256"`
	}
	limitsData struct {
		Changes []LimitChange `json:"changes"`
		Limits  Limits        `json:"limits"`
	}
)

// apply folds one event into the projection. An event the reducer cannot
// decode is corruption: the journal and the projection would disagree.
func (s *State) apply(ev store.Event) error {
	bad := func(err error) error {
		return fmt.Errorf("%w: event %d (%s): %v", store.ErrCorrupt, ev.Seq, ev.Type, err)
	}
	decode := func(v any) error {
		if err := json.Unmarshal(ev.Data, v); err != nil {
			return bad(err)
		}
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, ev.Time); err == nil {
		// A session start is a boundary: the time since the previous event,
		// a crashed session's downtime included, is never active time.
		if s.sessionOpen && ev.Type != evSession && t.After(s.lastTime) {
			s.ActiveMS += t.Sub(s.lastTime).Milliseconds()
		}
		s.lastTime = t
	}
	s.LastSeq, s.UpdatedAt = ev.Seq, ev.Time
	unit := func(id string) (*StepView, error) {
		u := s.Unit(id)
		if u == nil {
			return nil, bad(fmt.Errorf("unknown unit %q", id))
		}
		return u, nil
	}
	switch ev.Type {
	case evSession:
		s.sessionOpen = true
	case evClone:
		var d cloneData
		if err := decode(&d); err != nil {
			return err
		}
		s.CloneWork, s.CloneGitDir, s.Head, s.Metadata = d.Work, d.GitDir, d.Head, d.Metadata
	case evRunState:
		var d runStateData
		if err := decode(&d); err != nil {
			return err
		}
		s.State, s.Reason, s.Detail = d.State, d.Reason, d.Detail
		if stopping(d.State) {
			s.sessionOpen = false
		}
	case evLimits:
		var d limitsData
		if err := decode(&d); err != nil {
			return err
		}
		for _, c := range d.Changes {
			c.Seq = ev.Seq
			s.LimitHistory = append(s.LimitHistory, c)
		}
		s.Limits = d.Limits
	case evStepState, evChecks, evFinalStarted, evResidue:
		var d transition
		if err := decode(&d); err != nil {
			return err
		}
		if err := s.applyTransition(d, ev.Seq); err != nil {
			return bad(err)
		}
	case evTurn:
		var d TurnView
		if err := decode(&d); err != nil {
			return err
		}
		u, err := unit(d.Unit)
		if err != nil {
			return err
		}
		if s.Turn(d.ID) != nil {
			return bad(fmt.Errorf("turn %s started twice", d.ID))
		}
		d.StartedAt = ev.Time
		s.Turns = append(s.Turns, &d)
		u.Pending = d.ID
	case attempt.EvStarted:
		s.Invocations++
	case evProcessed:
		var d processedData
		if err := decode(&d); err != nil {
			return err
		}
		t := s.Turn(d.Turn)
		if t == nil {
			return bad(fmt.Errorf("turn %s was never started", d.Turn))
		}
		if t.Outcome != "" {
			return bad(fmt.Errorf("turn %s was processed twice", d.Turn))
		}
		t.Outcome, t.Reported = d.Outcome, d.Reported
		u, err := unit(t.Unit)
		if err != nil {
			return err
		}
		if u.Pending == t.ID {
			u.Pending = ""
		}
		if d.Outcome == OutcomeApplied {
			for _, q := range d.Questions {
				q := q
				s.Questions = append(s.Questions, &q)
			}
			if t.Role == string(contract.RoleExecutor) {
				u.LastExecutor = t.ID
				u.addSpecs(d.Specs)
				for p, r := range d.Justifications {
					u.Justifications[p] = r
				}
			} else if d.Review != nil {
				u.Reviews++
				r := *d.Review
				u.Review = &r
				u.addSpecs(d.Specs)
			}
		}
		d.transition.Unit = t.Unit
		if err := s.applyTransition(d.transition, ev.Seq); err != nil {
			return bad(err)
		}
	case evGateOpened:
		var d GateView
		if err := decode(&d); err != nil {
			return err
		}
		s.Gate = &d
	case evGateReleased:
		var d gateReleasedData
		if err := decode(&d); err != nil {
			return err
		}
		if s.Gate == nil || s.Gate.ID != d.Gate {
			return bad(fmt.Errorf("gate %s is not open", d.Gate))
		}
		s.Released = append(s.Released, d.Gate)
		s.Gate = nil
	case evAnswer:
		var d answerData
		if err := decode(&d); err != nil {
			return err
		}
		for _, q := range s.Questions {
			if q.ID == d.Question {
				q.Answered, q.Answer = true, d.Text
			}
		}
	case evAttestation:
		var d AttestationView
		if err := decode(&d); err != nil {
			return err
		}
		d.Seq = ev.Seq
		s.Attestations = append(s.Attestations, d)
	case evReceipt:
		var d ReceiptView
		if err := decode(&d); err != nil {
			return err
		}
		s.Receipts = append(s.Receipts, d)
	}
	return nil
}

// applyTransition applies one atomic unit change.
func (s *State) applyTransition(d transition, seq int64) error {
	u := s.Unit(d.Unit)
	if u == nil {
		return fmt.Errorf("unknown unit %q", d.Unit)
	}
	if c := d.Candidate; c != nil {
		cand := c.Candidate
		u.Candidate, u.CandidateRef, u.Checks, u.Review = &cand, c.Ref, nil, nil
		s.Head, s.Metadata = cand.Commit, c.Metadata
	}
	if r := d.Rejected; r != nil {
		if r.Snapshot != "" {
			u.Rejected = append(u.Rejected, r.Snapshot)
		}
		s.Metadata = r.Metadata
	}
	for _, m := range d.Messages {
		s.Messages = append(s.Messages, m.ID)
	}
	for _, f := range d.Findings {
		f := f
		if old := s.Finding(f.FindingID); old != nil {
			*old = f
		} else {
			s.Findings = append(s.Findings, &f)
		}
	}
	if d.Checks != nil {
		c := *d.Checks
		u.Checks = &c
	}
	if a := d.Accepted; a != nil {
		u.State, u.AcceptedAt, u.AcceptedSeq = contract.StepAccepted, a.Candidate, seq
	}
	if st := d.Step; st != nil {
		u.State, u.Repairs = st.State, st.Repairs
		if st.Start != "" {
			u.Start = st.Start
		}
		if st.Problems != nil {
			u.Problems = st.Problems
		}
	}
	return nil
}

// openQuestions lists the blocking questions without an answer.
func (s *State) openQuestions() []*QuestionView {
	var out []*QuestionView
	for _, q := range s.Questions {
		if !q.Answered {
			out = append(out, q)
		}
	}
	return out
}
