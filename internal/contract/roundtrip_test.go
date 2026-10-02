package contract

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Every typed struct marshals to JSON that its own schema accepts and unmarshals
// back to an equal value, so the Go types and the schemas cannot drift apart.
func TestRoundTrip(t *testing.T) {
	targets := map[string]any{
		envelopeSchema:             &Envelope{},
		string(KindCandidateReady): &CandidateReady{},
		string(KindFinding):        &Finding{},
		string(KindResponse):       &Response{},
		string(KindCheckRequest):   &CheckRequest{},
		string(KindQuestion):       &Question{},
		string(KindReviewResult):   &ReviewResult{},
		RecordCandidate:            &CandidateRecord{},
		RecordCheck:                &CheckEvidence{},
		RecordAttestation:          &Attestation{},
		RecordStepGate:             &StepContinue{},
		RecordHandoff:              &HandoffRecord{},
		DocExecutorTurn:            &ExecutorTurn{},
		DocReviewerTurn:            &ReviewerTurn{},
		DocAnswers:                 &Answers{},
	}
	for _, name := range Schemas() {
		ptr, ok := targets[name]
		if !ok {
			t.Fatalf("no Go type registered for schema %q", name)
		}
		t.Run(name, func(t *testing.T) {
			raw := readFixture(t, "valid/"+name+".json")
			if err := json.Unmarshal(raw, ptr); err != nil {
				t.Fatal(err)
			}
			if err := ValidateValue(name, ptr); err != nil {
				t.Fatalf("marshalled Go value rejected by its schema: %v", err)
			}
			again := reflect.New(reflect.TypeOf(ptr).Elem()).Interface()
			data, _ := json.Marshal(ptr)
			if err := json.Unmarshal(data, again); err != nil {
				t.Fatal(err)
			}
			if name == envelopeSchema {
				// RawMessage bytes differ in whitespace; compare the decoded payloads.
				a, b := ptr.(*Envelope), again.(*Envelope)
				var pa, pb any
				_ = json.Unmarshal(a.Payload, &pa)
				_ = json.Unmarshal(b.Payload, &pb)
				a.Payload, b.Payload = nil, nil
				if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(pa, pb) {
					t.Fatalf("envelope round trip differs")
				}
				return
			}
			if !reflect.DeepEqual(ptr, again) {
				t.Fatalf("round trip differs:\n%+v\n%+v", ptr, again)
			}
		})
	}
}

// A zero-valued struct must not pass: required fields are really required.
func TestZeroValuesRejected(t *testing.T) {
	for name, v := range map[string]any{
		string(KindCandidateReady): CandidateReady{},
		string(KindFinding):        Finding{},
		string(KindReviewResult):   ReviewResult{},
		RecordCandidate:            CandidateRecord{},
		RecordCheck:                CheckEvidence{},
		RecordAttestation:          Attestation{},
		RecordStepGate:             StepContinue{},
		RecordHandoff:              HandoffRecord{},
	} {
		if err := ValidateValue(name, v); err == nil {
			t.Errorf("%s: zero value accepted", name)
		}
	}
}

func TestEnums(t *testing.T) {
	for _, r := range []Role{RoleExecutor, RoleReviewer, RoleCoordinator, RoleUser} {
		if !r.Valid() {
			t.Errorf("role %q invalid", r)
		}
	}
	if Role("model").Valid() {
		t.Error("unknown role valid")
	}
	for _, k := range MessageKinds {
		if !k.Valid() || len(k.Senders()) == 0 {
			t.Errorf("kind %q invalid or without senders", k)
		}
	}
	if MessageKind("approval").Valid() {
		t.Error("unknown kind valid")
	}
	for _, s := range []RunState{RunPrepared, RunRunning, RunNeedsInput, RunPaused, RunFailed, RunImplemented, RunDone} {
		if !s.Valid() {
			t.Errorf("run state %q invalid", s)
		}
	}
	if RunState("cancelled").Valid() {
		t.Error("unknown run state valid")
	}
	for _, s := range []StepState{StepPending, StepImplementing, StepCandidate, StepReviewing, StepChangesRequested, StepAccepted, StepAwaitingExternal, StepBlocked} {
		if !s.Valid() {
			t.Errorf("step state %q invalid", s)
		}
	}
	if StepState("done").Valid() {
		t.Error("unknown step state valid")
	}
	for _, o := range []ExecutionOwner{OwnerNiten, OwnerHuman} {
		if !o.Valid() {
			t.Errorf("owner %q invalid", o)
		}
	}
	if ExecutionOwner("model").Valid() {
		t.Error("model owner valid")
	}
	for _, m := range []VerificationMethod{MethodTest, MethodCommand, MethodInspect, MethodMeasure} {
		if !m.Valid() {
			t.Errorf("method %q invalid", m)
		}
	}
	if !SeverityBlocker.Valid() || Severity("critical").Valid() {
		t.Error("severity enum wrong")
	}
	if !ResponseFixed.Valid() || ResponseDisposition("closed").Valid() {
		t.Error("response disposition enum wrong")
	}
	if !VerdictBlocked.Valid() || Verdict("done").Valid() {
		t.Error("verdict enum wrong")
	}
	if !FindingWithdrawn.Valid() || FindingState("closed").Valid() {
		t.Error("finding state enum wrong")
	}
	if !OffTargetReject.Valid() || OffTargetVerdict("accepted").Valid() {
		t.Error("off-target verdict enum wrong")
	}
	if !OffTargetPending.Valid() || OffTargetDisposition("accept").Valid() {
		t.Error("off-target disposition enum wrong")
	}
	if !CheckInvalidated.Valid() || CheckStatus("ok").Valid() {
		t.Error("check status enum wrong")
	}
	if !AttestationPassed.Valid() || AttestationResult("ok").Valid() {
		t.Error("attestation result enum wrong")
	}
}

// Exit codes follow the architecture table (Proposed CLI).
func TestExitCodes(t *testing.T) {
	want := map[ExitCode]int{ExitOK: 0, ExitRejected: 1, ExitFormat: 2, ExitNeedsInput: 3, ExitPaused: 4, ExitImplemented: 5, ExitInterrupted: 130}
	for c, n := range want {
		if int(c) != n || !c.Valid() {
			t.Errorf("%v != %d or invalid", c, n)
		}
	}
	if ExitCode(6).Valid() || ExitCode(-1).Valid() {
		t.Error("undocumented exit code valid")
	}
	if ExitInterrupted.String() != "exit 130" {
		t.Errorf("String() = %q", ExitInterrupted.String())
	}
}
