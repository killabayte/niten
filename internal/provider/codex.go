package provider

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"time"
)

// CodexDisabledFeatures are passed as --disable (reviewer profile).
var CodexDisabledFeatures = []string{"apps", "browser_use", "browser_use_external", "computer_use", "image_generation", "multi_agent", "goals", "hooks"}

// CodexRustLog enables the session diagnostics the adapter verifies.
const CodexRustLog = "RUST_LOG=codex_exec=info,codex_core=info"

// CodexRequest is one reviewer invocation of `codex exec`.
type CodexRequest struct {
	Model      string
	Effort     string
	SchemaPath string // self-contained output schema file in the attempt directory
	LastPath   string // the CLI's output file; must not exist before the attempt
	Launcher   string // clean launcher directory, without candidate-controlled ancestors
	Validate   func([]byte) error
}

// CodexArgs is the reviewer argv without the binary. project_doc_max_bytes=0
// keeps AGENTS.md out of the session, so instructions come only from the
// coordinator's prompt.
func CodexArgs(r CodexRequest) []string {
	args := []string{"exec", "--json", "--output-schema", r.SchemaPath, "-o", r.LastPath,
		"-m", r.Model, "-c", "model_reasoning_effort=" + r.Effort, "-c", `approval_policy="never"`,
		"-s", "workspace-write", "-C", r.Launcher, "--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "--ignore-rules"}
	for _, f := range CodexDisabledFeatures {
		args = append(args, "--disable", f)
	}
	return append(args, "-c", "agents.enabled=false", "-c", "project_doc_max_bytes=0",
		"-c", "mcp_servers={}", "-c", "plugins={}", "-c", `web_search="disabled"`,
		"-c", "sandbox_workspace_write.network_access=false",
		"-c", "sandbox_workspace_write.exclude_slash_tmp=true",
		"-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true", "-")
}

// CheckFreshOutput refuses an output file that exists before the attempt: a
// file left by another attempt must never be read as this attempt's answer.
func CheckFreshOutput(path string) error {
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("the output file " + path + " already exists before the attempt")
	}
	return nil
}

type codexEvent struct {
	Type    string          `json:"type"`
	Message string          `json:"message"`
	Usage   json.RawMessage `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
	Item *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"item"`
}

var (
	reSession   = regexp.MustCompile(`codex_exec: .*SessionConfiguredEvent \{.*`)
	reModel     = regexp.MustCompile(`\bmodel: "([^"]+)"`)
	reEffort    = regexp.MustCompile(`\breasoning_effort: Some\((\w+)\)`)
	reApproval  = regexp.MustCompile(`\bapproval_policy: (\w+)`)
	reProfile   = regexp.MustCompile(`\bpermission_profile: (.*?), active_permission_profile`)
	reToolCall  = regexp.MustCompile(`codex_core::stream_events_utils: ToolCall: (\S+)`)
	reDelegated = regexp.MustCompile(`inter_agent_communication`)
)

