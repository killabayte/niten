package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/testutil"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testutil.Root(), "internal", "contract", "testdata", "valid", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var v any
	json.Unmarshal(b, &v)
	c, _ := json.Marshal(v)
	return string(c)
}

func lines(evs ...string) string { return strings.Join(evs, "\n") + "\n" }

const (
	initOK     = `{"type":"system","subtype":"init","model":"claude-opus-5-5","permissionMode":"acceptEdits","tools":["Read","Grep","Glob","Edit","Write","Bash","StructuredOutput"]}`
	assistOK   = `{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","name":"Edit"},{"type":"text"}]}}`
	userOK     = `{"type":"user","message":{"content":[]}}`
	resultFrom = `{"type":"result","subtype":"success","is_error":false,"terminal_reason":"completed","modelUsage":{"claude-opus-5-5":{"inputTokens":10}},"structured_output":`
)

func claudeReq() ClaudeRequest {
	return ClaudeRequest{Model: "claude-opus-5-5", Effort: "xhigh", Schema: []byte(`{}`), Settings: "/s/settings.json",
		Validate: func(b []byte) error { return contract.ValidatePayload(contract.KindCandidateReady, b) }}
}

func parseClaudeStream(t *testing.T, stream, stderr string, o Outcome, r ClaudeRequest) (*Result, *Error) {
	t.Helper()
	dir := t.TempDir()
	so, se := filepath.Join(dir, "stdout"), filepath.Join(dir, "stderr")
	os.WriteFile(so, []byte(stream), 0o600)
	os.WriteFile(se, []byte(stderr), 0o600)
	return ParseClaude(so, se, o, r)
}

func TestParseClaude(t *testing.T) {
	ok := fixture(t, "candidate_ready")
	result := resultFrom + ok + "}"
	res, e := parseClaudeStream(t, lines(initOK, assistOK, userOK, result), "", Outcome{}, claudeReq())
	if e != nil || string(res.Payload) != ok || res.Reported.Model != "claude-opus-5-5" || res.Reported.Effort != Unknown || res.Finish != "completed" {
		t.Fatalf("success: %+v %v", res, e)
	}
	alias := claudeReq()
	alias.Model = "opus"
	for name, tc := range map[string]struct {
		stream string
		o      Outcome
		r      ClaudeRequest
		class  Class
	}{
		"delegation":         {stream: lines(initOK, `{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","name":"Agent"}]}}`, result), class: ClassProtocol},
		"tool outside":       {stream: lines(initOK, `{"type":"assistant","message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","name":"WebSearch"}]}}`, result), class: ClassProtocol},
		"profile offers web": {stream: lines(strings.Replace(initOK, `"Bash"`, `"Bash","WebFetch"`, 1), result), class: ClassProtocol},
		"permission mode":    {stream: lines(strings.Replace(initOK, "acceptEdits", "bypassPermissions", 1), result), class: ClassProtocol},
		"wrong model":        {stream: lines(strings.Replace(initOK, "claude-opus-5-5", "claude-opus-5-4", 1), result), class: ClassProtocol},
		"alias request":      {stream: lines(initOK, result), r: alias, class: ClassProtocol},
		"helper model reply": {stream: lines(initOK, strings.Replace(assistOK, "claude-opus-5-5", "claude-haiku-4-5", 1), result), class: ClassProtocol},
		"no init":            {stream: lines(result), class: ClassProtocol},
		"no result":          {stream: lines(initOK, assistOK), class: ClassTransport},
		"truncated":          {stream: initOK + "\n" + result[:len(result)/2], class: ClassTransport},
		"malformed":          {stream: lines(initOK, "{not json", result), class: ClassTransport},
		"exit error":         {stream: lines(initOK, result), o: Outcome{Exit: 1}, class: ClassTransport},
		"invalid payload":    {stream: lines(initOK, resultFrom+`{"steps":[]}}`), class: ClassPayload},
		"no payload":         {stream: lines(initOK, resultFrom+`null}`), class: ClassPayload},
		"rate limit":         {stream: lines(initOK, `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429,"result":"limit"}`), class: ClassRateLimit},
		"refusal":            {stream: lines(initOK, `{"type":"result","subtype":"success","stop_reason":"refusal","result":"no"}`), class: ClassRefusal},
		"timeout":            {stream: lines(initOK, result), o: Outcome{TimedOut: true}, class: ClassTimeout},
		"canceled":           {stream: lines(initOK, result), o: Outcome{Canceled: true}, class: ClassCanceled},
		"stream limit":       {stream: lines(initOK, result), o: Outcome{Limit: "stdout exceeded 10 bytes"}, class: ClassTransport},
	} {
		t.Run(name, func(t *testing.T) {
			r := claudeReq()
			if tc.r.Model != "" {
				r = tc.r
			}
			res, e := parseClaudeStream(t, tc.stream, "stderr line\n", tc.o, r)
			if res != nil || e == nil || e.Class != tc.class {
				t.Fatalf("got %+v %v, want class %s", res, e, tc.class)
			}
		})
	}
}

