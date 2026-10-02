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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/killabayte/niten/internal/holders"
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
	// The streams the result was parsed from, as they are after the process.
	StdoutRef string `json:"stdout_ref,omitempty"`
	StdoutSHA string `json:"stdout_sha256,omitempty"`
	StderrRef string `json:"stderr_ref,omitempty"`
	StderrSHA string `json:"stderr_sha256,omitempty"`
}

// streamDigests records the digests of an attempt's stream files.
func streamDigests(run *store.Run, f *finished, stdoutRef, stderrRef string) error {
	var err error
	f.StdoutRef, f.StdoutSHA, f.StderrRef, f.StderrSHA, err = digestStreams(run, stdoutRef, stderrRef)
	return err
}

func digestStreams(run *store.Run, stdoutRef, stderrRef string) (string, string, string, string, error) {
	var sums [2]string
	for i, ref := range []string{stdoutRef, stderrRef} {
		b, err := run.ReadArtifact(ref, "")
		if err != nil {
			return "", "", "", "", fmt.Errorf("digest the stream %s: %w", ref, err)
		}
		sums[i] = digest(b)
	}
	return stdoutRef, sums[0], stderrRef, sums[1], nil
}

type resolved struct {
	ID        string `json:"id"`
	Reason    string `json:"reason"`
	ResultRef string `json:"result_ref,omitempty"`
	ResultSHA string `json:"result_sha256,omitempty"`
	// The streams a recovered result was parsed from.
	StdoutRef string `json:"stdout_ref,omitempty"`
	StdoutSHA string `json:"stdout_sha256,omitempty"`
	StderrRef string `json:"stderr_ref,omitempty"`
	StderrSHA string `json:"stderr_sha256,omitempty"`
	Remains   []int  `json:"remaining_pids,omitempty"`
	Blocked   bool   `json:"blocked,omitempty"`
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
	if err := streamDigests(r.Store, &f, dir+"/stdout.jsonl", dir+"/stderr.log"); err != nil {
		return err
	}
	_, err = r.Store.Append(EvFinished, f)
	return err
}

// Status of an attempt after recovery.
type Status string

const (
	StatusFinished   Status = "finished"        // the protocol completed, before the crash or from its saved artifacts
	StatusRecovered  Status = "recovered"       // the process died with the coordinator; its saved stream held a complete result
	StatusUnknown    Status = "outcome_unknown" // what the attempt did cannot be established
	StatusNotStarted Status = "not_started"     // the process provably never started
)

// Recovered is one attempt's state after Recover.
type Recovered struct {
	ID     string
	Role   string
	Status Status
	Result *provider.Result
	Err    *provider.Error
	Reason string
	// Remains lists processes that hold the attempt's directory or are left in
	// its recorded group; their ownership is not provable, so they are never
	// signalled. Blocked is also set when they could not be listed.
	Remains []int
	Blocked bool
}

// Blocking reports whether the run must not continue: something may still be
// working on the attempt's directory.
func (r Recovered) Blocking() bool { return r.Blocked || len(r.Remains) > 0 }

