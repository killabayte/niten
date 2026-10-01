// Package engine is the sole owner of a run's state after prepare: it drives
// the sequential loop of executor turn, coordinator checks and independent
// review for every step, with bounded repairs, optional human step gates, a
// final check and review of the whole change, and the execution receipt.
//
// Every transition is one journal event folded by the same reducer live and
// on recovery; artifacts are written (idempotently) before the event that
// references them. Models are only ever reached through the attempt protocol,
// and nothing a model writes is trusted until the coordinator has resolved it
// against the plan, the clone and its own evidence.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/attempt"
	"github.com/killabayte/niten/internal/config"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/plan"
	"github.com/killabayte/niten/internal/store"
	"github.com/killabayte/niten/internal/verify"
	"github.com/killabayte/niten/internal/verify/sandbox"
	"github.com/killabayte/niten/internal/workspace"
)

// Options configure one engine session.
type Options struct {
	Store   *store.Store
	RunID   string
	Out     io.Writer // progress lines; nil discards them
	Now     func() time.Time
	Getenv  func(string) string
	Environ func() []string
	// Grace is how long a recorded process gets between TERM and KILL during recovery.
	Grace time.Duration
	// Heartbeat is the interval of progress lines while a model runs.
	Heartbeat time.Duration
	// NewBackend creates the verifier sandbox backend.
	NewBackend func(profileDir string) (*sandbox.Seatbelt, error)
}

// Outcome is how a session ended.
type Outcome struct {
	State  contract.RunState `json:"state"`
	Reason string            `json:"reason,omitempty"`
	Detail []string          `json:"detail,omitempty"`
	Exit   contract.ExitCode `json:"exit"`
}

// model is one resolved role binding: provider, exact model, effort, binary.
type model struct {
	Provider string
	Model    string
	Effort   string
	Bin      string
}

// Engine drives one open run. It holds the run lock for its lifetime.
type Engine struct {
	o           Options
	run         *store.Run
	events      []store.Event
	c           *plan.Contract
	doc         *plan.Document
	contractSHA string
	cfg         config.Config
	st          *State
	work        string
	clone       *workspace.Clone
	verifier    *verify.Verifier
	exec, rev   model
	runner      *attempt.Runner
	// crashAfter is a test hook: a non-nil error stops the engine right after
	// an attempt, before its result is processed, as a crash would.
	crashAfter func(turn string) error
}

// ErrIntegrity means a run file no longer matches what prepare recorded.
var ErrIntegrity = errors.New("run integrity")

// Open locks the run, verifies the contract and the stored inputs against
// their recorded digests and rebuilds the state from the journal.
func Open(o Options) (*Engine, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Grace <= 0 {
		o.Grace = 5 * time.Second
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 30 * time.Second
	}
	if o.NewBackend == nil {
		o.NewBackend = sandbox.New
	}
	if !store.ValidRunID(o.RunID) {
		return nil, fmt.Errorf("invalid run id %q", o.RunID)
	}
	run, events, err := o.Store.OpenRun(o.RunID)
	if err != nil {
		return nil, err
	}
	e := &Engine{o: o, run: run, events: events, runner: &attempt.Runner{Store: run}}
	if err := e.load(); err != nil {
		run.Close()
		return nil, err
	}
	run.Observe(e.observe)
	return e, nil
}

// Close releases the run lock.
func (e *Engine) Close() error { return e.run.Close() }

// State returns the current projection.
func (e *Engine) State() *State { return e.st }

