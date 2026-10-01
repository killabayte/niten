package engine

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/killabayte/niten/internal/contract"
)

// applyAnswers records the user's decisions from a resume --answers file.
// Models cannot produce this channel. A refused entry is reported and changes
// nothing; the others still apply.
func (e *Engine) applyAnswers(raw []byte) (*Outcome, error) {
	if err := contract.ValidateDocument(contract.DocAnswers, raw); err != nil {
		return nil, fmt.Errorf("answers: %w", err)
	}
	var a contract.Answers
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("answers: %w", err)
	}
	// Named by content: a refused file records no event, so the journal
	// position cannot name the next one.
	ref := "answers/" + digest(raw)[:16] + ".json"
	sha, err := e.writeOrReuse(ref, raw)
	if err != nil {
		return nil, err
	}
	var refused []string
	for _, sc := range a.StepContinue {
		if why := e.releaseGate(sc, ref, sha); why != "" {
			refused = append(refused, why)
		}
	}
	for _, at := range a.Attestations {
		if why, err := e.attest(at); err != nil {
			return nil, err
		} else if why != "" {
			refused = append(refused, why)
		}
	}
	for _, qa := range a.Answers {
		if why := e.answer(qa, ref, sha); why != "" {
			refused = append(refused, why)
		}
	}
	if len(refused) > 0 {
		exit := contract.ExitNeedsInput
		if e.st.State == contract.RunImplemented {
			exit = contract.ExitImplemented
		}
		return &Outcome{State: e.st.State, Reason: "answers_refused", Detail: refused, Exit: exit}, nil
	}
	return nil, nil
}

// releaseGate releases the open gate only with an answer bound to it exactly:
// gate, run, plan and contract digests, step and accepted candidate. Repeating
// the answer for a released gate is idempotent.
func (e *Engine) releaseGate(sc contract.StepContinue, ref, sha string) string {
	if e.released(sc.GateID) {
		for _, ev := range e.events {
			if ev.Type != evGateOpened {
				continue
			}
			var g GateView
			if json.Unmarshal(ev.Data, &g) == nil && g.ID == sc.GateID && g.Unit == sc.StepID && g.Candidate == sc.CandidateSHA {
				return ""
			}
		}
		return fmt.Sprintf("step_continue %s: the gate was released for a different step or candidate", sc.GateID)
	}
	g := e.st.Gate
	switch {
	case g == nil:
		return fmt.Sprintf("step_continue %s: no step gate is open", sc.GateID)
	case sc.GateID != g.ID || sc.StepID != g.Unit || sc.CandidateSHA != g.Candidate:
		return fmt.Sprintf("step_continue %s for %s at %s does not match the open gate %s for %s at %s", sc.GateID, sc.StepID, sc.CandidateSHA, g.ID, g.Unit, g.Candidate)
	case sc.RunID != e.st.RunID || sc.PlanDigest != e.c.PlanDigest || sc.ContractDigest != e.contractSHA:
		return fmt.Sprintf("step_continue %s is bound to another run, plan or contract", sc.GateID)
	}
	if err := e.emit(evGateReleased, gateReleasedData{Gate: g.ID, AnswerRef: ref, AnswerSHA: sha}); err != nil {
		return err.Error()
	}
	return ""
}

// attest stores a user attestation for a pre-assigned human criterion of the
// implemented candidate.
func (e *Engine) attest(in contract.AttestationInput) (string, error) {
	if e.st.State != contract.RunImplemented {
		return fmt.Sprintf("attestation %s: the run is %s; attestations bind the implemented candidate", in.CriterionID, e.st.State), nil
	}
	_, pending := e.criteriaStatus(e.st, map[string]bool{})
	want := false
	for _, c := range e.c.Criteria {
		if c.ID == in.CriterionID {
			want = true
		}
	}
	isPending := false
	for _, p := range pending {
		isPending = isPending || p == in.CriterionID
	}
	switch {
	case !want:
		return fmt.Sprintf("attestation %s: no such criterion", in.CriterionID), nil
	case !isPending:
		return fmt.Sprintf("attestation %s: the criterion is not pending an external confirmation", in.CriterionID), nil
	case in.CandidateSHA != e.st.Head:
		return fmt.Sprintf("attestation %s is for %s; the implemented candidate is %s", in.CriterionID, in.CandidateSHA, e.st.Head), nil
	case in.PlanDigest != e.c.PlanDigest || in.ContractDigest != e.contractSHA:
		return fmt.Sprintf("attestation %s is bound to another plan or contract", in.CriterionID), nil
	}
	if at, err := time.Parse(time.RFC3339, in.ObservedAt); err != nil || at.After(e.o.Now().Add(time.Minute)) {
		return fmt.Sprintf("attestation %s: observed_at %s is not a past time", in.CriterionID, in.ObservedAt), nil
	}
	rec := contract.Attestation{SchemaVersion: contract.SchemaVersion, CriterionID: in.CriterionID, PlanDigest: in.PlanDigest, ContractDigest: in.ContractDigest,
		CandidateSHA: in.CandidateSHA, Result: in.Result, Observation: in.Observation, Environment: in.Environment, ObservedAt: in.ObservedAt,
		Actor: in.Actor, EvidenceRefs: in.EvidenceRefs, SubmittedAt: e.o.Now().UTC().Format(time.RFC3339), Source: contract.AttestationSource}
	b, err := json.MarshalIndent(rec, "", " ")
	if err != nil {
		return "", err
	}
	if err := contract.ValidateRecord(contract.RecordAttestation, b); err != nil {
		return fmt.Sprintf("attestation %s: %v", in.CriterionID, err), nil
	}
	ref := fmt.Sprintf("attestations/%s-%06d.json", in.CriterionID, e.st.LastSeq+1)
	sha, err := e.writeOrReuse(ref, append(b, '\n'))
	if err != nil {
		return "", err
	}
	return "", e.emit(evAttestation, AttestationView{Criterion: in.CriterionID, Result: string(in.Result), Candidate: in.CandidateSHA, Ref: ref, SHA: sha})
}

// answer records the user's answer to an open blocking question.
func (e *Engine) answer(qa contract.QuestionAnswer, ref, sha string) string {
	for _, q := range e.st.Questions {
		if q.ID != qa.QuestionID {
			continue
		}
		if q.Answered {
			if q.Answer == qa.Text {
				return ""
			}
			return fmt.Sprintf("question %s is already answered differently", qa.QuestionID)
		}
		if err := e.emit(evAnswer, answerData{Question: q.ID, Text: qa.Text, Ref: ref, SHA: sha}); err != nil {
			return err.Error()
		}
		return ""
	}
	return fmt.Sprintf("question %s is not a question of this run", qa.QuestionID)
}
