package engine

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/killabayte/niten/internal/attempt"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/provider"
)

// parsers re-parse saved streams of a crashed coordinator's attempts with the
// same adapter requests the attempts ran with.
func (e *Engine) parsers() (map[string]attempt.ParseFunc, error) {
	ereq, err := e.executorRequest("")
	if err != nil {
		return nil, err
	}
	return map[string]attempt.ParseFunc{
		string(contract.RoleExecutor): func(so, se string, o provider.Outcome) (*provider.Result, *provider.Error) {
			return provider.ParseClaude(so, se, o, ereq)
		},
		string(contract.RoleReviewer): func(so, se string, o provider.Outcome) (*provider.Result, *provider.Error) {
			id := filepath.Base(filepath.Dir(so))
			return provider.ParseCodex(so, se, o, e.reviewerRequest(e.reviewBase(id)))
		},
	}, nil
}

// recoverAttempts resolves the attempts a crashed coordinator left and
// processes every started turn that has no recorded outcome, never by calling
// the model again: a saved result is applied, a process that never started
// lets the turn run anew, and an unknown outcome keeps the worktree as a
// snapshot and asks the user.
func (e *Engine) recoverAttempts(ctx context.Context) (*Outcome, error) {
	parsers, err := e.parsers()
	if err != nil {
		return nil, err
	}
	recs, err := attempt.Recover(e.run, e.events, parsers, e.o.Grace)
	if err != nil {
		return nil, err
	}
	byID := map[string]attempt.Recovered{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	for _, r := range recs {
		if r.Blocking() {
			return e.stop(contract.RunPaused, "blocked", []string{fmt.Sprintf("attempt %s: %s %v", r.ID, r.Reason, r.Remains)}, contract.ExitPaused)
		}
	}
	for _, t := range e.st.Turns {
		if t.Outcome != "" {
			continue
		}
		r, ok := byID[t.ID]
		var out *Outcome
		switch {
		case !ok || r.Status == attempt.StatusNotStarted:
			err = e.emit(evProcessed, processedData{Turn: t.ID, Outcome: OutcomeNotStarted, Reasons: []string{"the model process never started"}})
		case r.Status == attempt.StatusUnknown:
			out, err = e.unknownTurn(ctx, t, r.Reason)
		case t.Role == string(contract.RoleExecutor):
			e.logf("%s: applying the saved result of %s (%s)", t.Unit, t.ID, r.Status)
			out, err = e.processExecutor(ctx, t, r.Result, r.Err)
		default:
			e.logf("%s: applying the saved result of %s (%s)", t.Unit, t.ID, r.Status)
			out, err = e.processReviewer(ctx, t, r.Result, r.Err)
		}
		if err != nil || out != nil {
			return out, err
		}
	}
	return nil, nil
}

// unknownTurn records a turn whose outcome cannot be established. The
// executor's worktree is kept as a rejected snapshot and restored; the user
// decides whether to continue.
func (e *Engine) unknownTurn(ctx context.Context, t *TurnView, reason string) (*Outcome, error) {
	pd := processedData{Turn: t.ID, Outcome: OutcomeUnknown, Reasons: []string{reason}}
	detail := []string{fmt.Sprintf("the outcome of %s cannot be established: %s", t.ID, reason)}
	if t.Role == string(contract.RoleExecutor) {
		u := e.st.Unit(t.Unit)
		ins, err := e.clone.Inspect(ctx, e.rules(u))
		if err != nil {
			return nil, err
		}
		if len(ins.Changes) > 0 || len(ins.Violations) > 0 || len(ins.Ignored) > 0 {
			plan, out, err := e.planDiscard(ctx, ins, t.ID, turnTime(t))
			if out != nil || err != nil {
				return out, err
			}
			pd.Discard = plan
			detail = append(detail, "its changes are kept at "+plan.Snapshot)
		}
	}
	if err := e.emit(evProcessed, pd); err != nil {
		return nil, err
	}
	if err := e.completeDiscard(ctx, e.st.Unit(t.Unit)); err != nil {
		return nil, err
	}
	detail = append(detail, "resume again to run the turn anew")
	return e.stop(contract.RunNeedsInput, "outcome_unknown", detail, contract.ExitNeedsInput)
}
