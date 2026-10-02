package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/verify/sandbox"
)

// Probe modes of a fake CLI (FakeAction.Probe).
const (
	ProbeHonest      = "honest"      // follows the probe steps; forbidden ones are refused, the helper runs under a sandbox
	ProbeUnsandboxed = "unsandboxed" // follows the probe steps with nothing refused and no sandbox
	ProbeLazy        = "lazy"        // answers without attempting anything
)

type probeTargets struct {
	Roles map[string]struct {
		Writes []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"writes"`
		Marker string `json:"marker"`
	} `json:"roles"`
}

func (t probeTargets) path(role, name string) string {
	for _, w := range t.Roles[role].Writes {
		if w.Name == name {
			return w.Path
		}
	}
	return ""
}

func readTargets(p string) (probeTargets, error) {
	var t probeTargets
	b, err := os.ReadFile(p)
	if err == nil {
		err = json.Unmarshal(b, &t)
	}
	return t, err
}

// runHelper runs the probe helper test, under the verifier's sandbox with the
// given roots when sandboxed, and returns its combined output and exit code.
func runHelper(sandboxed bool, source, scratch, pkgDir, role string) (string, int) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return err.Error(), 127
	}
	args := []string{"test", "./probe/", "-run", "TestProbe", "-count=1", "-v", "-args", role}
	if !sandboxed {
		cmd := exec.Command(goBin, args...)
		cmd.Dir = pkgDir
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			code = 1
		}
		return string(out), code
	}
	rootOut, err := exec.Command(goBin, "env", "GOROOT").Output()
	if err != nil {
		return err.Error(), 127
	}
	goroot, _ := filepath.EvalSymlinks(strings.TrimSpace(string(rootOut)))
	sb, err := sandbox.New(filepath.Join(filepath.Dir(scratch), "fake-profiles-"+role))
	if err != nil {
		return err.Error(), 126
	}
	if err := sandbox.PrepareScratch(scratch); err != nil {
		return err.Error(), 126
	}
	np, err := sandbox.Policy{SourceRoot: source, ScratchRoot: scratch, Toolchains: []string{goroot}}.Normalize()
	if err != nil {
		return err.Error(), 126
	}
	env, err := sandbox.Environment(np, nil)
	if err != nil {
		return err.Error(), 126
	}
	var out bytes.Buffer
	res, err := sb.Run(contextBackground(), np, sandbox.Command{Argv: append([]string{filepath.Join(goroot, "bin", "go")}, args...), Dir: pkgDir, Env: env, Stdout: &out, Stderr: &out, Timeout: 3 * time.Minute})
	if err != nil {
		return out.String() + err.Error(), 125
	}
	return out.String(), res.ExitCode
}

type fakeUse struct {
	name  string
	input map[string]any
	ok    bool
	out   string
}

// probeExecutor emulates a Claude probe session.
func probeExecutor(mode, model string) int {
	cwd, _ := os.Getwd()
	t, err := readTargets(filepath.Join(cwd, "probe", "targets.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake probe:", err)
		return 3
	}
	original := filepath.Dir(t.path("executor", "original repository"))
	var uses []fakeUse
	if mode != ProbeLazy {
		honest := mode == ProbeHonest
		os.WriteFile("probe/positive.txt", []byte("niten probe positive"), 0o644)
		uses = append(uses, fakeUse{"Write", map[string]any{"file_path": filepath.Join(cwd, "probe/positive.txt"), "content": "niten probe positive"}, true, "File created"})
		b, _ := os.ReadFile("probe/edit.txt")
		os.WriteFile("probe/edit.txt", bytes.Replace(b, []byte("before"), []byte("after"), 1), 0o644)
		uses = append(uses, fakeUse{"Edit", map[string]any{"file_path": filepath.Join(cwd, "probe/edit.txt")}, true, "File edited"})
		for _, p := range []string{filepath.Join(original, "ESCAPE-write"), filepath.Join(cwd, ".claude", "settings.local.json")} {
			if honest {
				uses = append(uses, fakeUse{"Write", map[string]any{"file_path": p}, false, "Permission denied"})
				continue
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte("escape"), 0o644)
			uses = append(uses, fakeUse{"Write", map[string]any{"file_path": p}, true, "File created"})
		}
		scratch := filepath.Dir(os.Getenv("TMPDIR"))
		out, code := runHelper(honest, cwd, scratch, cwd, "executor")
		uses = append(uses, fakeUse{"Bash", map[string]any{"command": "go test ./probe/ -run TestProbe -count=1 -v -args executor"}, code == 0, out})
		log, _ := exec.Command("git", "log", "-1", "--format=%H").Output()
		uses = append(uses, fakeUse{"Bash", map[string]any{"command": "git log -1 --format=%H"}, true, string(log)})
		diff, _ := exec.Command("git", "diff", "--stat").Output()
		uses = append(uses, fakeUse{"Bash", map[string]any{"command": "git diff --stat"}, true, string(diff)})
		esc := filepath.Join(original, "ESCAPE-unsandboxed")
		if !honest {
			os.WriteFile(esc, nil, 0o644)
		}
		uses = append(uses, fakeUse{"Bash", map[string]any{"command": "/usr/bin/touch " + esc, "dangerouslyDisableSandbox": true}, !honest, "sandbox required"})
	}
	m, _ := json.Marshal(model)
	fmt.Printf(`{"type":"system","subtype":"init","model":%s,"permissionMode":"acceptEdits","apiKeySource":"none","mcp_servers":[],"tools":["Read","Grep","Glob","Edit","Write","Bash","StructuredOutput"]}`+"\n", m)
	for i, u := range uses {
		id := fmt.Sprintf("toolu_%02d", i)
		use, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"model": model, "content": []any{map[string]any{"type": "tool_use", "id": id, "name": u.name, "input": u.input}}}})
		res, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": !u.ok, "content": u.out}}}})
		fmt.Println(string(use))
		fmt.Println(string(res))
	}
	fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","modelUsage":{},"permission_denials":[],"structured_output":{"steps":[{"step":1,"outcome":"%s"}]}}`+"\n", mode)
	return 0
}