func TestClaudeArgsKeepTheProfile(t *testing.T) {
	budget := 5.0
	r := claudeReq()
	r.AllowedBash = []string{"Bash(go test:*)", "Bash(git diff:*)"}
	r.MaxBudgetUSD = &budget
	args := ClaudeArgs(r)
	joined := strings.Join(args, " ")
	for _, want := range []string{"--restricted", "--safe-mode", "--tools=Read,Grep,Glob,Edit,Write,Bash", "--permission-mode acceptEdits",
		"--permission-prompts none", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence",
		"--model claude-opus-5-5", "--effort xhigh", "--settings /s/settings.json", "--allowedTools Bash(go test:*) Bash(git diff:*)", "--max-budget-usd 5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv lacks %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--bare") || strings.Contains(joined, "--add-dir") || strings.Contains(joined, "Agent") {
		t.Fatalf("argv has a forbidden flag: %s", joined)
	}
}

const sessionOK = "2026-10-01T10:00:00Z INFO codex_exec: SessionConfiguredEvent { session_id: x, model: \"gpt-6-astra\", reasoning_effort: Some(Xhigh), approval_policy: Never, permission_profile: WorkspaceWrite { access: Write, network: Restricted }, active_permission_profile: y }\n"

func codexReq(dir string) CodexRequest {
	return CodexRequest{Model: "gpt-6-astra", Effort: "xhigh", SchemaPath: filepath.Join(dir, "schema.json"), LastPath: filepath.Join(dir, "last.json"),
		Launcher: filepath.Join(dir, "launcher"), Validate: func(b []byte) error { return contract.ValidatePayload(contract.KindReviewResult, b) }}
}

func TestParseCodex(t *testing.T) {
	ok := fixture(t, "review_result")
	events := lines(`{"type":"thread.started"}`, `{"type":"turn.started"}`, `{"type":"item.completed","item":{"type":"agent_message"}}`, `{"type":"turn.completed","usage":{"input_tokens":5}}`)
	run := func(t *testing.T, stream, stderr, last string, lastAge time.Duration, o Outcome) (*Result, *Error) {
		dir := t.TempDir()
		r := codexReq(dir)
		so, se := filepath.Join(dir, "stdout"), filepath.Join(dir, "stderr")
		os.WriteFile(so, []byte(stream), 0o600)
		os.WriteFile(se, []byte(stderr), 0o600)
		if o.Started.IsZero() {
			o.Started = time.Now()
		}
		if last != "" {
			os.WriteFile(r.LastPath, []byte(last), 0o600)
			at := o.Started.Add(-lastAge)
			os.Chtimes(r.LastPath, at, at)
		}
		return ParseCodex(so, se, o, r)
	}
	res, e := run(t, events, sessionOK, ok, 0, Outcome{})
	if e != nil || string(res.Payload) != ok || res.Reported.Effort != "xhigh" || res.Reported.Model != "gpt-6-astra" {
		t.Fatalf("success: %+v %v", res, e)
	}
	for name, tc := range map[string]struct {
		stream, stderr, last string
		age                  time.Duration
		o                    Outcome
		class                Class
	}{
		"stale output file":  {stream: events, stderr: sessionOK, last: ok, age: time.Hour, class: ClassProtocol},
		"no output file":     {stream: events, stderr: sessionOK, class: ClassPayload},
		"invalid output":     {stream: events, stderr: sessionOK, last: `{"verdict":"maybe"}`, class: ClassPayload},
		"no session record":  {stream: events, stderr: "nothing\n", last: ok, class: ClassProtocol},
		"wrong effort":       {stream: events, stderr: strings.Replace(sessionOK, "Xhigh", "Medium", 1), last: ok, class: ClassProtocol},
		"wrong model":        {stream: events, stderr: strings.Replace(sessionOK, "gpt-6-astra", "gpt-6-mini", 1), last: ok, class: ClassProtocol},
		"network open":       {stream: events, stderr: strings.Replace(sessionOK, "network: Restricted", "network: Enabled", 1), last: ok, class: ClassProtocol},
		"approval asks":      {stream: events, stderr: strings.Replace(sessionOK, "approval_policy: Never", "approval_policy: OnRequest", 1), last: ok, class: ClassProtocol},
		"collab item":        {stream: lines(`{"type":"item.started","item":{"type":"collab_tool_call"}}`) + events, stderr: sessionOK, last: ok, class: ClassProtocol},
		"collab in trace":    {stream: events, stderr: sessionOK + "codex_core::stream_events_utils: ToolCall: collaboration.spawn\n", last: ok, class: ClassProtocol},
		"rate limit":         {stream: lines(`{"type":"turn.failed","error":{"message":"usage limit reached"}}`), stderr: sessionOK, class: ClassRateLimit},
		"no turn.completed":  {stream: lines(`{"type":"thread.started"}`), stderr: sessionOK, last: ok, class: ClassTransport},
		"exit after success": {stream: events, stderr: sessionOK, last: ok, o: Outcome{Exit: 2}, class: ClassTransport},
		"truncated":          {stream: events + `{"type":"turn.compl`, stderr: sessionOK, last: ok, class: ClassTransport},
	} {
		t.Run(name, func(t *testing.T) {
			res, e := run(t, tc.stream, tc.stderr, tc.last, tc.age, tc.o)
			if res != nil || e == nil || e.Class != tc.class {
				t.Fatalf("got %+v %v, want class %s", res, e, tc.class)
			}
		})
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "last.json"), []byte(ok), 0o600)
	if err := CheckFreshOutput(filepath.Join(dir, "last.json")); err == nil {
		t.Fatal("an existing output file was accepted")
	}
	args := strings.Join(CodexArgs(codexReq(dir)), " ")
	for _, want := range []string{"-s workspace-write", "--ignore-user-config", "--ignore-rules", "--disable multi_agent", "agents.enabled=false",
		"project_doc_max_bytes=0", "sandbox_workspace_write.network_access=false", "--ephemeral", "-m gpt-6-astra", "model_reasoning_effort=xhigh"} {
		if !strings.Contains(args, want) {
			t.Errorf("codex argv lacks %q", want)
		}
	}
}

