// Package contract holds the typed message and record contracts between Niten's
// coordinator, the executor model, the reviewer model and the user, together with
// the embedded JSON Schemas that validate their wire form.
//
// Envelope fields are coordinator-owned. Model payloads never carry sender,
// attempt, sequence or acceptance fields: every payload schema closes additional
// properties, so a smuggled coordinator field is a validation error.
package contract

import "fmt"

// SchemaVersion is the schema_version every envelope and record carries.
const SchemaVersion = 1

// Role identifies a participant of a run.
type Role string

const (
	RoleExecutor    Role = "executor"
	RoleReviewer    Role = "reviewer"
	RoleCoordinator Role = "coordinator"
	RoleUser        Role = "user"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleExecutor, RoleReviewer, RoleCoordinator, RoleUser:
		return true
	}
	return false
}

// MessageKind names a model message. Each kind has exactly one payload schema.
type MessageKind string

const (
	KindCandidateReady MessageKind = "candidate_ready"
	KindFinding        MessageKind = "finding"
	KindResponse       MessageKind = "response"
	KindCheckRequest   MessageKind = "check_request"
	KindQuestion       MessageKind = "question"
	KindReviewResult   MessageKind = "review_result"
)

// MessageKinds lists every kind in a stable order.
var MessageKinds = []MessageKind{KindCandidateReady, KindFinding, KindResponse, KindCheckRequest, KindQuestion, KindReviewResult}

// Valid reports whether k is a known message kind.
func (k MessageKind) Valid() bool {
	_, ok := senders[k]
	return ok
}

// senders lists the roles allowed to originate each kind. Models talk to the
// coordinator only; the coordinator relays and records.
var senders = map[MessageKind][]Role{
	KindCandidateReady: {RoleExecutor},
	KindFinding:        {RoleReviewer},
	KindResponse:       {RoleExecutor},
	KindCheckRequest:   {RoleReviewer},
	KindQuestion:       {RoleExecutor, RoleReviewer},
	KindReviewResult:   {RoleReviewer},
}

// Senders returns the roles that may originate kind.
func (k MessageKind) Senders() []Role { return append([]Role(nil), senders[k]...) }

// RunState is the state of a whole run.
type RunState string

const (
	RunPrepared    RunState = "prepared"
	RunRunning     RunState = "running"
	RunNeedsInput  RunState = "needs_input"
	RunPaused      RunState = "paused"
	RunFailed      RunState = "failed"
	RunImplemented RunState = "implemented"
	RunDone        RunState = "done"
)

// Valid reports whether s is a known run state.
func (s RunState) Valid() bool {
	switch s {
	case RunPrepared, RunRunning, RunNeedsInput, RunPaused, RunFailed, RunImplemented, RunDone:
		return true
	}
	return false
}

// StepState is the state of one plan step.
type StepState string

const (
	StepPending          StepState = "pending"
	StepImplementing     StepState = "implementing"
	StepCandidate        StepState = "candidate"
	StepReviewing        StepState = "reviewing"
	StepChangesRequested StepState = "changes_requested"
	StepAccepted         StepState = "accepted"
	StepAwaitingExternal StepState = "awaiting_external"
	StepBlocked          StepState = "blocked"
)

// Valid reports whether s is a known step state.
func (s StepState) Valid() bool {
	switch s {
	case StepPending, StepImplementing, StepCandidate, StepReviewing, StepChangesRequested, StepAccepted, StepAwaitingExternal, StepBlocked:
		return true
	}
	return false
}

// ExitCode is a process exit status of the niten CLI (architecture, "Proposed CLI").
type ExitCode int

const (
	ExitOK          ExitCode = 0   // successful operation; run/resume: only done
	ExitRejected    ExitCode = 1   // rejected result or failed gate
	ExitFormat      ExitCode = 2   // format, configuration or protocol error
	ExitNeedsInput  ExitCode = 3   // needs input
	ExitPaused      ExitCode = 4   // paused
	ExitImplemented ExitCode = 5   // implemented with pending_external
	ExitInterrupted ExitCode = 130 // SIGINT
)

// Valid reports whether c is a documented exit code.
func (c ExitCode) Valid() bool {
	switch c {
	case ExitOK, ExitRejected, ExitFormat, ExitNeedsInput, ExitPaused, ExitImplemented, ExitInterrupted:
		return true
	}
	return false
}

