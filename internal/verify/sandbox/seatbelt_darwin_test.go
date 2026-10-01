//go:build darwin

package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// backend fails the test when Seatbelt cannot be used: on macOS the verifier
// sandbox is mandatory, so an unavailable launcher is a real P0a failure.
func backend(t *testing.T) *Seatbelt {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatalf("seatbelt backend unavailable: %v", err)
	}
	if err := s.SelfTest(context.Background()); err != nil {
		t.Fatalf("seatbelt self-test failed (an outer sandbox may forbid nesting): %v", err)
	}
	return s
}

// goToolchain returns the real GOROOT and the go binary inside it.
func goToolchain(t *testing.T) (goroot, gobin string) {
	t.Helper()
	goroot = runtime.GOROOT()
	if goroot == "" || !fileExists(filepath.Join(goroot, "bin", "go")) {
		path, err := exec.LookPath("go")
		if err != nil {
			t.Skip("no go toolchain found")
		}
		out, err := exec.Command(path, "env", "GOROOT").Output()
		if err != nil {
			t.Fatal(err)
		}
		goroot = strings.TrimSpace(string(out))
	}
	goroot, err := filepath.EvalSymlinks(goroot)
	if err != nil {
		t.Fatal(err)
	}
	gobin = filepath.Join(goroot, "bin", "go")
	if !fileExists(gobin) {
		t.Fatalf("go binary missing at %s", gobin)
	}
	return goroot, gobin
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

type harness struct {
	policy                      Policy
	src, scratch, outside       string
	store, gitdir, home, tmpKey string
	gobin                       string
	env                         []string
}

// newHarness lays out the topology from docs/p0-profile.md: a verification
// copy, a scratch area, and neighbours that must stay untouched (a fake
// original repo, store, gitdir and home with credential canaries).
func newHarness(t *testing.T) *harness {
	t.Helper()
	goroot, gobin := goToolchain(t)
	base := t.TempDir()
	h := &harness{src: filepath.Join(base, "src"), scratch: filepath.Join(base, "scratch"),
		outside: filepath.Join(base, "outside"), store: filepath.Join(base, "store"),
		gitdir: filepath.Join(base, "gitdir"), home: filepath.Join(base, "home"), gobin: gobin}
	for _, d := range []string{h.src, h.scratch, h.outside, h.store, h.gitdir, filepath.Join(h.home, ".claude")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(h.store, "canary.txt"), "STORE-CANARY\n")
	mustWrite(t, filepath.Join(h.home, ".claude", "credentials.json"), `{"token":"FAKE"}`+"\n")
	if err := copyTree(filepath.Join("testdata", "fixture"), h.src); err != nil {
		t.Fatal(err)
	}
	if err := PrepareScratch(h.scratch); err != nil {
		t.Fatal(err)
	}
	h.tmpKey = fmt.Sprintf("/tmp/niten-probe-%d-%d.txt", os.Getpid(), time.Now().UnixNano())
	p, err := Policy{SourceRoot: h.src, ScratchRoot: h.scratch, Toolchains: []string{goroot}}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	h.policy = p
	h.env, err = Environment(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) run(t *testing.T, s *Seatbelt, timeout time.Duration, extraEnv []string, argv ...string) (Result, string) {
	t.Helper()
	var out bytes.Buffer
	res, err := s.Run(context.Background(), h.policy, Command{
		Argv: argv, Dir: h.src, Env: append(append([]string{}, h.env...), extraEnv...),
		Stdout: &out, Stderr: &out, Timeout: timeout,
	})
	if err != nil {
		t.Fatalf("run %v: %v\n%s", argv, err, out.String())
	}
	return res, out.String()
}

func TestGoBuildAndTestInsideSandbox(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	bin := filepath.Join(h.scratch, "fixture-bin")
	res, out := h.run(t, s, 3*time.Minute, nil, h.gobin, "build", "-o", bin, "./...")
	if res.ExitCode != 0 {
		t.Fatalf("go build failed (%d):\n%s", res.ExitCode, out)
	}
	if !fileExists(bin) {
		t.Fatal("go build produced no binary in scratch")
	}
	res, out = h.run(t, s, 3*time.Minute, nil, h.gobin, "test", "-count=1", "-run", "TestOK", "./...")
	if res.ExitCode != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("go test failed (%d):\n%s", res.ExitCode, out)
	}
	if res.Backend != BackendSeatbelt || res.Launcher != Launcher || len(res.ProfileDigest) != 64 || res.OSVersion == "" {
		t.Fatalf("incomplete evidence fields: %+v", res)
	}
	st, err := os.Stat(res.ProfilePath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("profile file %s: %v mode %v", res.ProfilePath, err, st.Mode())
	}
	if res.Stragglers || res.TimedOut {
		t.Fatalf("unexpected run state: %+v", res)
	}
}

func TestNegativeControlsFromChildProcess(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	before := snapshot(t, h.outside, h.store, h.gitdir, h.home)
	report := filepath.Join(h.scratch, "probe-report.json")
	extra := []string{
		"NITEN_PROBE_REPORT=" + report, "NITEN_PROBE_OUTSIDE=" + h.outside, "NITEN_PROBE_STORE=" + h.store,
		"NITEN_PROBE_GITDIR=" + h.gitdir, "NITEN_PROBE_HOME=" + h.home, "NITEN_PROBE_TMPFILE=" + h.tmpKey,
		"NITEN_PROBE_PORT=" + port,
	}
	res, out := h.run(t, s, 3*time.Minute, extra, h.gobin, "test", "-count=1", "-run", "TestProbe", "./...")
	if res.ExitCode != 0 {
		t.Fatalf("probe test did not complete (%d):\n%s", res.ExitCode, out)
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("no probe report: %v\n%s", err, out)
	}
	var got map[string]struct {
		Allowed bool   `json:"allowed"`
		Err     string `json:"err"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"allow_write_source": true, "allow_write_scratch": true, "allow_read_source": true,
		"deny_write_outside": false, "deny_write_store": false, "deny_write_gitdir": false,
		"deny_write_shared_tmp": false, "deny_read_store_canary": false, "deny_read_home_credential": false,
		"deny_symlink_escape": false, "deny_child_shell_escape": false,
		"deny_network_loopback": false, "deny_network_dns": false,
	}
	for name, allowed := range want {
		a, ok := got[name]
		if !ok {
			t.Errorf("probe %s missing from report", name)
			continue
		}
		if a.Allowed != allowed {
			t.Errorf("probe %s: allowed=%v (%s), want %v", name, a.Allowed, a.Err, allowed)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected probe %s in report", name)
		}
	}
	if after := snapshot(t, h.outside, h.store, h.gitdir, h.home); after != before {
		t.Fatalf("host state changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := os.Stat(h.tmpKey); !errors.Is(err, fs.ErrNotExist) {
		os.Remove(h.tmpKey)
		t.Fatalf("shared temp file was created: %v", err)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	start := time.Now()
	res, _ := h.run(t, s, 1*time.Second, nil, "/bin/sh", "-c", "sleep 30 & sleep 30; wait")
	if !res.TimedOut {
		t.Fatalf("expected timeout, got %+v", res)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout took too long: %v", time.Since(start))
	}
	if res.Stragglers {
		t.Fatal("processes survived the group kill")
	}
	if res.ExitCode == 0 {
		t.Fatalf("timed-out run reported success: %+v", res)
	}
}

func TestRunRefusals(t *testing.T) {
	s := backend(t)
	h := newHarness(t)
	ctx := context.Background()
	good := Command{Argv: []string{"/usr/bin/true"}, Dir: h.src, Env: h.env, Timeout: 10 * time.Second}
	cases := map[string]Command{
		"relative argv":     {Argv: []string{"true"}, Dir: h.src, Env: h.env, Timeout: time.Second},
		"missing binary":    {Argv: []string{"/usr/bin/definitely-not-here"}, Dir: h.src, Env: h.env, Timeout: time.Second},
		"dir outside roots": {Argv: good.Argv, Dir: h.outside, Env: h.env, Timeout: time.Second},
		"nil env":           {Argv: good.Argv, Dir: h.src, Env: nil, Timeout: time.Second},
		"zero timeout":      {Argv: good.Argv, Dir: h.src, Env: h.env},
		"relative dir":      {Argv: good.Argv, Dir: "src", Env: h.env, Timeout: time.Second},
	}
	for name, c := range cases {
		if _, err := s.Run(ctx, h.policy, c); !errors.Is(err, ErrCommand) {
			t.Errorf("%s: want ErrCommand, got %v", name, err)
		}
	}
	if res, err := s.Run(ctx, h.policy, good); err != nil || res.ExitCode != 0 {
		t.Fatalf("valid command refused: %v %+v", err, res)
	}
	if _, err := s.Run(ctx, Policy{SourceRoot: h.src, ScratchRoot: h.src}, good); !errors.Is(err, ErrPolicy) {
		t.Fatalf("invalid policy accepted: %v", err)
	}
	inside, err := New(filepath.Join(h.scratch, "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inside.Run(ctx, h.policy, good); !errors.Is(err, ErrPolicy) {
		t.Fatalf("profile dir inside scratch accepted: %v", err)
	}
}

func TestLauncherMustBeTheFixedRootBinary(t *testing.T) {
	dir := t.TempDir()
	if _, err := newSeatbelt("/usr/bin/true", dir); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("foreign launcher accepted: %v", err)
	}
	if _, err := newSeatbelt(Launcher, "relative/profiles"); !errors.Is(err, ErrPolicy) {
		t.Fatalf("relative profile dir accepted: %v", err)
	}
}

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// snapshot lists every file under the roots with its size and content hash, so
// a test can prove the neighbours of the sandbox were not modified.
func snapshot(t *testing.T, roots ...string) string {
	t.Helper()
	var b strings.Builder
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "%s %d %s\n", p, len(data), Digest(string(data)))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}