func (e *Engine) load() error {
	cb, err := e.run.ReadArtifact("contract.json", "")
	if err != nil {
		return fmt.Errorf("%w: contract.json: %v", ErrIntegrity, err)
	}
	e.contractSHA = digest(cb)
	want := ""
	for _, ev := range e.events {
		if ev.Type == evSession {
			var d sessionData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				return fmt.Errorf("%w: event %d: %v", store.ErrCorrupt, ev.Seq, err)
			}
			want = d.ContractSHA256
			break
		}
	}
	if want == "" {
		var prepared struct {
			ContractSHA256 string `json:"contract_sha256"`
		}
		if err := e.run.LoadState(&prepared); err != nil {
			return fmt.Errorf("%w: state.json: %v", ErrIntegrity, err)
		}
		want = prepared.ContractSHA256
	}
	if want == "" || want != e.contractSHA {
		return fmt.Errorf("%w: contract.json changed since preparation (sha256 %s, recorded %s)", ErrIntegrity, e.contractSHA, want)
	}
	var c plan.Contract
	if err := json.Unmarshal(cb, &c); err != nil {
		return fmt.Errorf("%w: contract.json: %v", ErrIntegrity, err)
	}
	if c.Kind != plan.ContractKind || c.SchemaVersion != plan.ContractVersion || c.RunID != e.o.RunID || c.Document == nil {
		return fmt.Errorf("%w: contract.json is not this run's execution contract", ErrIntegrity)
	}
	if len(c.Repos) != 1 || c.Repos[0].Role != "write" {
		return fmt.Errorf("%w: v0.1 executes exactly one writable repository", ErrIntegrity)
	}
	if c.Limits.MaxAheadSteps != 0 {
		return fmt.Errorf("%w: max_ahead_steps %d is unsupported in v0.1", ErrIntegrity, c.Limits.MaxAheadSteps)
	}
	e.c, e.doc = &c, c.Document
	if err := e.verifyInputs(); err != nil {
		return err
	}
	cfgBytes, err := e.run.ReadArtifact("config.json", "")
	if err != nil {
		return fmt.Errorf("%w: config.json: %v", ErrIntegrity, err)
	}
	var loaded config.Loaded
	if err := json.Unmarshal(cfgBytes, &loaded); err != nil {
		return fmt.Errorf("%w: config.json: %v", ErrIntegrity, err)
	}
	e.cfg = loaded.Config
	if e.exec, err = parseModel(c.Models.Executor, "claude", c.Models.ClaudeCommand); err != nil {
		return err
	}
	if e.rev, err = parseModel(c.Models.Reviewer, "codex", c.Models.CodexCommand); err != nil {
		return err
	}
	lim := Limits{MaxInvocations: c.Limits.MaxInvocations, MaxActiveTime: c.Limits.MaxActiveTime, InvocationDeadline: c.Limits.InvocationDeadline,
		MaxRepairsPerStep: c.Limits.MaxRepairsPerStep, FinalReserveInvocations: c.Limits.FinalReserveInvocations, FinalReserveTime: c.Limits.FinalReserveTime}
	for _, d := range []string{lim.MaxActiveTime, lim.InvocationDeadline, lim.FinalReserveTime} {
		if _, err := time.ParseDuration(d); err != nil {
			return fmt.Errorf("%w: limit %q: %v", ErrIntegrity, d, err)
		}
	}
	e.st = newState(c.RunID, e.contractSHA, c.PlanDigest, c.Repos[0].BaseCommit, c.Order, lim)
	for _, ev := range e.events {
		if err := e.st.apply(ev); err != nil {
			return err
		}
	}
	e.work = filepath.Join(e.o.Store.Root, "work", c.RunID)
	return nil
}

// verifyInputs re-reads every stored input against the digests in the contract
// and recomputes the plan digest.
func (e *Engine) verifyInputs() error {
	c := e.c
	files := []struct {
		rec  plan.FileRecord
		want string
	}{{c.Inputs.Plan, c.Inputs.Plan.SHA256}, {c.Inputs.Receipt, c.Plan.ReceiptSHA256}, {c.Inputs.Manifest, c.Plan.ManifestSHA256}}
	for _, f := range files {
		if f.rec.SHA256 != f.want {
			return fmt.Errorf("%w: %s is recorded with two digests", ErrIntegrity, f.rec.Stored)
		}
		if _, err := e.run.ReadArtifact(f.rec.Stored, f.rec.SHA256); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrIntegrity, f.rec.Stored, err)
		}
	}
	pb, _ := e.run.ReadArtifact(c.Inputs.Plan.Stored, c.Inputs.Plan.SHA256)
	split, err := plan.SplitBody(pb)
	if err != nil || split.BodySHA256 != c.Plan.BodySHA256 {
		return fmt.Errorf("%w: the stored plan's approved body does not match the contract", ErrIntegrity)
	}
	if plan.PlanDigest(c.Plan.BodySHA256, c.Plan.ReceiptSHA256, c.Plan.ManifestSHA256) != c.PlanDigest {
		return fmt.Errorf("%w: the plan digest does not match the plan inputs", ErrIntegrity)
	}
	for _, in := range c.Inputs.Execution {
		if _, err := e.run.ReadArtifact(in.Stored, in.SHA256); err != nil {
			return fmt.Errorf("%w: execution input %s: %v", ErrIntegrity, in.ID, err)
		}
	}
	for _, ins := range c.Repos[0].Instructions {
		if ins.Stored == "" {
			continue
		}
		if _, err := e.run.ReadArtifact(ins.Stored, ins.SHA256); err != nil {
			return fmt.Errorf("%w: instruction copy %s: %v", ErrIntegrity, ins.Path, err)
		}
	}
	return nil
}

