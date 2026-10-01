package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/killabayte/niten/internal/procinfo"
)

// TestMain turns the test binary into a fake CLI when NITEN_FAKE_CLI is set.
func TestMain(m *testing.M) {
	if mode := os.Getenv("NITEN_FAKE_CLI"); mode != "" {
		os.Exit(fakeCLI(mode))
	}
	os.Exit(m.Run())
}

func fakeCLI(mode string) int {
	switch mode {
	case "echo-stdin":
		b, _ := io.ReadAll(os.Stdin)
		os.Stdout.Write(b)
		return 0
	case "both-streams":
		chunk := bytes.Repeat([]byte("x"), 1<<16)
		for i := 0; i < 160; i++ { // 10 MiB on each stream, interleaved
			os.Stdout.Write(append(chunk[:len(chunk)-1:len(chunk)-1], '\n'))
			os.Stderr.Write(chunk)
		}
		return 0
	case "big-event":
		os.Stdout.Write(bytes.Repeat([]byte("y"), 3<<20))
		time.Sleep(30 * time.Second)
		return 0
	case "exit3":
		fmt.Println(`{"type":"partial"}`)
		return 3
	case "sleep-with-grandchild":
		c := exec.Command("/bin/sleep", "60")
		c.Start()
		fmt.Println(c.Process.Pid)
		time.Sleep(60 * time.Second)
		return 0
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		time.Sleep(60 * time.Second)
		return 0
	case "replay":
		if p := os.Getenv("NITEN_FAKE_ARGV"); p != "" {
			b, _ := json.Marshal(os.Args[1:])
			os.WriteFile(p, b, 0o600)
		}
		if p := os.Getenv("NITEN_FAKE_COUNT"); p != "" {
			f, _ := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			f.WriteString("1\n")
			f.Close()
		}
		if p := os.Getenv("NITEN_FAKE_STREAM"); p != "" {
			b, _ := os.ReadFile(p)
			os.Stdout.Write(b)
		}
		if p := os.Getenv("NITEN_FAKE_STDERR"); p != "" {
			b, _ := os.ReadFile(p)
			os.Stderr.Write(b)
		}
		if src := os.Getenv("NITEN_FAKE_LAST"); src != "" {
			for i, a := range os.Args {
				if a == "-o" && i+1 < len(os.Args) {
					b, _ := os.ReadFile(src)
					os.WriteFile(os.Args[i+1], b, 0o600)
				}
			}
		}
		code, _ := strconv.Atoi(os.Getenv("NITEN_FAKE_EXIT"))
		return code
	case "stray":
		c := exec.Command("/bin/sleep", "60")
		c.Stdout = os.Stdout
		c.Start()
		fmt.Println(c.Process.Pid)
		return 0
	}
	return 99
}

func spec(t *testing.T, mode string) Spec {
	t.Helper()
	dir := t.TempDir()
	env, _ := FilterEnv(os.Environ(), nil)
	return Spec{Bin: os.Args[0], Args: []string{"-test.run=^$"}, Env: append(env, "NITEN_FAKE_CLI="+mode), Dir: dir,
		StdoutPath: filepath.Join(dir, "stdout.jsonl"), StderrPath: filepath.Join(dir, "stderr.log"), Grace: time.Second}
}