// Recover resolves every attempt of the replayed journal and records each
// answer, so it is not repeated:
//
//   - finished attempts are read back; both artifacts must match their digests;
//   - an attempt whose outcome artifact was saved before the crash is
//     completed from it: the known outcome decides, never a guess;
//   - an attempt that started but saved no outcome has its recorded process
//     stopped if it is still alive with the recorded start time; then any
//     process still holding the attempt's directory (or left in a group whose
//     leader is gone) blocks the run, and otherwise the saved stream is parsed:
//     a complete result is recovered without a new model call, anything else
//     is outcome_unknown;
//   - an attempt with only an intent never yields a result (its prompt is
//     delivered only after attempt.started), is not_started when its stream
//     files were never created, and blocks the run while anything holds its
//     directory.
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
		var target any
		var id *string
		switch ev.Type {
		case EvIntent:
			v := &intent{}
			target, id = v, &v.ID
		case EvStarted:
			v := &started{}
			target, id = v, &v.ID
		case EvFinished:
			v := &finished{}
			target, id = v, &v.ID
		case EvRecovered, EvUnknown, EvNotStarted:
			v := &resolved{}
			target, id = v, &v.ID
		default:
			continue
		}
		if err := json.Unmarshal(ev.Data, target); err != nil {
			return nil, fmt.Errorf("%w: event %d: %v", store.ErrCorrupt, ev.Seq, err)
		}
		s := get(*id)
		switch v := target.(type) {
		case *intent:
			s.in = v
		case *started:
			s.st = v
		case *finished:
			s.fin = v
		case *resolved:
			s.resolved, s.res = ev.Type, v
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
			if _, err := run.ReadArtifact(s.fin.OutcomeRef, s.fin.OutcomeSHA); err != nil {
				return nil, fmt.Errorf("%w: attempt %s outcome: %v", store.ErrCorrupt, id, err)
			}
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
		case s.resolved == EvNotStarted:
			rec.Status, rec.Reason = StatusNotStarted, s.res.Reason
		case s.resolved == EvUnknown && !s.res.Blocked && len(s.res.Remains) == 0:
			rec.Status, rec.Reason = StatusUnknown, s.res.Reason
		default:
			// Unresolved, or recorded as blocked: resolve (again).
			var err error
			if rec, err = resolve(run, s.in, s.st, parsers, grace); err != nil {
				return nil, err
			}
			prevBlocked := s.res != nil && (s.res.Blocked || len(s.res.Remains) > 0)
			if rec.Status != StatusFinished && !(prevBlocked && rec.Blocking()) {
				if err := record(run, rec); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

// resolve decides an attempt that has no finished event.
func resolve(run *store.Run, in *intent, st *started, parsers map[string]ParseFunc, grace time.Duration) (Recovered, error) {
	rec := Recovered{ID: in.ID, Role: in.Role}
	dir := "attempts/" + in.ID
	if st != nil {
		ob, err := run.ReadArtifact(dir+"/outcome.json", "")
		switch {
		case err == nil:
			return completeFromArtifacts(run, in, ob, parsers)
		case !errors.Is(err, fs.ErrNotExist):
			// The outcome may exist but cannot be read: what happened is
			// recorded and unknown to us, so nothing is guessed.
			return rec, fmt.Errorf("%w: attempt %s: the saved outcome cannot be read: %v", store.ErrCorrupt, in.ID, err)
		}
		err = provider.TerminateRecorded(st.Identity, grace)
		var remains *provider.GroupRemainsError
		if errors.As(err, &remains) {
			rec.Status, rec.Remains = StatusUnknown, remains.PIDs
			rec.Reason = "processes of the attempt's group remain and their ownership cannot be proven"
			return rec, nil
		}
	}
	if pids, err := holdersOf(in.Dir); err != nil || len(pids) > 0 {
		rec.Status, rec.Remains, rec.Blocked = StatusUnknown, pids, true
		rec.Reason = "the attempt's directory is still held by processes whose ownership cannot be proven"
		if err != nil {
			rec.Reason = "the holders of the attempt's directory could not be listed: " + err.Error()
		}
		return rec, nil
	}
	if st == nil {
		so, _ := run.Path(in.StdoutRef)
		se, _ := run.Path(in.StderrRef)
		_, e1 := os.Lstat(so)
		_, e2 := os.Lstat(se)
		for _, e := range []error{e1, e2} {
			if e != nil && !errors.Is(e, fs.ErrNotExist) {
				return rec, fmt.Errorf("attempt %s: the stream files cannot be checked: %w", in.ID, e)
			}
		}
		if errors.Is(e1, fs.ErrNotExist) && errors.Is(e2, fs.ErrNotExist) {
			rec.Status, rec.Reason = StatusNotStarted, "the stream files were never created, so the process never started"
		} else {
			rec.Status, rec.Reason = StatusUnknown, "the start was never recorded, so the process never received its prompt; its stream is not a result"
		}
		return rec, nil
	}
	return parseSaved(run, in, st, parsers, rec), nil
}

// completeFromArtifacts finishes the protocol from the outcome (and result)
// the attempt saved before the crash. The saved outcome is what happened; the
// stream is parsed against it, never against an assumed clean exit.
func completeFromArtifacts(run *store.Run, in *intent, outcomeBytes []byte, parsers map[string]ParseFunc) (Recovered, error) {
	rec := Recovered{ID: in.ID, Role: in.Role, Status: StatusFinished, Reason: "completed from the artifacts saved before the crash"}
	dir := "attempts/" + in.ID
	var o provider.Outcome
	if err := json.Unmarshal(outcomeBytes, &o); err != nil {
		return rec, fmt.Errorf("%w: %s/outcome.json: %v", store.ErrCorrupt, dir, err)
	}
	var st stored
	resultBytes, err := run.ReadArtifact(dir+"/result.json", "")
	switch {
	case err == nil:
		if err := json.Unmarshal(resultBytes, &st); err != nil {
			return rec, fmt.Errorf("%w: %s/result.json: %v", store.ErrCorrupt, dir, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return rec, fmt.Errorf("%w: %s/result.json cannot be read: %v", store.ErrCorrupt, dir, err)
	default:
		parse := parsers[in.Role]
		if parse == nil {
			return rec, fmt.Errorf("no parser for role %s", in.Role)
		}
		so, _ := run.Path(in.StdoutRef)
		se, _ := run.Path(in.StderrRef)
		st.Result, st.Error = parse(so, se, o)
		b, _ := json.MarshalIndent(st, "", " ")
		resultBytes = append(b, '\n')
		if _, err := writeOrReuse(run, dir+"/result.json", resultBytes); err != nil {
			return rec, err
		}
	}
	f := finished{ID: in.ID, OK: st.Result != nil, OutcomeRef: dir + "/outcome.json", OutcomeSHA: digest(outcomeBytes),
		ResultRef: dir + "/result.json", ResultSHA: digest(resultBytes)}
	if st.Error != nil {
		f.Class = string(st.Error.Class)
	}
	if err := streamDigests(run, &f, in.StdoutRef, in.StderrRef); err != nil {
		return rec, err
	}
	if _, err := run.Append(EvFinished, f); err != nil {
		return rec, err
	}
	rec.Result, rec.Err = st.Result, st.Error
	return rec, nil
}

func parseSaved(run *store.Run, in *intent, st *started, parsers map[string]ParseFunc, rec Recovered) Recovered {
	parse := parsers[in.Role]
	if parse == nil {
		rec.Status, rec.Reason = StatusUnknown, "no parser for role "+in.Role
		return rec
	}
	so, _ := run.Path(in.StdoutRef)
	se, _ := run.Path(in.StderrRef)
	o := provider.Outcome{Exit: 0, Identity: st.Identity, Started: time.UnixMicro(st.Identity.StartMicros)}
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

// holdersOf lists processes holding the attempt's working directory.
func holdersOf(dir string) ([]int, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, nil
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return holders.List([]string{dir})
}

func record(run *store.Run, rec Recovered) error {
	v := resolved{ID: rec.ID, Reason: rec.Reason, Remains: rec.Remains, Blocked: rec.Blocked}
	typ := EvUnknown
	switch rec.Status {
	case StatusRecovered:
		typ = EvRecovered
		b, _ := json.MarshalIndent(stored{Result: rec.Result}, "", " ")
		ref := "attempts/" + rec.ID + "/recovered-result.json"
		sha, err := writeOrReuse(run, ref, append(b, '\n'))
		if err != nil {
			return err
		}
		v.ResultRef, v.ResultSHA = ref, sha
		dir := "attempts/" + rec.ID
		if v.StdoutRef, v.StdoutSHA, v.StderrRef, v.StderrSHA, err = digestStreams(run, dir+"/stdout.jsonl", dir+"/stderr.log"); err != nil {
			return err
		}
	case StatusNotStarted:
		typ = EvNotStarted
	}
	_, err := run.Append(typ, v)
	return err
}

// writeOrReuse writes a write-once artifact, or accepts an identical one that
// an interrupted recovery already wrote. Different bytes are an error.
func writeOrReuse(run *store.Run, ref string, data []byte) (string, error) {
	sha, err := run.WriteArtifact(ref, data, 0o600)
	if err == nil {
		return sha, nil
	}
	existing, rerr := run.ReadArtifact(ref, "")
	if rerr != nil {
		return "", err
	}
	if !bytes.Equal(existing, data) {
		return "", fmt.Errorf("%w: %s exists with different content", store.ErrCorrupt, ref)
	}
	return digest(existing), nil
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

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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