// parseModel reads "provider/model:effort" and resolves the command without a shell.
func parseModel(spec, provider, command string) (model, error) {
	p, rest, ok := strings.Cut(spec, "/")
	name, effort, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || name == "" || effort == "" {
		return model{}, fmt.Errorf("%w: model %q is not provider/model:effort", ErrIntegrity, spec)
	}
	if p != provider {
		return model{}, fmt.Errorf("%w: model %q: v0.1 binds this role to the %s adapter", ErrIntegrity, spec, provider)
	}
	return model{Provider: p, Model: name, Effort: effort, Bin: command}, nil
}

// Run starts a prepared run.
func (e *Engine) Run(ctx context.Context) (Outcome, error) {
	if e.st.State != contract.RunPrepared {
		return Outcome{State: e.st.State, Reason: e.st.Reason, Exit: contract.ExitFormat},
			fmt.Errorf("run %s is %s, not prepared; continue it with niten resume", e.st.RunID, e.st.State)
	}
	return e.session(ctx, "run")
}

// ResumeOptions are the user's inputs to resume.
type ResumeOptions struct {
	Answers        []byte // the --answers file, nil without one
	MaxInvocations int    // a raised invocation limit, 0 to keep it
	MaxActiveTime  time.Duration
	MaxRepairs     int // a raised repair budget per step, 0 to keep it
}

// Resume recovers the run's attempts, applies the user's answers and limit
// raises, and continues while there is permitted work.
func (e *Engine) Resume(ctx context.Context, r ResumeOptions) (Outcome, error) {
	switch e.st.State {
	case contract.RunPrepared:
		return e.outcome(contract.ExitFormat), fmt.Errorf("run %s has not started; start it with niten run", e.st.RunID)
	case contract.RunFailed:
		return e.outcome(contract.ExitFormat), fmt.Errorf("run %s failed (%s) and cannot be resumed", e.st.RunID, e.st.Reason)
	case contract.RunDone:
		if r.Answers != nil || r.MaxInvocations != 0 || r.MaxActiveTime != 0 || r.MaxRepairs != 0 {
			return e.outcome(contract.ExitFormat), fmt.Errorf("run %s is done; it takes no answers or limits", e.st.RunID)
		}
		return e.outcome(contract.ExitOK), nil
	}
	if err := e.setup(ctx); err != nil {
		return e.outcome(contract.ExitFormat), err
	}
	if out, err := e.recoverAttempts(ctx); err != nil || out != nil {
		if err != nil {
			return e.failure(err)
		}
		return *out, nil
	}
	if err := e.raiseLimits(r); err != nil {
		return e.outcome(contract.ExitFormat), err
	}
	if r.Answers != nil {
		out, err := e.applyAnswers(r.Answers)
		if err != nil || out != nil {
			if out != nil {
				return *out, err
			}
			return e.outcome(contract.ExitFormat), err
		}
	}
	if e.st.State == contract.RunImplemented {
		return e.completeExternal(ctx)
	}
	if out := e.waiting(); out != nil {
		return *out, nil
	}
	return e.session(ctx, "resume")
}

