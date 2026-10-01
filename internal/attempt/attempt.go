// Package attempt runs one model CLI invocation under a durable protocol and
// recovers attempts a crashed coordinator left behind.
//
// Protocol of one attempt, each record written before the next step:
//
//	attempts/<id>/prompt        artifact: the prompt sent on stdin
//	attempt.intent              event: role, argv, digests, environment names, stream paths
//	attempts/<id>/stdout.jsonl  created exclusively by the supervisor, then the process starts
//	attempt.started             event: PID, kernel start time, process group (before waiting)
//	attempts/<id>/outcome.json  artifact: what happened to the process
//	attempts/<id>/result.json   artifact: the parsed result or the classified failure
//	attempt.finished            event: digests of both artifacts
//
// A missing step after a crash is resolved by Recover, never by running the
// model again: a complete saved stream is parsed, otherwise the attempt is
// recorded as outcome_unknown.
package attempt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/killabayte/niten/internal/provider"
	"github.com/killabayte/niten/internal/store"
)

// Event types.
const (
	EvIntent     = "attempt.intent"
	EvStarted    = "attempt.started"
	EvFinished   = "attempt.finished"
	EvRecovered  = "attempt.recovered"
	EvUnknown    = "attempt.outcome_unknown"
	EvNotStarted = "attempt.not_started"
)

// ParseFunc turns saved streams into a result; it is the role's adapter parser.
type ParseFunc func(stdoutPath, stderrPath string, o provider.Outcome) (*provider.Result, *provider.Error)

// Spec is one attempt.
type Spec struct {
	ID       string
	Role     string
	Bin      string
	Args     []string
	Env      []string // filtered child environment (provider.FilterEnv)
	Stripped []string // names FilterEnv removed; recorded, never values
	Dir      string
	Stdin    []byte
	Deadline time.Time
	Parse    ParseFunc
}

type intent struct {
	ID        string   `json:"id"`
	Role      string   `json:"role"`
	Bin       string   `json:"bin"`
	Args      []string `json:"args"`
	ArgvSHA   string   `json:"argv_sha256"`
	PromptRef string   `json:"prompt_ref"`
	PromptSHA string   `json:"prompt_sha256"`
	EnvNames  []string `json:"env_names"`
	Stripped  []string `json:"stripped_env"`
	Dir       string   `json:"dir"`
	StdoutRef string   `json:"stdout_ref"`
	StderrRef string   `json:"stderr_ref"`
	Deadline  string   `json:"deadline,omitempty"`
}

type started struct {
	ID       string            `json:"id"`
	Identity provider.Identity `json:"identity"`
}

type finished struct {
	ID         string `json:"id"`
	OK         bool   `json:"ok"`
	Class      string `json:"class,omitempty"`
	OutcomeRef string `json:"outcome_ref"`
	OutcomeSHA string `json:"outcome_sha256"`
	ResultRef  string `json:"result_ref"`
	ResultSHA  string `json:"result_sha256"`
}

type resolved struct {
	ID        string `json:"id"`
	Reason    string `json:"reason"`
	ResultRef string `json:"result_ref,omitempty"`
	ResultSHA string `json:"result_sha256,omitempty"`
	Remains   []int  `json:"remaining_pids,omitempty"`
}

// stored is the result artifact.
type stored struct {
	Result *provider.Result `json:"result,omitempty"`
	Error  *provider.Error  `json:"error,omitempty"`
}

var reID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Runner runs attempts of one open run.
type Runner struct {
	Store *store.Run
}

// Run performs one attempt. A returned error means the protocol could not be
// followed (a store write failed, the process could not be started or its
// start not recorded); then nothing about the attempt is claimed. Otherwise
// exactly one of the result and the classified failure is set.
func (r *Runner) Run(ctx context.Context, s Spec) (*provider.Result, *provider.Error, error) {
	if !reID.MatchString(s.ID) {
		return nil, nil, fmt.Errorf("invalid attempt id %q", s.ID)
	}
	if s.Parse == nil {
		return nil, nil, errors.New("attempt has no parser")
	}
	dir := "attempts/" + s.ID
	p, err := r.Store.Path(dir)
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Lstat(p); err == nil {
		return nil, nil, fmt.Errorf("attempt %s already exists; attempts never reuse files", s.ID)
	}
	promptRef := dir + "/prompt"
	promptSHA, err := r.Store.WriteArtifact(promptRef, s.Stdin, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("store the prompt: %w", err)
	}
	stdoutRef, stderrRef := dir+"/stdout.jsonl", dir+"/stderr.log"
	stdoutPath, _ := r.Store.Path(stdoutRef)
	stderrPath, _ := r.Store.Path(stderrRef)
	in := intent{ID: s.ID, Role: s.Role, Bin: s.Bin, Args: s.Args, ArgvSHA: digestJSON(append([]string{s.Bin}, s.Args...)),
		PromptRef: promptRef, PromptSHA: promptSHA, EnvNames: names(s.Env), Stripped: s.Stripped, Dir: s.Dir,
		StdoutRef: stdoutRef, StderrRef: stderrRef}
	if !s.Deadline.IsZero() {
		in.Deadline = s.Deadline.UTC().Format(time.RFC3339Nano)
	}
	if _, err := r.Store.Append(EvIntent, in); err != nil {
		return nil, nil, err
	}
	out, err := provider.Supervise(ctx, provider.Spec{Bin: s.Bin, Args: s.Args, Env: s.Env, Dir: s.Dir, Stdin: s.Stdin,
		StdoutPath: stdoutPath, StderrPath: stderrPath, Deadline: s.Deadline,
		OnStart: func(id provider.Identity) error {
			_, err := r.Store.Append(EvStarted, started{ID: s.ID, Identity: id})
			return err
		}})
	if err != nil {
		return nil, nil, err
	}
	res, perr := s.Parse(stdoutPath, stderrPath, out)
	return res, perr, r.finish(s.ID, out, res, perr)
}