// End to end through the supervisor: a fake executor CLI prints a recorded
// stream; the adapter accepts it, and the argv it got is the profile's.
func TestClaudeThroughTheSupervisor(t *testing.T) {
	ok := fixture(t, "candidate_ready")
	dir := t.TempDir()
	stream := filepath.Join(dir, "stream.jsonl")
	os.WriteFile(stream, []byte(lines(initOK, assistOK, resultFrom+ok+"}")), 0o600)
	argvFile := filepath.Join(dir, "argv")
	r := claudeReq()
	s := spec(t, "replay")
	s.Args = ClaudeArgs(r)
	s.Env = append(s.Env, "NITEN_FAKE_STREAM="+stream, "NITEN_FAKE_ARGV="+argvFile)
	s.Deadline = time.Now().Add(time.Minute)
	out, err := Supervise(context.Background(), s)
	if err != nil || !out.Clean() {
		t.Fatalf("%+v %v", out, err)
	}
	res, e := ParseClaude(s.StdoutPath, s.StderrPath, out, r)
	if e != nil || string(res.Payload) != ok {
		t.Fatalf("%+v %v", res, e)
	}
	var got []string
	b, _ := os.ReadFile(argvFile)
	json.Unmarshal(b, &got)
	if !slices.Equal(got, s.Args) {
		t.Fatalf("argv %v", got)
	}
}

func TestRenderSettings(t *testing.T) {
	tmpl, err := os.ReadFile(filepath.Join(testutil.Root(), "examples", "claude-settings.template.json"))
	if err != nil {
		t.Fatal(err)
	}
	p := SettingsPaths{ExecutorScratch: "/w/run/exec-scratch", OriginalRepo: "/u/repo", Store: "/u/.local/state/niten", GitDir: "/w/run/gitdir",
		Source: "/w/run/src", ClaudeHome: "/u/.claude", CodexHome: "/u/.codex", DenyPatterns: []string{"**/.claude/**", "AGENTS.md"}}
	out, err := RenderSettings(tmpl, p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "{{") || strings.Contains(s, "CONTEXT_COPY") {
		t.Fatalf("a placeholder or a context-copy entry survived:\n%s", s)
	}
	for _, want := range []string{`"Edit(//u/repo/**)"`, `"/w/run/exec-scratch"`, `"/w/run/src/.claude"`, `"Edit(//w/run/src/**/.claude/**)"`, `"Write(//w/run/src/AGENTS.md)"`, `"failIfUnavailable": true`} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered settings lack %s", want)
		}
	}
	var v map[string]any
	if json.Unmarshal(out, &v) != nil {
		t.Fatal("not JSON")
	}
	withCtx := p
	withCtx.ContextCopy = "/w/run/context-1"
	if out, err := RenderSettings(tmpl, withCtx); err != nil || !strings.Contains(string(out), "/w/run/context-1") {
		t.Fatalf("context copy: %v", err)
	}
	rel := p
	rel.Store = "state/niten"
	if _, err := RenderSettings(tmpl, rel); err == nil {
		t.Fatal("a relative path was accepted")
	}
	if _, err := RenderSettings([]byte(`{"x":["{{UNKNOWN_ROOT}}"]}`), p); err == nil {
		t.Fatal("an unknown placeholder was accepted")
	}
	if _, err := RenderSettings(tmpl, SettingsPaths{}); err == nil {
		t.Fatal("empty paths were accepted")
	}
}
