package contract

import "encoding/json"

// CandidateID pins a candidate: full commit and tree SHA, the plan digest and
// the run generation.
type CandidateID struct {
	Commit     string `json:"commit"`
	Tree       string `json:"tree"`
	PlanDigest string `json:"plan_digest"`
	Generation int    `json:"generation"`
}

// Envelope wraps one message in the durable queue. Every field except Payload is
// set by the coordinator; a model cannot choose its role, attempt or snapshot.
type Envelope struct {
	SchemaVersion int             `json:"schema_version"`
	RunID         string          `json:"run_id"`
	Sequence      int             `json:"sequence"`
	MessageID     string          `json:"message_id"`
	ReplyTo       *string         `json:"reply_to"`
	AttemptID     string          `json:"attempt_id"`
	StepIDs       []string        `json:"step_ids"`
	CandidateID   CandidateID     `json:"candidate_id"`
	From          Role            `json:"from"`
	To            Role            `json:"to"`
	Kind          MessageKind     `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
}

// CheckExpected is the small assertion vocabulary of a proposed check.
type CheckExpected struct {
	ExitCode       *int     `json:"exit_code,omitempty"`
	StdoutContains []string `json:"stdout_contains,omitempty"`
	StdoutRegex    string   `json:"stdout_regex,omitempty"`
}

// CheckSpecProposal is a model's proposal for a check. It is derived material:
// Niten validates it against the policy before anything runs, and an unverified
// proposal never closes a criterion.
type CheckSpecProposal struct {
	ID              string             `json:"id"`
	Method          VerificationMethod `json:"method"`
	Argv            []string           `json:"argv,omitempty"`
	Cwd             string             `json:"cwd,omitempty"`
	Timeout         string             `json:"timeout,omitempty"`
	Expected        CheckExpected      `json:"expected"`
	CriterionIDs    []string           `json:"criterion_ids"`
	VerificationIDs []string           `json:"verification_ids"`
}

// Handoff is the brief, unverified note an executor leaves for the next fresh session.
type Handoff struct {
	ImplementationNotes []string `json:"implementation_notes"`
	RemainingWork       []string `json:"remaining_work"`
	Risks               []string `json:"risks"`
	DecisionRefs        []string `json:"decision_refs"`
	EvidenceRefs        []string `json:"evidence_refs"`
}

// OffTargetJustification is the executor's explanation of one edit outside the plan targets.
type OffTargetJustification struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// CandidateReady is the executor's payload announcing a finished step candidate.
type CandidateReady struct {
	Steps                   []string                 `json:"steps"`
	Description             string                   `json:"description"`
	ChangedPathsClaimed     []string                 `json:"changed_paths_claimed"`
	OffTargetJustifications []OffTargetJustification `json:"off_target_justifications"`
	ProposedChecks          []CheckSpecProposal      `json:"proposed_checks"`
	Questions               []string                 `json:"questions"`
	Handoff                 Handoff                  `json:"handoff"`
}

// Location points at code.
type Location struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start,omitempty"`
	LineEnd   int    `json:"line_end,omitempty"`
}

// Finding is the reviewer's payload describing one defect.
type Finding struct {
	FindingID      string   `json:"finding_id"`
	Severity       Severity `json:"severity"`
	CriterionIDs   []string `json:"criterion_ids"`
	Location       Location `json:"location"`
	DefectScenario string   `json:"defect_scenario"`
	ExpectedFix    string   `json:"expected_fix"`
	EvidenceRefs   []string `json:"evidence_refs"`
}

// Response is the executor's payload answering one finding.
type Response struct {
	FindingID    string              `json:"finding_id"`
	Disposition  ResponseDisposition `json:"disposition"`
	Explanation  string              `json:"explanation"`
	EvidenceRefs []string            `json:"evidence_refs"`
}

// CheckRequest is the reviewer's payload proposing an additional coordinator-run check.
type CheckRequest struct {
	Proposal     CheckSpecProposal `json:"proposal"`
	Reason       string            `json:"reason"`
	CriterionIDs []string          `json:"criterion_ids"`
}

// Question is either model's payload for a missing fact or a user decision.
type Question struct {
	Text           string   `json:"text"`
	NeededDecision bool     `json:"needed_decision"`
	Options        []string `json:"options,omitempty"`
}

// OffTargetReviewDisposition is the reviewer's decision on one off-target edit.
type OffTargetReviewDisposition struct {
	Path        string           `json:"path"`
	Disposition OffTargetVerdict `json:"disposition"`
	Reason      string           `json:"reason"`
}