// ParseCodex turns a finished reviewer attempt into a result. It needs
// turn.completed, a clean exit, a session record whose model, effort and
// approval policy are the requested ones with network access restricted, and
// a schema-valid output file written during this attempt. Delegation anywhere
// is a protocol violation.
func ParseCodex(stdoutPath, stderrPath string, o Outcome, r CodexRequest) (*Result, *Error) {
	if e := fromOutcome(o); e != nil {
		return nil, e
	}
	lines, truncated, err := readEvents(stdoutPath)
	if err != nil {
		return nil, fail(ClassTransport, "read stdout: %v", err)
	}
	stderr, _ := os.ReadFile(stderrPath)
	if truncated {
		return nil, fail(ClassTransport, "the stream ended in the middle of an event (exit %d)", o.Exit)
	}
	for _, m := range reToolCall.FindAllSubmatch(stderr, -1) {
		if strings.HasPrefix(string(m[1]), "collaboration") {
			return nil, fail(ClassProtocol, "sub-agent tool call %s", m[1])
		}
	}
	if reDelegated.Match(stderr) {
		return nil, fail(ClassProtocol, "inter-agent communication in the trace")
	}
	res := &Result{Reported: Reported{Model: Unknown, Effort: Unknown, PermissionMode: Unknown, Profile: Unknown}}
	completed, failure := false, ""
	for _, line := range lines {
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fail(ClassTransport, "malformed event: %v", err)
		}
		switch ev.Type {
		case "thread.started", "turn.started":
		case "item.started", "item.updated", "item.completed":
			if ev.Item != nil && ev.Item.Type == "collab_tool_call" {
				return nil, fail(ClassProtocol, "collab_tool_call in %s", ev.Type)
			}
			if ev.Type == "item.completed" && ev.Item != nil && ev.Item.Type == "error" {
				res.Degraded = append(res.Degraded, "error item: "+ev.Item.Message)
			}
		case "turn.completed":
			completed, res.Usage = true, ev.Usage
		case "turn.failed":
			if ev.Error != nil {
				failure = ev.Error.Message
			}
		case "error":
			if failure == "" {
				failure = ev.Message
			}
		default:
			res.Degraded = append(res.Degraded, "unknown event "+ev.Type)
		}
	}
	failU := func(c Class, format string, a ...any) *Error {
		e := fail(c, format, a...)
		e.Usage = res.Usage
		return e
	}
	if failure != "" {
		return nil, failU(classifyCodexFailure(failure), "%s", failure)
	}
	if !completed {
		return nil, failU(ClassTransport, "no turn.completed (exit %d): %s", o.Exit, firstLine(stderr))
	}
	if o.Exit != 0 || o.Signal != "" {
		return nil, failU(ClassTransport, "exit %d after turn.completed", o.Exit)
	}
	line := reSession.Find(stderr)
	if line == nil {
		return nil, failU(ClassProtocol, "no SessionConfiguredEvent in stderr: model, effort and policy are unverified")
	}
	get := func(re *regexp.Regexp) string {
		if m := re.FindSubmatch(line); m != nil {
			return string(m[1])
		}
		return Unknown
	}
	res.Reported = Reported{Model: get(reModel), Effort: strings.ToLower(get(reEffort)), PermissionMode: get(reApproval), Profile: get(reProfile)}
	rep := res.Reported
	switch {
	case rep.Model == Unknown || rep.Effort == Unknown || rep.PermissionMode == Unknown || rep.Profile == Unknown:
		return nil, failU(ClassProtocol, "SessionConfiguredEvent is incomplete: %+v", rep)
	case rep.Model != r.Model:
		return nil, failU(ClassProtocol, "requested model %q, the CLI reports %q", r.Model, rep.Model)
	case rep.Effort != r.Effort:
		return nil, failU(ClassProtocol, "requested effort %q, the CLI reports %q", r.Effort, rep.Effort)
	case rep.PermissionMode != "Never":
		return nil, failU(ClassProtocol, "approval policy %q, want Never", rep.PermissionMode)
	case !strings.Contains(rep.Profile, "network: Restricted") || strings.Contains(strings.ToLower(rep.Profile), "fullaccess") || strings.Contains(strings.ToLower(rep.Profile), "full_access"):
		return nil, failU(ClassProtocol, "the permission profile does not restrict the network: %s", rep.Profile)
	}
	st, err := os.Stat(r.LastPath)
	if err != nil {
		return nil, failU(ClassPayload, "no output file: %v", err)
	}
	if st.ModTime().Before(o.Started.Add(-time.Second)) {
		return nil, failU(ClassProtocol, "the output file predates this attempt")
	}
	payload, err := os.ReadFile(r.LastPath)
	if err != nil {
		return nil, failU(ClassPayload, "%v", err)
	}
	if r.Validate == nil {
		return nil, failU(ClassPayload, "no payload validator")
	}
	if err := r.Validate(payload); err != nil {
		return nil, failU(ClassPayload, "%v", err)
	}
	res.Payload, res.Finish = json.RawMessage(payload), "completed"
	if o.Stray {
		res.Degraded = append(res.Degraded, "a descendant kept the output open and was killed")
	}
	return res, nil
}

func classifyCodexFailure(msg string) Class {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, `"status":429`) || strings.Contains(m, `"status": 429`) || strings.Contains(m, "usage limit") || strings.Contains(m, "rate limit"):
		return ClassRateLimit
	case strings.Contains(m, "invalid_request_error") || strings.Contains(m, `"status":400`) || strings.Contains(m, `"status":401`) ||
		strings.Contains(m, `"status":403`) || strings.Contains(m, "not supported"):
		return ClassConfig
	}
	return ClassTransport
}