// waiting reports a needs_input state whose cause the answers did not resolve:
// then no session starts, no model is called and no active time accrues.
func (e *Engine) waiting() *Outcome {
	if e.st.State != contract.RunNeedsInput {
		return nil
	}
	switch {
	case e.st.Gate != nil:
		o := e.outcome(contract.ExitNeedsInput)
		return &o
	case len(e.st.openQuestions()) > 0:
		o := e.outcome(contract.ExitNeedsInput)
		return &o
	}
	return nil
}

func (e *Engine) outcome(exit contract.ExitCode) Outcome {
	return Outcome{State: e.st.State, Reason: e.st.Reason, Detail: e.st.Detail, Exit: exit}
}

// session opens a working session: the verifier sandbox is certified and the
// clone opened or created before anything is recorded or any model started.
func (e *Engine) session(ctx context.Context, command string) (Outcome, error) {
	if e.verifier == nil {
		if err := e.setup(ctx); err != nil {
			return e.outcome(contract.ExitFormat), err
		}
	}
	if err := e.emit(evSession, sessionData{Command: command, ContractSHA256: e.contractSHA}); err != nil {
		return e.failure(err)
	}
	if e.st.CloneWork == "" {
		if err := e.createClone(ctx); err != nil {
			return e.failure(err)
		}
	}
	if err := e.emit(evRunState, runStateData{State: contract.RunRunning}); err != nil {
		return e.failure(err)
	}
	e.logf("run %s: %s (invocations %d of %d)", e.st.RunID, command, e.st.Invocations, e.st.Limits.MaxInvocations)
	out, err := e.drive(ctx)
	if err != nil {
		return e.failure(err)
	}
	return out, nil
}

// failure records an engine error when the journal still accepts it. Corrupt
// or tampered run files fail the run for good; any other error (a git or file
// system failure, say) pauses it, so the user can resume after fixing the cause.
func (e *Engine) failure(err error) (Outcome, error) {
	st, reason := contract.RunPaused, "engine_error"
	if errors.Is(err, store.ErrCorrupt) || errors.Is(err, ErrIntegrity) {
		st, reason = contract.RunFailed, "integrity"
	}
	if e.run.Broken() == nil && e.st.State != contract.RunFailed {
		_ = e.emit(evRunState, runStateData{State: st, Reason: reason, Detail: []string{err.Error()}})
	}
	return Outcome{State: e.st.State, Reason: e.st.Reason, Detail: e.st.Detail, Exit: contract.ExitFormat}, err
}

// stop records a state the run waits in and ends the session.
func (e *Engine) stop(st contract.RunState, reason string, detail []string, exit contract.ExitCode) (*Outcome, error) {
	if err := e.emit(evRunState, runStateData{State: st, Reason: reason, Detail: detail}); err != nil {
		return nil, err
	}
	e.logf("run %s: %s (%s) %s", e.st.RunID, st, reason, strings.Join(detail, "; "))
	return &Outcome{State: st, Reason: reason, Detail: detail, Exit: exit}, nil
}

// observe folds every appended event, the engine's own and those of the
// attempt and verify packages, into the projection.
func (e *Engine) observe(ev store.Event) error {
	e.events = append(e.events, ev)
	return e.st.apply(ev)
}

// emit appends one event and saves the projection.
func (e *Engine) emit(typ string, data any) error {
	if _, err := e.run.Append(typ, data); err != nil {
		return err
	}
	return e.run.SaveState(e.st)
}

func (e *Engine) logf(format string, a ...any) {
	fmt.Fprintf(e.o.Out, format+"\n", a...)
}

// drive runs the sequential loop until the run waits, fails or completes.
func (e *Engine) drive(ctx context.Context) (Outcome, error) {
	for {
		if ctx.Err() != nil {
			out, err := e.stop(contract.RunPaused, "interrupted", []string{"stopped by the user"}, contract.ExitInterrupted)
			if err != nil {
				return Outcome{}, err
			}
			return *out, nil
		}
		u, gate := e.current()
		var out *Outcome
		var err error
		switch {
		case gate:
			out, err = e.openGate(u)
		case u == nil:
			out, err = e.finish(ctx)
		default:
			out, err = e.advance(ctx, u)
		}
		if err != nil {
			return Outcome{}, err
		}
		if out != nil {
			return *out, nil
		}
	}
}