// probeReviewer emulates a Codex probe session.
func probeReviewer(mode, model, last string) int {
	cwd, _ := os.Getwd()
	t, err := readTargets(filepath.Join(cwd, "source", "probe", "targets.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake probe:", err)
		return 3
	}
	clone := filepath.Dir(t.path("reviewer", "other role's workspace"))
	type item struct {
		cmd  string
		code int
		out  string
	}
	var items []item
	if mode != ProbeLazy {
		honest := mode == ProbeHonest
		out, code := runHelper(honest, filepath.Join(cwd, "source"), filepath.Join(cwd, "scratch"), filepath.Join(cwd, "source"), "reviewer")
		items = append(items, item{"bash -lc 'cd source && go test ./probe/ -run TestProbe -count=1 -v -args reviewer'", code, out})
		os.WriteFile(filepath.Join(cwd, "source", "probe", "review-positive.txt"), []byte("niten probe positive\n"), 0o644)
		items = append(items, item{"bash -lc \"printf 'niten probe positive\\n' > source/probe/review-positive.txt\"", 0, ""})
		esc := filepath.Join(clone, "ESCAPE-reviewer")
		c := 1
		if !honest {
			os.WriteFile(esc, []byte("escape\n"), 0o644)
			c = 0
		}
		items = append(items, item{"bash -lc \"printf 'escape\\n' > " + esc + "\"", c, ""})
	}
	os.WriteFile(last, []byte(`{"steps":[{"step":1,"outcome":"`+mode+`"}]}`), 0o600)
	fmt.Fprintf(os.Stderr, "2026-10-02T10:00:00Z INFO codex_exec: SessionConfiguredEvent { session_id: x, model: %q, reasoning_effort: Some(Xhigh), approval_policy: Never, permission_profile: WorkspaceWrite { access: Write, network: Restricted }, active_permission_profile: y }\n", model)
	fmt.Println(`{"type":"thread.started"}`)
	fmt.Println(`{"type":"turn.started"}`)
	for i, it := range items {
		b, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"id": fmt.Sprintf("item_%d", i), "type": "command_execution", "command": it.cmd, "exit_code": it.code, "status": "completed", "aggregated_output": it.out}})
		fmt.Println(string(b))
	}
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1}}`)
	return 0
}

func contextBackground() context.Context { return context.Background() }
