package engine

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/verify"
	"github.com/killabayte/niten/internal/workspace"
)

// requiredSpecs are the policy's required checks; they run on every candidate.
func (e *Engine) requiredSpecs() ([]CheckSpec, error) {
	req, err := e.requiredChecks()
	if err != nil {
		return nil, err
	}
	cmds, err := e.policyCommands()
	if err != nil {
		return nil, err
	}
	var out []CheckSpec
	for _, r := range req {
		idx := slices.IndexFunc(cmds, func(c configCommand) bool { return c.ID == r.CommandID })
		if idx < 0 {
			return nil, fmt.Errorf("%w: required check %s names the unknown command %s", ErrIntegrity, r.ID, r.CommandID)
		}
		cwd := r.Cwd
		if cwd == "" {
			cwd = "."
		}
		code := r.ExpectedExitCode
		out = append(out, CheckSpec{ID: "required/" + r.ID, Source: "required", CommandID: r.CommandID, Argv: slices.Clone(cmds[idx].Argv),
			Cwd: cwd, Timeout: cmds[idx].Timeout.D().String(), Expected: contract.CheckExpected{ExitCode: &code},
			CriterionIDs: []string{}, VerificationIDs: []string{}})
	}
	return out, nil
}

// specsFor is the check set of a unit: the required checks and the unit's
// accepted proposals; the final stage runs every unit's checks.
func (e *Engine) specsFor(u *StepView) ([]CheckSpec, error) {
	out, err := e.requiredSpecs()
	if err != nil {
		return nil, err
	}
	units := []*StepView{u}
	if u.ID == FinalUnit {
		units = append(slices.Clone(e.st.Steps), e.st.Final)
	}
	for _, x := range units {
		for _, s := range x.Specs {
			if !slices.ContainsFunc(out, func(o CheckSpec) bool { return o.ID == s.ID }) {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// coverageProblems lists the coordinator-owned test and command
// verifications of a unit that no check in the set covers.
func (e *Engine) coverageProblems(u *StepView, specs []CheckSpec) []string {
	var out []string
	for _, c := range e.c.Checks {
		if u.ID != FinalUnit && c.StepID != u.ID {
			continue
		}
		if c.Owner != string(contract.OwnerNiten) || (c.Method != string(contract.MethodTest) && c.Method != string(contract.MethodCommand)) {
			continue
		}
		if !slices.ContainsFunc(specs, func(s CheckSpec) bool { return slices.Contains(s.VerificationIDs, c.ScopedID) }) {
			out = append(out, fmt.Sprintf("verification %s (%s: %s) has no check; propose one from the allowed commands", c.ScopedID, c.Method, c.Expected))
		}
	}
	return out
}

// evaluate runs the unit's checks on its candidate in the verifier sandbox.
// Equal specs run once and share their evidence.
func (e *Engine) evaluate(ctx context.Context, u *StepView) (*Outcome, error) {
	specs, err := e.specsFor(u)
	if err != nil {
		return nil, err
	}
	problems := e.coverageProblems(u, specs)
	cand := workspace.Candidate{Commit: u.Candidate.Commit, Tree: u.Candidate.Tree}
	ran := map[string]CheckOutcome{}
	var results []CheckOutcome
	unknown := false
	for _, s := range specs {
		key := digestJSON([]any{s.Argv, s.Cwd, s.Timeout, s.Expected})
		r, ok := ran[key]
		if !ok {
			timeout, _ := time.ParseDuration(s.Timeout)
			e.logf("%s: check %s (%s)", u.ID, s.ID, strings.Join(s.Argv, " "))
			res, err := e.verifier.Run(ctx, verify.Request{Check: verify.Check{ID: s.ID, Argv: s.Argv, Cwd: s.Cwd, Timeout: timeout, Expected: s.Expected},
				Candidate: cand, PlanDigest: e.c.PlanDigest, ContractDigest: e.contractSHA})
			if err != nil {
				return nil, fmt.Errorf("check %s: %w", s.ID, err)
			}
			r = CheckOutcome{Status: res.Evidence.Status, EvidenceRef: res.EvidenceRef, EvidenceSHA: res.EvidenceDigest, Reasons: res.Reasons}
			ran[key] = r
		}
		r.ID = s.ID
		results = append(results, r)
		unknown = unknown || r.Status == contract.CheckUnknown
		if r.Status != contract.CheckPassed {
			problems = append(problems, fmt.Sprintf("check %s %s: %s", s.ID, r.Status, strings.Join(r.Reasons, "; ")))
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })
	view := ChecksView{Candidate: cand.Commit, Results: results, Problems: problems, Passed: len(problems) == 0}
	if view.Problems == nil {
		view.Problems = []string{}
	}
	view.Digest = evidenceDigest(cand.Commit, results, view.Problems)
	// Refused proposals of the executor's turn stay with the unit; they block
	// only through the coverage they leave missing.
	carried := slices.Clone(u.Problems)
	tr := transition{Unit: u.ID, Checks: &view}
	switch {
	case unknown:
	case view.Passed:
		tr.Step = &stepStateData{State: contract.StepReviewing, Repairs: u.Repairs, Problems: nonNil(carried)}
	default:
		tr.Step = &stepStateData{State: contract.StepChangesRequested, Repairs: u.Repairs, Problems: append(carried, view.Problems...)}
	}
	if err := e.emit(evChecks, tr); err != nil {
		return nil, err
	}
	e.logf("%s: checks %s at %s (evidence %s)", u.ID, map[bool]string{true: "passed", false: "did not pass"}[view.Passed], short(cand.Commit), short(view.Digest))
	if unknown {
		return e.stop(contract.RunPaused, "check_unknown", view.Problems, contract.ExitPaused)
	}
	return nil, nil
}

// evidenceDigest binds a candidate to the exact evidence a reviewer is shown.
func evidenceDigest(commit string, results []CheckOutcome, problems []string) string {
	type line struct {
		ID, SHA string
		Status  contract.CheckStatus
	}
	var ls []line
	for _, r := range results {
		ls = append(ls, line{r.ID, r.EvidenceSHA, r.Status})
	}
	return digestJSON(map[string]any{"candidate": commit, "checks": ls, "problems": problems})
}
