package attempt

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/procinfo"
	"github.com/killabayte/niten/internal/provider"
	"github.com/killabayte/niten/internal/store"
	"github.com/killabayte/niten/internal/testutil"
)

// TestMain turns the test binary into a fake executor CLI.
func TestMain(m *testing.M) {
	if os.Getenv("NITEN_FAKE_CLI") == "replay" {
		if p := os.Getenv("NITEN_FAKE_COUNT"); p != "" {
			f, _ := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			f.WriteString("1\n")
			f.Close()
		}
		b, _ := os.ReadFile(os.Getenv("NITEN_FAKE_STREAM"))
		os.Stdout.Write(b)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type world struct {
	t      *testing.T
	st     *store.Store
	id     string
	run    *store.Run
	stream string // a complete executor stream
	count  string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := store.NewRunID(time.Now())
	stg, _ := s.Stage(id)
	stg.Write("state.json", []byte(`{"state":"prepared"}`), 0o600)
	stg.Commit()
	run, _, err := s.OpenRun(id)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := os.ReadFile(filepath.Join(testutil.Root(), "internal", "contract", "testdata", "valid", "candidate_ready.json"))
	var v any
	json.Unmarshal(payload, &v)
	compact, _ := json.Marshal(v)
	stream := `{"type":"system","subtype":"init","model":"claude-opus-5-5","permissionMode":"acceptEdits","tools":["Read","Edit","Bash"]}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","structured_output":` + string(compact) + "}\n"
	w := &world{t: t, st: s, id: id, run: run, stream: filepath.Join(root, "stream.jsonl"), count: filepath.Join(root, "count")}
	os.WriteFile(w.stream, []byte(stream), 0o600)
	t.Cleanup(func() { w.run.Close() })
	return w
}

func claudeParse() ParseFunc {
	req := provider.ClaudeRequest{Model: "claude-opus-5-5", Effort: "xhigh",
		Validate: func(b []byte) error { return contract.ValidatePayload(contract.KindCandidateReady, b) }}
	return func(so, se string, o provider.Outcome) (*provider.Result, *provider.Error) {
		return provider.ParseClaude(so, se, o, req)
	}
}

func (w *world) spec(id string) Spec {
	env, stripped := provider.FilterEnv(os.Environ(), nil)
	return Spec{ID: id, Role: "executor", Bin: os.Args[0], Args: []string{"-p"}, Dir: w.t.TempDir(), Stdin: []byte("prompt"),
		Env: append(env, "NITEN_FAKE_CLI=replay", "NITEN_FAKE_STREAM="+w.stream, "NITEN_FAKE_COUNT="+w.count), Stripped: stripped,
		Deadline: time.Now().Add(time.Minute), Parse: claudeParse()}
}

func (w *world) invocations() int {
	b, _ := os.ReadFile(w.count)
	return strings.Count(string(b), "\n")
}

// reopen simulates a new coordinator after a crash.
func (w *world) reopen() []store.Event {
	w.t.Helper()
	w.run.Close()
	run, events, err := w.st.OpenRun(w.id)
	if err != nil {
		w.t.Fatal(err)
	}
	w.run = run
	return events
}

func (w *world) recover() map[string]Recovered {
	w.t.Helper()
	events := w.reopen()
	recs, err := Recover(w.run, events, map[string]ParseFunc{"executor": claudeParse()}, time.Second)
	if err != nil {
		w.t.Fatal(err)
	}
	out := map[string]Recovered{}
	for _, r := range recs {
		out[r.ID] = r
	}
	return out
}

func deadIdentity(t *testing.T) provider.Identity {
	t.Helper()
	c := exec.Command("/usr/bin/true")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Start()
	info, _ := procinfo.Get(c.Process.Pid)
	c.Wait()
	return provider.Identity{PID: c.Process.Pid, StartMicros: info.StartMicros(), PGID: c.Process.Pid}
}

// writeIntent records an intent (and the stream files, if given) by hand.
func (w *world) writeIntent(id, stdout string, createFiles bool) {
	w.t.Helper()
	dir := "attempts/" + id
	w.run.WriteArtifact(dir+"/prompt", []byte("prompt"), 0o600)
	w.run.Append(EvIntent, intent{ID: id, Role: "executor", Bin: "claude", PromptRef: dir + "/prompt", StdoutRef: dir + "/stdout.jsonl", StderrRef: dir + "/stderr.log"})
	if createFiles {
		p, _ := w.run.Path(dir + "/stdout.jsonl")
		os.WriteFile(p, []byte(stdout), 0o600)
		e, _ := w.run.Path(dir + "/stderr.log")
		os.WriteFile(e, nil, 0o600)
	}
}

func TestAttemptProtocol(t *testing.T) {
	w := newWorld(t)
	res, perr, err := (&Runner{Store: w.run}).Run(context.Background(), w.spec("a0001-executor"))
	if err != nil || perr != nil || res == nil {
		t.Fatalf("%+v %v %v", res, perr, err)
	}
	if _, _, err := (&Runner{Store: w.run}).Run(context.Background(), w.spec("a0001-executor")); err == nil {
		t.Fatal("an attempt id was reused")
	}
	events := w.reopen()
	var types []string
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != EvIntent+","+EvStarted+","+EvFinished {
		t.Fatalf("events %v", types)
	}
	var in intent
	json.Unmarshal(events[0].Data, &in)
	if in.PromptSHA == "" || len(in.EnvNames) == 0 || strings.Contains(string(events[0].Data), "secret") {
		t.Fatalf("intent %+v", in)
	}
	for _, n := range in.EnvNames {
		if strings.Contains(n, "=") {
			t.Fatalf("an environment value was recorded: %q", n)
		}
	}
	var st started
	json.Unmarshal(events[1].Data, &st)
	if st.Identity.PID == 0 || st.Identity.StartMicros == 0 {
		t.Fatalf("started %+v", st)
	}
}

// After the result and before the state checkpoint: the journal already has
// the finished attempt, so the outcome is known without the projection.
func TestCrashAfterFinishBeforeCheckpoint(t *testing.T) {
	w := newWorld(t)
	(&Runner{Store: w.run}).Run(context.Background(), w.spec("a0001-executor"))
	r := w.recover()["a0001-executor"]
	if r.Status != StatusFinished || r.Result == nil || w.invocations() != 1 {
		t.Fatalf("%+v invocations %d", r, w.invocations())
	}
}

// A crash in the middle of writing attempt.finished leaves a torn line. The
// saved stream still holds the result, which is recovered without calling the
// model again.
func TestCrashDuringTheFinishWrite(t *testing.T) {
	w := newWorld(t)
	(&Runner{Store: w.run}).Run(context.Background(), w.spec("a0001-executor"))
	w.run.Close()
	journal := filepath.Join(w.st.RunDir(w.id), "events.jsonl")
	b, _ := os.ReadFile(journal)
	lines := strings.SplitAfter(string(b), "\n")
	torn := strings.Join(lines[:2], "") + lines[2][:len(lines[2])/2]
	os.WriteFile(journal, []byte(torn), 0o600)
	run, _, err := w.st.OpenRun(w.id)
	if err != nil {
		t.Fatal(err)
	}
	w.run = run
	r := w.recover()["a0001-executor"]
	if r.Status != StatusRecovered || r.Result == nil || w.invocations() != 1 {
		t.Fatalf("%+v invocations %d", r, w.invocations())
	}
	if !strings.Contains(strings.Join(r.Result.Degraded, " "), "exit status is unknown") {
		t.Fatalf("degraded %v", r.Result.Degraded)
	}
	// The resolution is durable: a second recovery reads it back.
	if again := w.recover()["a0001-executor"]; again.Status != StatusRecovered || w.invocations() != 1 {
		t.Fatalf("second recovery %+v", again)
	}
}

func TestCrashBeforeAndDuringTheAttempt(t *testing.T) {
	w := newWorld(t)
	full, _ := os.ReadFile(w.stream)
	// Before start: the intent is recorded, the stream files were never created.
	w.writeIntent("a0001-executor", "", false)
	// Started, then the coordinator died with half the stream.
	w.writeIntent("a0002-executor", string(full[:len(full)/2]), true)
	w.run.Append(EvStarted, started{ID: "a0002-executor", Identity: deadIdentity(t)})
	// Started, the stream is complete, the process is gone.
	w.writeIntent("a0003-executor", string(full), true)
	w.run.Append(EvStarted, started{ID: "a0003-executor", Identity: deadIdentity(t)})
	// The process started but the started event was lost; the files are empty.
	w.writeIntent("a0004-executor", "", true)
	recs := w.recover()
	for id, want := range map[string]Status{"a0001-executor": StatusNotStarted, "a0002-executor": StatusUnknown,
		"a0003-executor": StatusRecovered, "a0004-executor": StatusUnknown} {
		if recs[id].Status != want {
			t.Errorf("%s: %s (%s), want %s", id, recs[id].Status, recs[id].Reason, want)
		}
	}
	if w.invocations() != 0 {
		t.Fatalf("recovery called the model %d time(s)", w.invocations())
	}
}

// A recorded process that is still alive with its recorded identity is
// stopped; a group whose leader is gone is reported and left alone, and the
// run is blocked until those processes are gone.
func TestRecoveryAndLiveProcesses(t *testing.T) {
	w := newWorld(t)
	live := exec.Command("/bin/sleep", "60")
	live.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	live.Start()
	go live.Wait()
	info, _ := procinfo.Get(live.Process.Pid)
	w.writeIntent("a0001-executor", "", true)
	w.run.Append(EvStarted, started{ID: "a0001-executor", Identity: provider.Identity{PID: live.Process.Pid, StartMicros: info.StartMicros(), PGID: live.Process.Pid}})

	pidFile := filepath.Join(t.TempDir(), "member")
	leader := exec.Command("/bin/sh", "-c", "/bin/sleep 60 >/dev/null 2>&1 & echo $! > '"+pidFile+"'; wait")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	leader.Start()
	linfo, _ := procinfo.Get(leader.Process.Pid)
	var member int
	for i := 0; i < 200 && member == 0; i++ {
		b, _ := os.ReadFile(pidFile)
		member, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(10 * time.Millisecond)
	}
	defer syscall.Kill(-leader.Process.Pid, syscall.SIGKILL)
	syscall.Kill(leader.Process.Pid, syscall.SIGKILL)
	leader.Wait()
	w.writeIntent("a0002-executor", "", true)
	w.run.Append(EvStarted, started{ID: "a0002-executor", Identity: provider.Identity{PID: leader.Process.Pid, StartMicros: linfo.StartMicros(), PGID: leader.Process.Pid}})

	recs := w.recover()
	if r := recs["a0001-executor"]; r.Status != StatusUnknown || r.Blocking() {
		t.Fatalf("live leader: %+v", r)
	}
	for i := 0; i < 100 && syscall.Kill(live.Process.Pid, 0) == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(live.Process.Pid, 0) == nil {
		t.Fatal("the recorded live process was not stopped")
	}
	r := recs["a0002-executor"]
	if !r.Blocking() || r.Remains[0] != member || syscall.Kill(member, 0) != nil {
		t.Fatalf("dead leader with a member: %+v (member %d alive=%v)", r, member, syscall.Kill(member, 0) == nil)
	}
	syscall.Kill(member, syscall.SIGKILL)
	for i := 0; i < 100 && syscall.Kill(member, 0) == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if again := w.recover()["a0002-executor"]; again.Blocking() || again.Status != StatusUnknown {
		t.Fatalf("after the member exited: %+v", again)
	}
}

// When the store refuses the intent, the model is not started at all.
func TestStoreFailureStartsNothing(t *testing.T) {
	w := newWorld(t)
	os.Chmod(w.run.Dir, 0o500)
	defer os.Chmod(w.run.Dir, 0o700)
	if _, _, err := (&Runner{Store: w.run}).Run(context.Background(), w.spec("a0001-executor")); err == nil {
		t.Fatal("an attempt ran without its prompt stored")
	}
	if w.invocations() != 0 {
		t.Fatal("the model was started")
	}
}