// ExecutionOwner says who executes a criterion or check: the coordinator or a human.
type ExecutionOwner string

const (
	OwnerNiten ExecutionOwner = "niten"
	OwnerHuman ExecutionOwner = "human"
)

// Valid reports whether o is a known owner.
func (o ExecutionOwner) Valid() bool { return o == OwnerNiten || o == OwnerHuman }

// VerificationMethod mirrors Shogun's verification.method enum.
type VerificationMethod string

const (
	MethodTest    VerificationMethod = "test"
	MethodCommand VerificationMethod = "command"
	MethodInspect VerificationMethod = "inspect"
	MethodMeasure VerificationMethod = "measure" // schema value kept; backend deferred beyond v0.1
)

// Valid reports whether m is a known method.
func (m VerificationMethod) Valid() bool {
	switch m {
	case MethodTest, MethodCommand, MethodInspect, MethodMeasure:
		return true
	}
	return false
}

// Severity of a finding.
type Severity string

const (
	SeverityBlocker Severity = "blocker"
	SeverityMajor   Severity = "major"
	SeverityMinor   Severity = "minor"
)

// Valid reports whether s is a known severity.
func (s Severity) Valid() bool {
	return s == SeverityBlocker || s == SeverityMajor || s == SeverityMinor
}

// ResponseDisposition is the executor's answer to a finding.
type ResponseDisposition string

const (
	ResponseFixed    ResponseDisposition = "fixed"
	ResponseDisputed ResponseDisposition = "disputed"
)

// Valid reports whether d is a known disposition.
func (d ResponseDisposition) Valid() bool { return d == ResponseFixed || d == ResponseDisputed }

// Verdict of a review.
type Verdict string

const (
	VerdictApprove Verdict = "approve"
	VerdictRevise  Verdict = "revise"
	VerdictBlocked Verdict = "blocked"
)

// Valid reports whether v is a known verdict.
func (v Verdict) Valid() bool {
	return v == VerdictApprove || v == VerdictRevise || v == VerdictBlocked
}

// FindingState is the reviewer's disposition of an existing finding.
type FindingState string

const (
	FindingOpen      FindingState = "open"
	FindingFixed     FindingState = "fixed"
	FindingWithdrawn FindingState = "withdrawn"
)

// Valid reports whether s is a known finding state.
func (s FindingState) Valid() bool {
	return s == FindingOpen || s == FindingFixed || s == FindingWithdrawn
}

// OffTargetVerdict is the reviewer's decision on one off-target edit.
type OffTargetVerdict string

const (
	OffTargetAccept OffTargetVerdict = "accept"
	OffTargetReject OffTargetVerdict = "reject"
)

// Valid reports whether v is a known verdict.
func (v OffTargetVerdict) Valid() bool { return v == OffTargetAccept || v == OffTargetReject }

// OffTargetDisposition is the coordinator-recorded state of one off-target edit.
type OffTargetDisposition string

const (
	OffTargetPending  OffTargetDisposition = "pending"
	OffTargetAccepted OffTargetDisposition = "accepted"
	OffTargetRejected OffTargetDisposition = "rejected"
)

// Valid reports whether d is a known disposition.
func (d OffTargetDisposition) Valid() bool {
	return d == OffTargetPending || d == OffTargetAccepted || d == OffTargetRejected
}

// CheckStatus is the outcome recorded for one check run.
type CheckStatus string

const (
	CheckPassed      CheckStatus = "passed"
	CheckFailed      CheckStatus = "failed"
	CheckUnknown     CheckStatus = "unknown"
	CheckInvalidated CheckStatus = "invalidated"
)

// Valid reports whether s is a known status.
func (s CheckStatus) Valid() bool {
	return s == CheckPassed || s == CheckFailed || s == CheckUnknown || s == CheckInvalidated
}

// AttestationResult is the user's reported result for a human criterion.
type AttestationResult string

const (
	AttestationPassed AttestationResult = "passed"
	AttestationFailed AttestationResult = "failed"
)

// Valid reports whether r is a known result.
func (r AttestationResult) Valid() bool { return r == AttestationPassed || r == AttestationFailed }

// AttestationSource is the only channel through which an attestation may arrive.
const AttestationSource = "resume_answers"

// String implements fmt.Stringer for log lines.
func (c ExitCode) String() string { return fmt.Sprintf("exit %d", int(c)) }