// current returns the unit with work, or a step whose gate is not released,
// or nil when the final stage is accepted.
func (e *Engine) current() (*StepView, bool) {
	for _, u := range e.st.Steps {
		if u.State != contract.StepAccepted {
			return u, false
		}
		if e.c.GatePerStep && !e.released(gateID(u)) {
			return u, true
		}
	}
	if e.st.Final.State != contract.StepAccepted {
		return e.st.Final, false
	}
	return nil, false
}

func (e *Engine) released(id string) bool {
	for _, g := range e.st.Released {
		if g == id {
			return true
		}
	}
	return false
}

// gateID binds a gate to its step and the exact accepted candidate.
func gateID(u *StepView) string {
	return "gate-" + strings.ToLower(u.ID) + "-" + short(u.AcceptedAt)
}

func (e *Engine) openGate(u *StepView) (*Outcome, error) {
	id := gateID(u)
	if e.st.Gate == nil || e.st.Gate.ID != id {
		if err := e.emit(evGateOpened, GateView{ID: id, Unit: u.ID, Candidate: u.AcceptedAt}); err != nil {
			return nil, err
		}
	}
	return e.stop(contract.RunNeedsInput, "step_gate", []string{fmt.Sprintf("%s was accepted at %s; continue with a step_continue answer for gate %s", u.ID, u.AcceptedAt, id)}, contract.ExitNeedsInput)
}

// advance performs the next piece of work of a unit.
func (e *Engine) advance(ctx context.Context, u *StepView) (*Outcome, error) {
	switch u.State {
	case contract.StepPending:
		if u.ID == FinalUnit {
			return nil, e.startFinal(ctx)
		}
		if out, err := e.reserve(); out != nil || err != nil {
			return out, err
		}
		e.logf("%s: start at %s", u.ID, short(e.st.Head))
		return nil, e.emit(evStepState, transition{Unit: u.ID, Step: &stepStateData{State: contract.StepImplementing, Start: e.st.Head, Repairs: 0, Problems: []string{}}})
	case contract.StepImplementing:
		return e.executorTurn(ctx, u)
	case contract.StepCandidate:
		return e.evaluate(ctx, u)
	case contract.StepReviewing:
		return e.reviewerTurn(ctx, u)
	case contract.StepChangesRequested:
		if u.Repairs >= e.st.Limits.MaxRepairsPerStep {
			return e.stop(contract.RunNeedsInput, "repair_limit", e.disputeSummary(u), contract.ExitNeedsInput)
		}
		return nil, e.emit(evStepState, transition{Unit: u.ID, Step: &stepStateData{State: contract.StepImplementing, Repairs: u.Repairs + 1, Problems: u.Problems}})
	}
	return nil, fmt.Errorf("unit %s is in state %s, which the sequential engine does not drive", u.ID, u.State)
}

// disputeSummary is the brief needs_input report after the repair budget of a unit is spent.
func (e *Engine) disputeSummary(u *StepView) []string {
	out := []string{fmt.Sprintf("%s used %d of %d repair cycles without acceptance", u.ID, u.Repairs, e.st.Limits.MaxRepairsPerStep)}
	for _, f := range e.st.Findings {
		if f.Unit != u.ID || f.State != contract.FindingOpen {
			continue
		}
		line := fmt.Sprintf("%s %s %s: %s", f.FindingID, f.Severity, f.Location.Path, f.DefectScenario)
		if n := len(f.Responses); n > 0 {
			line += fmt.Sprintf(" (executor: %s)", f.Responses[n-1].Disposition)
		}
		out = append(out, line)
	}
	out = append(out, u.Problems...)
	return out
}

// Budget.

func (e *Engine) maxActive() time.Duration {
	d, _ := time.ParseDuration(e.st.Limits.MaxActiveTime)
	return d
}