func gone(pid int) bool {
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestStreamsAreDrainedConcurrently(t *testing.T) {
	s := spec(t, "both-streams")
	s.Deadline = time.Now().Add(time.Minute)
	out, err := Supervise(context.Background(), s)
	if err != nil || !out.Clean() {
		t.Fatalf("%+v %v", out, err)
	}
	so, _ := os.Stat(s.StdoutPath)
	se, _ := os.Stat(s.StderrPath)
	if so.Size() != 160<<16 || se.Size() != 160<<16 {
		t.Fatalf("stream sizes %d %d", so.Size(), se.Size())
	}
}

func TestIdentityIsRecordedBeforeWaiting(t *testing.T) {
	s := spec(t, "echo-stdin")
	s.Stdin = []byte(`{"prompt":"hi"}`)
	var got Identity
	s.OnStart = func(id Identity) error {
		got = id
		if info, err := procinfo.Get(id.PID); err != nil || info.StartMicros() != id.StartMicros || info.PGID != id.PID {
			return fmt.Errorf("identity %+v does not match the live process %+v %v", id, info, err)
		}
		return nil
	}
	out, err := Supervise(context.Background(), s)
	if err != nil || !out.Clean() || got.PID == 0 || got != out.Identity {
		t.Fatalf("%+v %+v %v", out, got, err)
	}
	if b, _ := os.ReadFile(s.StdoutPath); string(b) != `{"prompt":"hi"}` {
		t.Fatalf("stdin was not delivered: %q", b)
	}
}

func TestFailuresAreNotClean(t *testing.T) {
	for name, tc := range map[string]struct {
		mode   string
		tweak  func(*Spec)
		check  func(Outcome) bool
		wantRe string
	}{
		"exit code":    {mode: "exit3", check: func(o Outcome) bool { return o.Exit == 3 }},
		"event limit":  {mode: "big-event", tweak: func(s *Spec) { s.MaxEventBytes = 1 << 20 }, check: func(o Outcome) bool { return strings.Contains(o.Limit, "event exceeded") }},
		"stream limit": {mode: "both-streams", tweak: func(s *Spec) { s.MaxStreamBytes = 1 << 20 }, check: func(o Outcome) bool { return strings.Contains(o.Limit, "exceeded") }},
		"deadline":     {mode: "ignore-term", tweak: func(s *Spec) { s.Deadline = time.Now().Add(500 * time.Millisecond) }, check: func(o Outcome) bool { return o.TimedOut && o.Signal == "killed" }},
	} {
		t.Run(name, func(t *testing.T) {
			s := spec(t, tc.mode)
			if s.Deadline.IsZero() {
				s.Deadline = time.Now().Add(time.Minute)
			}
			if tc.tweak != nil {
				tc.tweak(&s)
			}
			out, err := Supervise(context.Background(), s)
			if err != nil || out.Clean() || !tc.check(out) {
				t.Fatalf("%+v %v", out, err)
			}
		})
	}
}

// Cancel stops the whole group: the CLI and a grandchild it started.
func TestCancelTerminatesTheGroup(t *testing.T) {
	s := spec(t, "sleep-with-grandchild")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 200; i++ {
			if b, _ := os.ReadFile(s.StdoutPath); len(b) > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	out, err := Supervise(ctx, s)
	if err != nil || !out.Canceled || out.Clean() {
		t.Fatalf("%+v %v", out, err)
	}
	b, _ := os.ReadFile(s.StdoutPath)
	grandchild, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if grandchild == 0 || !gone(grandchild) || !gone(out.Identity.PID) {
		t.Fatalf("processes survived cancel: grandchild %d", grandchild)
	}
}

func TestStrayDescendantIsKilled(t *testing.T) {
	s := spec(t, "stray")
	s.Deadline = time.Now().Add(time.Minute)
	out, err := Supervise(context.Background(), s)
	b, _ := os.ReadFile(s.StdoutPath)
	child, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || !out.Stray || child == 0 || !gone(child) {
		t.Fatalf("%+v %v child %d", out, err, child)
	}
}

func TestOnStartFailureKillsTheAttempt(t *testing.T) {
	s := spec(t, "ignore-term")
	var pid int
	s.OnStart = func(id Identity) error { pid = id.PID; return errors.New("journal is full") }
	if _, err := Supervise(context.Background(), s); err == nil || !strings.Contains(err.Error(), "journal is full") {
		t.Fatalf("got %v", err)
	}
	if !gone(pid) {
		t.Fatal("the attempt kept running after its start could not be recorded")
	}
}

// A result file of another attempt is never reused: the stream files must be new.
func TestStreamFilesMustBeFresh(t *testing.T) {
	s := spec(t, "echo-stdin")
	os.WriteFile(s.StdoutPath, []byte(`{"type":"result","subtype":"success"}`), 0o600)
	if _, err := Supervise(context.Background(), s); err == nil {
		t.Fatal("an existing stdout file was accepted")
	}
}

func TestTerminateRecordedOnlyKillsTheSameProcess(t *testing.T) {
	start := func() (*exec.Cmd, Identity) {
		c := exec.Command("/bin/sleep", "60")
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		go c.Wait()
		info, _ := procinfo.Get(c.Process.Pid)
		return c, Identity{PID: c.Process.Pid, StartMicros: info.StartMicros(), PGID: c.Process.Pid}
	}
	c, id := start()
	defer c.Process.Kill()
	stale := id
	stale.StartMicros-- // the PID now names a different process
	if err := TerminateRecorded(stale, time.Second); !errors.Is(err, ErrNotOurs) || gone(id.PID) {
		t.Fatalf("a reused PID was signalled: %v", err)
	}
	if err := TerminateRecorded(id, time.Second); err != nil || !gone(id.PID) {
		t.Fatalf("the recorded process survived: %v", err)
	}
	if err := TerminateRecorded(id, time.Second); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("a dead group: %v", err)
	}
	// A dead leader with members left: they are reported, never signalled.
	pidFile := filepath.Join(t.TempDir(), "member")
	sh := exec.Command("/bin/sh", "-c", "/bin/sleep 60 >/dev/null 2>&1 & echo $! > '"+pidFile+"'; wait")
	sh.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sh.Start(); err != nil {
		t.Fatal(err)
	}
	info, _ := procinfo.Get(sh.Process.Pid)
	var member int
	for i := 0; i < 200 && member == 0; i++ {
		b, _ := os.ReadFile(pidFile)
		member, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(10 * time.Millisecond)
	}
	defer syscall.Kill(-sh.Process.Pid, syscall.SIGKILL)
	syscall.Kill(sh.Process.Pid, syscall.SIGKILL)
	sh.Wait()
	var remains *GroupRemainsError
	err := TerminateRecorded(Identity{PID: sh.Process.Pid, StartMicros: info.StartMicros(), PGID: sh.Process.Pid}, time.Second)
	if !errors.As(err, &remains) || !slices.Contains(remains.PIDs, member) || syscall.Kill(member, 0) != nil {
		t.Fatalf("members of a dead leader: %v (member %d)", err, member)
	}
}

func TestFilterEnvNeverReturnsValues(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/h", "ANTHROPIC_API_KEY=secret-1", "OPENAI_API_KEY=secret-2", "CLAUDE_CODE_USE_BEDROCK=1",
		"RUST_LOG=debug", "MY_TOKEN=secret-3", "CODEX_HOME=/h/.codex", "CLAUDECODE=1", "ANTHROPIC_MODEL=other"}
	env, stripped := FilterEnv(base, []string{"MY_TOKEN"})
	if !slices.Equal(env, []string{"PATH=/usr/bin", "HOME=/h", "CODEX_HOME=/h/.codex"}) {
		t.Fatalf("env %v", env)
	}
	want := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_MODEL", "CLAUDECODE", "CLAUDE_CODE_USE_BEDROCK", "MY_TOKEN", "OPENAI_API_KEY", "RUST_LOG"}
	if !slices.Equal(stripped, want) {
		t.Fatalf("stripped %v", stripped)
	}
	for _, n := range stripped {
		if strings.Contains(n, "secret") || strings.Contains(n, "=") {
			t.Fatalf("a value leaked into the stripped list: %q", n)
		}
	}
}

// The prompt is written only after the start was recorded: OnStart sees a
// process that has not received its task yet.
func TestPromptFollowsTheRecordedStart(t *testing.T) {
	s := spec(t, "echo-stdin")
	s.Stdin = []byte("the task")
	s.OnStart = func(Identity) error {
		time.Sleep(200 * time.Millisecond) // the child would have echoed by now if it had input
		if b, _ := os.ReadFile(s.StdoutPath); len(b) != 0 {
			return fmt.Errorf("the child received its prompt before the start was recorded: %q", b)
		}
		return nil
	}
	out, err := Supervise(context.Background(), s)
	if err != nil || !out.Clean() {
		t.Fatalf("%+v %v", out, err)
	}
	if b, _ := os.ReadFile(s.StdoutPath); string(b) != "the task" {
		t.Fatalf("stdout %q", b)
	}
}
