package testutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Environment of the scripted model CLIs.
const (
	FakeRoleEnv   = "NITEN_FAKE_ROLE"   // executor or reviewer; set only by the wrapper scripts
	FakeScriptEnv = "NITEN_FAKE_SCRIPT" // the scenario file
	FakeStateEnv  = "NITEN_FAKE_STATE"  // a directory for counters, prompts and argv logs
)

// FakeAction is one scripted invocation of a fake model CLI.
type FakeAction struct {
	// Write and Delete change files relative to the CLI's working directory:
	// the executor's worktree, or the reviewer's launcher (its copy is "source/").
	Write  map[string]string `json:"write,omitempty"`
	Delete []string          `json:"delete,omitempty"`
	// Payload is the structured output. Strings may contain {{CANDIDATE}}, the
	// commit named in the reviewer's prompt, {{PREVIOUS}}, the candidate the
	// reviewer saw before, and {{HEAD}}, the executor's starting commit.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Model overrides the reported model (default: the requested one).
	Model string `json:"model,omitempty"`
	// Exit is the exit status after the streams are written.
	Exit int `json:"exit,omitempty"`
	// Raw replaces the whole stdout stream.
	Raw string `json:"raw,omitempty"`
	// Sleep delays the answer, for deadline tests.
	Sleep string `json:"sleep,omitempty"`
	// Evidence files the reviewer leaves in ./evidence/.
	Evidence map[string]string `json:"evidence,omitempty"`
}

// FakeScript is a scenario: the actions of each role in invocation order.
type FakeScript struct {
	Executor []FakeAction `json:"executor"`
	Reviewer []FakeAction `json:"reviewer"`
}