func (r *Runner) finish(id string, out provider.Outcome, res *provider.Result, perr *provider.Error) error {
	dir := "attempts/" + id
	ob, _ := json.MarshalIndent(out, "", " ")
	osha, err := r.Store.WriteArtifact(dir+"/outcome.json", append(ob, '\n'), 0o600)
	if err != nil {
		return fmt.Errorf("store the outcome: %w", err)
	}
	rb, _ := json.MarshalIndent(stored{Result: res, Error: perr}, "", " ")
	rsha, err := r.Store.WriteArtifact(dir+"/result.json", append(rb, '\n'), 0o600)
	if err != nil {
		return fmt.Errorf("store the result: %w", err)
	}
	f := finished{ID: id, OK: res != nil, OutcomeRef: dir + "/outcome.json", OutcomeSHA: osha, ResultRef: dir + "/result.json", ResultSHA: rsha}
	if perr != nil {
		f.Class = string(perr.Class)
	}
	_, err = r.Store.Append(EvFinished, f)
	return err
}

// Status of an attempt after recovery.
type Status string

const (
	StatusFinished   Status = "finished"        // the protocol completed before the crash
	StatusRecovered  Status = "recovered"       // the saved stream held a complete result; no model was called
	StatusUnknown    Status = "outcome_unknown" // what the attempt did cannot be established
	StatusNotStarted Status = "not_started"     // the process provably never started
)

// Recovered is one attempt's state after Recover.
type Recovered struct {
	ID      string
	Role    string
	Status  Status
	Result  *provider.Result
	Err     *provider.Error
	Reason  string
	Remains []int // processes left in a recorded group, never signalled
}

// Blocking reports whether the run must not continue: processes of unproven
// ownership remain from the attempt.
func (r Recovered) Blocking() bool { return len(r.Remains) > 0 }