// activeNow is the active time including the open session up to now.
func (e *Engine) activeNow() time.Duration {
	a := time.Duration(e.st.ActiveMS) * time.Millisecond
	if e.st.sessionOpen && !e.st.lastTime.IsZero() {
		if since := e.o.Now().Sub(e.st.lastTime); since > 0 {
			a += since
		}
	}
	return a
}

func (e *Engine) remaining() (int, time.Duration) {
	return e.st.Limits.MaxInvocations - e.st.Invocations, e.maxActive() - e.activeNow()
}

// budget refuses a model call when the invocations or the active time are spent.
func (e *Engine) budget() (*Outcome, error) {
	inv, t := e.remaining()
	switch {
	case inv < 1:
		return e.stop(contract.RunPaused, "limit", []string{fmt.Sprintf("invocations: %d of %d used; raise with resume --max-invocations", e.st.Invocations, e.st.Limits.MaxInvocations)}, contract.ExitPaused)
	case t <= 0:
		return e.stop(contract.RunPaused, "limit", []string{fmt.Sprintf("active time: %s of %s used; raise with resume --max-time", e.activeNow().Round(time.Second), e.st.Limits.MaxActiveTime)}, contract.ExitPaused)
	}
	return nil, nil
}

// reserve refuses to start a new step when the remainder would not leave the
// final reserve: a step needs at least an implementation and a review.
func (e *Engine) reserve() (*Outcome, error) {
	inv, t := e.remaining()
	rt, _ := time.ParseDuration(e.st.Limits.FinalReserveTime)
	need := e.st.Limits.FinalReserveInvocations + 2
	if inv < need || t <= rt {
		return e.stop(contract.RunPaused, "limit", []string{fmt.Sprintf("final reserve: a new step needs %d invocations and more than %s of active time; %d invocations and %s remain", need, rt, inv, t.Round(time.Second))}, contract.ExitPaused)
	}
	return nil, nil
}

// deadline of the next model call.
func (e *Engine) deadline() time.Time {
	d, _ := time.ParseDuration(e.st.Limits.InvocationDeadline)
	if _, t := e.remaining(); t < d {
		d = t
	}
	return e.o.Now().Add(d)
}

// raiseLimits records the user's limit raises as a separate event.
func (e *Engine) raiseLimits(r ResumeOptions) error {
	lim := e.st.Limits
	var changes []LimitChange
	if r.MaxInvocations != 0 {
		if r.MaxInvocations <= lim.MaxInvocations {
			return fmt.Errorf("--max-invocations %d does not raise the limit %d", r.MaxInvocations, lim.MaxInvocations)
		}
		changes = append(changes, LimitChange{Field: "max_invocations", Previous: fmt.Sprint(lim.MaxInvocations), New: fmt.Sprint(r.MaxInvocations)})
		lim.MaxInvocations = r.MaxInvocations
	}
	if r.MaxActiveTime != 0 {
		if r.MaxActiveTime <= e.maxActive() {
			return fmt.Errorf("--max-time %s does not raise the limit %s", r.MaxActiveTime, lim.MaxActiveTime)
		}
		changes = append(changes, LimitChange{Field: "max_active_time", Previous: lim.MaxActiveTime, New: r.MaxActiveTime.String()})
		lim.MaxActiveTime = r.MaxActiveTime.String()
	}
	if r.MaxRepairs != 0 {
		if r.MaxRepairs <= lim.MaxRepairsPerStep {
			return fmt.Errorf("--max-repairs %d does not raise the limit %d", r.MaxRepairs, lim.MaxRepairsPerStep)
		}
		changes = append(changes, LimitChange{Field: "max_repairs_per_step", Previous: fmt.Sprint(lim.MaxRepairsPerStep), New: fmt.Sprint(r.MaxRepairs)})
		lim.MaxRepairsPerStep = r.MaxRepairs
	}
	if len(changes) == 0 {
		return nil
	}
	return e.emit(evLimits, limitsData{Changes: changes, Limits: lim})
}

// Setup.