// ReviewCoverage states what the reviewer actually covered.
type ReviewCoverage struct {
	CriterionIDsChecked   []string                     `json:"criterion_ids_checked"`
	PathsReviewed         []string                     `json:"paths_reviewed"`
	OffTargetDispositions []OffTargetReviewDisposition `json:"off_target_dispositions"`
}

// FindingDisposition is the reviewer's state for an existing finding.
type FindingDisposition struct {
	FindingID string       `json:"finding_id"`
	State     FindingState `json:"state"`
}

// ReviewResult is the reviewer's payload closing one review.
type ReviewResult struct {
	Verdict             Verdict              `json:"verdict"`
	Coverage            ReviewCoverage       `json:"coverage"`
	FindingDispositions []FindingDisposition `json:"finding_dispositions"`
	Summary             string               `json:"summary"`
}

// OffTargetChange is the coordinator's record of one edit outside the plan targets.
type OffTargetChange struct {
	Path          string               `json:"path"`
	ReasonClaimed *string              `json:"reason_claimed"`
	Disposition   OffTargetDisposition `json:"disposition"`
}

// CandidateRecord is the coordinator's record of one candidate snapshot.
type CandidateRecord struct {
	SchemaVersion        int               `json:"schema_version"`
	CandidateID          CandidateID       `json:"candidate_id"`
	ParentCandidateID    *CandidateID      `json:"parent_candidate_id"`
	StepIDs              []string          `json:"step_ids"`
	ActualChangedPaths   []string          `json:"actual_changed_paths"`
	OffTargetChanges     []OffTargetChange `json:"off_target_changes"`
	HardPolicyViolations []string          `json:"hard_policy_violations"`
	CreatedAt            string            `json:"created_at"`
}

// CheckKey identifies reusable evidence; only a fully matching key is reused.
type CheckKey struct {
	PlanDigest        string `json:"plan_digest"`
	CandidateCommit   string `json:"candidate_commit"`
	CandidateTree     string `json:"candidate_tree"`
	ContractDigest    string `json:"contract_digest"`
	CheckID           string `json:"check_id"`
	CheckSpecDigest   string `json:"check_spec_digest"`
	EnvironmentDigest string `json:"environment_digest"`
}

// Assertion is one named expectation evaluated on a check's output.
type Assertion struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// CheckEvidence is the coordinator's record of one check run.
type CheckEvidence struct {
	SchemaVersion    int         `json:"schema_version"`
	Key              CheckKey    `json:"key"`
	Argv             []string    `json:"argv"`
	Cwd              string      `json:"cwd"`
	ToolVersion      string      `json:"tool_version"`
	EnvNames         []string    `json:"env_names"`
	StartedAt        string      `json:"started_at"`
	FinishedAt       *string     `json:"finished_at"`
	ExitCode         *int        `json:"exit_code"`
	StdoutRef        string      `json:"stdout_ref"`
	StderrRef        string      `json:"stderr_ref"`
	Assertions       []Assertion `json:"assertions"`
	SourcesUnchanged bool        `json:"sources_unchanged"`
	Status           CheckStatus `json:"status"`
}

// Attestation is the user's testimony for a pre-assigned human criterion, as stored.
// SubmittedAt and Source are added by the coordinator; a model cannot issue one.
type Attestation struct {
	SchemaVersion  int               `json:"schema_version"`
	CriterionID    string            `json:"criterion_id"`
	PlanDigest     string            `json:"plan_digest"`
	ContractDigest string            `json:"contract_digest"`
	CandidateSHA   string            `json:"candidate_sha"`
	Result         AttestationResult `json:"result"`
	Observation    string            `json:"observation"`
	Environment    string            `json:"environment"`
	ObservedAt     string            `json:"observed_at"`
	Actor          string            `json:"actor"`
	EvidenceRefs   []string          `json:"evidence_refs"`
	SubmittedAt    string            `json:"submitted_at"`
	Source         string            `json:"source"`
}

// StepContinue is the user's answer releasing one step gate. It is bound to the
// exact accepted candidate; an answer for an old SHA does not release a new gate.
type StepContinue struct {
	SchemaVersion  int    `json:"schema_version"`
	GateID         string `json:"gate_id"`
	RunID          string `json:"run_id"`
	PlanDigest     string `json:"plan_digest"`
	ContractDigest string `json:"contract_digest"`
	StepID         string `json:"step_id"`
	CandidateSHA   string `json:"candidate_sha"`
}

// HandoffRecord stores an executor handoff with its attempt; Verified stays false
// until the coordinator has checked the notes against evidence.
type HandoffRecord struct {
	SchemaVersion int     `json:"schema_version"`
	AttemptID     string  `json:"attempt_id"`
	Verified      bool    `json:"verified"`
	Handoff       Handoff `json:"handoff"`
}