// Recover resolves every attempt of the replayed journal. Finished attempts
// are read back and checked against their digests. An attempt that started
// but did not finish is stopped if its recorded process is still alive with
// the recorded identity; processes left in its group whose ownership is not
// provable are reported, never signalled. Its saved stream is then parsed
// with the role's parser: a complete result is recovered without a new model
// call, anything else is outcome_unknown. An attempt with only an intent is
// not_started when its stream files were never created. Each resolution is
// written to the journal, so recovery is not repeated.
func Recover(run *store.Run, events []store.Event, parsers map[string]ParseFunc, grace time.Duration) ([]Recovered, error) {
	type state struct {
		in       *intent
		st       *started
		fin      *finished
		resolved string
		res      *resolved
	}
	byID := map[string]*state{}
	var order []string
	get := func(id string) *state {
		if byID[id] == nil {
			byID[id] = &state{}
			order = append(order, id)
		}
		return byID[id]
	}
	for _, ev := range events {
		switch ev.Type {
		case EvIntent:
			var v intent
			if err := json.Unmarshal(ev.Data, &v); err != nil {
				return nil, fmt.Errorf("%w: event %d: %v", store.ErrCorrupt, ev.Seq, err)
			}
			get(v.ID).in = &v
		case EvStarted:
			var v started
			if err := json.Unmarshal(ev.Data, &v); err != nil {
				return nil, fmt.Errorf("%w: event %d: %v", store.ErrCorrupt, ev.Seq, err)
			}
			get(v.ID).st = &v
		case EvFinished:
			var v finished
			if err := json.Unmarshal(ev.Data, &v); err != nil {
				return nil, fmt.Errorf("%w: event %d: %v", store.ErrCorrupt, ev.Seq, err)
			}
			get(v.ID).fin = &v
		case EvRecovered, EvUnknown, EvNotStarted:
			var v resolved
			if err := json.Unmarshal(ev.Data, &v); err != nil {
				return nil, fmt.Errorf("%w: event %d: %v", store.ErrCorrupt, ev.Seq, err)
			}
			s := get(v.ID)
			s.resolved, s.res = ev.Type, &v
		}
	}
	var out []Recovered
	for _, id := range order {
		s := byID[id]
		if s.in == nil {
			return nil, fmt.Errorf("%w: attempt %s has events but no intent", store.ErrCorrupt, id)
		}
		rec := Recovered{ID: id, Role: s.in.Role}
		switch {
		case s.fin != nil:
			st, err := readStored(run, s.fin.ResultRef, s.fin.ResultSHA)
			if err != nil {
				return nil, err
			}
			rec.Status, rec.Result, rec.Err = StatusFinished, st.Result, st.Error
		case s.resolved == EvRecovered:
			st, err := readStored(run, s.res.ResultRef, s.res.ResultSHA)
			if err != nil {
				return nil, err
			}
			rec.Status, rec.Result, rec.Err, rec.Reason = StatusRecovered, st.Result, st.Error, s.res.Reason
		case s.resolved == EvUnknown:
			rec.Status, rec.Reason, rec.Remains = StatusUnknown, s.res.Reason, s.res.Remains
			if len(rec.Remains) > 0 {
				// The remaining processes may have exited since: resolve again
				// and record the new answer once nothing blocks.
				rec = resolveStarted(run, s.in, s.st, parsers, grace)
				if !rec.Blocking() {
					if err := record(run, rec); err != nil {
						return nil, err
					}
				}
			}
		case s.resolved == EvNotStarted:
			rec.Status, rec.Reason = StatusNotStarted, s.res.Reason
		default:
			if s.st != nil {
				rec = resolveStarted(run, s.in, s.st, parsers, grace)
			} else {
				rec = resolveIntentOnly(run, s.in, parsers)
			}
			if err := record(run, rec); err != nil {
				return nil, err
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

func resolveStarted(run *store.Run, in *intent, st *started, parsers map[string]ParseFunc, grace time.Duration) Recovered {
	rec := Recovered{ID: in.ID, Role: in.Role}
	if st != nil {
		err := provider.TerminateRecorded(st.Identity, grace)
		var remains *provider.GroupRemainsError
		if errors.As(err, &remains) {
			rec.Status, rec.Remains = StatusUnknown, remains.PIDs
			rec.Reason = "processes of the attempt's group remain and their ownership cannot be proven; the run must not continue until they are gone"
			return rec
		}
	}
	return parseSaved(run, in, st, parsers, rec)
}

func resolveIntentOnly(run *store.Run, in *intent, parsers map[string]ParseFunc) Recovered {
	rec := Recovered{ID: in.ID, Role: in.Role}
	so, _ := run.Path(in.StdoutRef)
	se, _ := run.Path(in.StderrRef)
	_, e1 := os.Lstat(so)
	_, e2 := os.Lstat(se)
	if errors.Is(e1, fs.ErrNotExist) && errors.Is(e2, fs.ErrNotExist) {
		rec.Status, rec.Reason = StatusNotStarted, "the stream files were never created, so the process never started"
		return rec
	}
	return parseSaved(run, in, nil, parsers, rec)
}

func parseSaved(run *store.Run, in *intent, st *started, parsers map[string]ParseFunc, rec Recovered) Recovered {
	parse := parsers[in.Role]
	if parse == nil {
		rec.Status, rec.Reason = StatusUnknown, "no parser for role "+in.Role
		return rec
	}
	so, _ := run.Path(in.StdoutRef)
	se, _ := run.Path(in.StderrRef)
	o := provider.Outcome{Exit: 0}
	if st != nil {
		o.Identity = st.Identity
		o.Started = time.UnixMicro(st.Identity.StartMicros)
	}
	res, perr := parse(so, se, o)
	if res == nil {
		reason := "the saved stream holds no complete result"
		if perr != nil {
			reason += ": " + perr.Error()
		}
		rec.Status, rec.Reason = StatusUnknown, reason
		return rec
	}
	res.Degraded = append(res.Degraded, "recovered from the saved stream after a coordinator crash; the exit status is unknown")
	rec.Status, rec.Result, rec.Reason = StatusRecovered, res, "complete result in the saved stream"
	return rec
}

func record(run *store.Run, rec Recovered) error {
	v := resolved{ID: rec.ID, Reason: rec.Reason, Remains: rec.Remains}
	typ := EvUnknown
	switch rec.Status {
	case StatusRecovered:
		typ = EvRecovered
		b, _ := json.MarshalIndent(stored{Result: rec.Result}, "", " ")
		ref := "attempts/" + rec.ID + "/recovered-result.json"
		sha, err := run.WriteArtifact(ref, append(b, '\n'), 0o600)
		if err != nil {
			return err
		}
		v.ResultRef, v.ResultSHA = ref, sha
	case StatusNotStarted:
		typ = EvNotStarted
	}
	_, err := run.Append(typ, v)
	return err
}

func readStored(run *store.Run, ref, sha string) (stored, error) {
	var st stored
	b, err := run.ReadArtifact(ref, sha)
	if err != nil {
		return st, fmt.Errorf("%w: %v", store.ErrCorrupt, err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%w: %s: %v", store.ErrCorrupt, ref, err)
	}
	return st, nil
}

func names(env []string) []string {
	var out []string
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out = append(out, kv[:i])
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func digestJSON(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