// setup certifies the verifier sandbox, opens the clone and binds the verifier.
func (e *Engine) setup(ctx context.Context) error {
	if err := privateDir(filepath.Join(e.o.Store.Root, "work")); err != nil {
		return err
	}
	if err := privateDir(e.work); err != nil {
		return err
	}
	backend, err := e.o.NewBackend(filepath.Join(e.work, "profiles"))
	if err != nil {
		return fmt.Errorf("verifier sandbox: %w", err)
	}
	if err := backend.SelfTest(ctx); err != nil {
		return fmt.Errorf("verifier sandbox: %w", err)
	}
	toolchains, version, err := e.toolchains(ctx)
	if err != nil {
		return err
	}
	for _, m := range []*model{&e.exec, &e.rev} {
		bin, err := lookCommand(m.Bin, e.o.Getenv("PATH"))
		if err != nil {
			return err
		}
		m.Bin = bin
	}
	if e.st.CloneWork != "" {
		c, err := workspace.OpenClone(e.st.CloneWork, e.st.CloneGitDir)
		if err != nil {
			return fmt.Errorf("%w: the run's clone: %v", ErrIntegrity, err)
		}
		head, err := c.Head(ctx)
		if err != nil {
			return err
		}
		if head != e.st.Head {
			if !e.advancedByPendingTurn(ctx, c, head) {
				return fmt.Errorf("%w: the clone's branch points at %s, the journal at %s", ErrIntegrity, head, e.st.Head)
			}
			// The turn's own commit, not yet in the journal: move the branch
			// back (compare-and-swap) so the metadata matches the journal; the
			// turn's processing prepares the same commit again.
			if err := c.Advance(ctx, workspace.Candidate{Commit: e.st.Head, Parent: head}); err != nil {
				return err
			}
			if fp, err := c.MetadataFingerprint(); err != nil || fp != e.st.Metadata {
				return fmt.Errorf("%w: the clone's git metadata differs from the journal after rewinding %s", ErrIntegrity, short(head))
			}
		}
		e.clone = c
	}
	e.verifier = &verify.Verifier{Backend: backend, Store: e.run, Root: filepath.Join(e.work, "checks"), Toolchains: toolchains,
		ToolVersion: version, Now: e.o.Now}
	if e.clone != nil {
		e.verifier.Clone = e.clone
	}
	return nil
}

// advancedByPendingTurn recognizes a crash between moving the branch and
// recording the candidate: an unprocessed executor turn started at the
// journal's head, and the branch is one Niten commit of that turn ahead.
func (e *Engine) advancedByPendingTurn(ctx context.Context, c *workspace.Clone, head string) bool {
	parent, err := c.Parent(ctx, head)
	if err != nil || parent != e.st.Head {
		return false
	}
	for _, t := range e.st.Turns {
		if t.Outcome == "" && t.Role == string(contract.RoleExecutor) && t.Base == e.st.Head {
			subject, err := c.Subject(ctx, head)
			return err == nil && subject == "niten: "+t.ID
		}
	}
	return false
}

// createClone makes the run's owned clone at the base commit.
func (e *Engine) createClone(ctx context.Context) error {
	repo := e.c.Repos[0]
	c, err := workspace.CreateClone(ctx, repo.Path, repo.BaseCommit, filepath.Join(e.work, "clone"), filepath.Join(e.work, "gitdir"))
	if err != nil {
		return fmt.Errorf("create the run's clone: %w", err)
	}
	tree, err := c.TreeOf(ctx, repo.BaseCommit)
	if err != nil {
		return err
	}
	if tree != repo.BaseTree {
		return fmt.Errorf("%w: base commit %s has tree %s, prepare recorded %s", ErrIntegrity, repo.BaseCommit, tree, repo.BaseTree)
	}
	fp, err := c.MetadataFingerprint()
	if err != nil {
		return err
	}
	e.clone, e.verifier.Clone = c, c
	return e.emit(evClone, cloneData{Work: c.Work, GitDir: c.GitDir, Head: repo.BaseCommit, Metadata: fp})
}

func privateDir(p string) error {
	if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a real directory", p)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must be private to its owner (mode %o)", p, fi.Mode().Perm())
	}
	return nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func digestJSON(v any) string {
	b, _ := json.Marshal(v)
	return digest(b)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