// FakeModels writes the scenario and two wrapper scripts that run the current
// test binary as the claude and codex CLIs; the wrappers carry the scenario's
// paths, so tests using them can run in parallel. TestMain must call MaybeFakeCLI.
// It returns the wrapper paths and the state directory.
func FakeModels(t *testing.T, dir string, script FakeScript) (claude, codex, state string) {
	t.Helper()
	state = filepath.Join(dir, "fake-state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(script, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(dir, "fake-script.json")
	if err := os.WriteFile(sp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fake-bin")
	os.MkdirAll(bin, 0o755)
	for role, name := range map[string]string{"executor": "claude", "reviewer": "codex"} {
		p := filepath.Join(bin, name)
		w := fmt.Sprintf("#!/bin/sh\n%s=%s %s='%s' %s='%s' exec '%s' \"$@\"\n", FakeRoleEnv, role, FakeScriptEnv, sp, FakeStateEnv, state, os.Args[0])
		if err := os.WriteFile(p, []byte(w), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(bin, "claude"), filepath.Join(bin, "codex"), state
}

// FakeCalls lists the recorded invocations of a role: prompt and argv.
func FakeCalls(t *testing.T, state, role string) (prompts []string, argvs [][]string) {
	t.Helper()
	for n := 1; ; n++ {
		p, err := os.ReadFile(filepath.Join(state, fmt.Sprintf("%s-%d.prompt", role, n)))
		if err != nil {
			return
		}
		prompts = append(prompts, string(p))
		var argv []string
		a, _ := os.ReadFile(filepath.Join(state, fmt.Sprintf("%s-%d.argv", role, n)))
		_ = json.Unmarshal(a, &argv)
		argvs = append(argvs, argv)
	}
}

// MaybeFakeCLI turns the test binary into a fake model CLI when the wrapper
// set the role; it never returns then.
func MaybeFakeCLI() {
	role := os.Getenv(FakeRoleEnv)
	if role == "" {
		return
	}
	os.Exit(fakeCLI(role))
}

var (
	reCandidate = regexp.MustCompile(`Candidate commit ([0-9a-f]{40,64})`)
	reHead      = regexp.MustCompile(`The working directory is at commit ([0-9a-f]{40,64})`)
)

func fakeCLI(role string) int {
	state := os.Getenv(FakeStateEnv)
	var script FakeScript
	b, err := os.ReadFile(os.Getenv(FakeScriptEnv))
	if err == nil {
		err = json.Unmarshal(b, &script)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake cli: script:", err)
		return 3
	}
	n := 1
	for ; ; n++ {
		f, err := os.OpenFile(filepath.Join(state, fmt.Sprintf("%s-%d.prompt", role, n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			prompt, _ := io.ReadAll(os.Stdin)
			f.Write(prompt)
			f.Close()
			argv, _ := json.Marshal(os.Args[1:])
			os.WriteFile(filepath.Join(state, fmt.Sprintf("%s-%d.argv", role, n)), argv, 0o600)
			return act(role, n, script, string(prompt), state)
		}
		if !os.IsExist(err) {
			fmt.Fprintln(os.Stderr, "fake cli:", err)
			return 3
		}
	}
}

func argAfter(flag string) string {
	for i, a := range os.Args {
		if a == flag && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

func act(role string, n int, script FakeScript, prompt, state string) int {
	actions := script.Executor
	if role == "reviewer" {
		actions = script.Reviewer
	}
	if n > len(actions) {
		fmt.Fprintf(os.Stderr, "fake cli: no scripted action %d for the %s\n", n, role)
		return 3
	}
	a := actions[n-1]
	if a.Sleep != "" {
		d, _ := time.ParseDuration(a.Sleep)
		time.Sleep(d)
	}
	for p, c := range a.Write {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "fake cli:", err)
			return 3
		}
	}
	for _, p := range a.Delete {
		os.RemoveAll(p)
	}
	for name, c := range a.Evidence {
		os.WriteFile(filepath.Join("evidence", name), []byte(c), 0o644)
	}
	var compact bytes.Buffer
	if len(a.Payload) > 0 {
		if err := json.Compact(&compact, a.Payload); err != nil {
			fmt.Fprintln(os.Stderr, "fake cli: payload:", err)
			return 3
		}
	}
	payload := compact.String()
	if m := reCandidate.FindStringSubmatch(prompt); m != nil {
		prev, _ := os.ReadFile(filepath.Join(state, "last-candidate"))
		payload = strings.ReplaceAll(payload, "{{PREVIOUS}}", strings.TrimSpace(string(prev)))
		payload = strings.ReplaceAll(payload, "{{CANDIDATE}}", m[1])
		os.WriteFile(filepath.Join(state, "last-candidate"), []byte(m[1]), 0o600)
	}
	if m := reHead.FindStringSubmatch(prompt); m != nil {
		payload = strings.ReplaceAll(payload, "{{HEAD}}", m[1])
	}
	if role == "executor" {
		model := argAfter("--model")
		if a.Model != "" {
			model = a.Model
		}
		if a.Raw != "" {
			fmt.Print(a.Raw)
		} else {
			m, _ := json.Marshal(model)
			fmt.Printf(`{"type":"system","subtype":"init","model":%s,"permissionMode":"acceptEdits","tools":["Read","Grep","Glob","Edit","Write","Bash","StructuredOutput"]}`+"\n", m)
			fmt.Printf(`{"type":"assistant","message":{"model":%s,"content":[{"type":"text","text":"working"}]}}`+"\n", m)
			if payload == "" {
				payload = "null"
			}
			fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","modelUsage":{%s:{"inputTokens":1}},"structured_output":%s}`+"\n", m, payload)
		}
		return a.Exit
	}
	model := argAfter("-m")
	if a.Model != "" {
		model = a.Model
	}
	effort := "Xhigh"
	if out := argAfter("-o"); out != "" && payload != "" {
		os.WriteFile(out, []byte(payload), 0o600)
	}
	fmt.Fprintf(os.Stderr, "2026-10-01T10:00:00Z INFO codex_exec: SessionConfiguredEvent { session_id: x, model: %q, reasoning_effort: Some(%s), approval_policy: Never, permission_profile: WorkspaceWrite { access: Write, network: Restricted }, active_permission_profile: y }\n", model, effort)
	if a.Raw != "" {
		fmt.Print(a.Raw)
	} else {
		fmt.Println(`{"type":"thread.started"}`)
		fmt.Println(`{"type":"turn.started"}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1}}`)
	}
	return a.Exit
}
