package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// ExecutorTools are the only tools of the executor profile (P0 profiles).
// StructuredOutput is the CLI's own tool for --json-schema.
var ExecutorTools = []string{"Read", "Grep", "Glob", "Edit", "Write", "Bash"}

// delegationTools start another agent; they are a contract violation.
var delegationTools = map[string]bool{"Agent": true, "Task": true}

// ClaudeRequest is one executor invocation of `claude -p`.
type ClaudeRequest struct {
	Model        string   // exact model id; aliases are not accepted
	Effort       string   // requested effort; Claude does not report it back
	Schema       []byte   // self-contained JSON Schema of the payload (contract.Bundle)
	Settings     string   // rendered settings file, outside every model write root
	AllowedBash  []string // permission rules such as "Bash(go test:*)"
	MaxBudgetUSD *float64 // optional Claude-only cap; not a shared budget
	// Validate checks the structured output; it must be the contract
	// validator of the payload kind.
	Validate func([]byte) error
}

// ClaudeArgs is the executor argv without the binary. --safe-mode keeps the
// repository's CLAUDE.md and customizations out of the session, so a
// candidate's edit to an instruction file never instructs the next call.
func ClaudeArgs(r ClaudeRequest) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose",
		"--model", r.Model, "--effort", r.Effort, "--json-schema", string(r.Schema),
		"--restricted", "--safe-mode", "--tools=" + strings.Join(ExecutorTools, ","),
		"--permission-mode", "acceptEdits", "--permission-prompts", "none"}
	if len(r.AllowedBash) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, r.AllowedBash...)
	}
	args = append(args, "--settings", r.Settings, "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence")
	if r.MaxBudgetUSD != nil {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(*r.MaxBudgetUSD, 'f', -1, 64))
	}
	return args
}

type claudeEvent struct {
	Type           string          `json:"type"`
	Subtype        string          `json:"subtype"`
	Model          string          `json:"model"`
	PermissionMode string          `json:"permissionMode"`
	Tools          []string        `json:"tools"`
	Message        json.RawMessage `json:"message"`
	RateLimitInfo  *struct {
		Status string `json:"status"`
	} `json:"rate_limit_info"`
	IsError           bool            `json:"is_error"`
	StopReason        string          `json:"stop_reason"`
	TerminalReason    string          `json:"terminal_reason"`
	APIErrorStatus    int             `json:"api_error_status"`
	Result            string          `json:"result"`
	StructuredOutput  json.RawMessage `json:"structured_output"`
	ModelUsage        json.RawMessage `json:"modelUsage"`
	PermissionDenials []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
}

type assistantMessage struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content"`
}

// ParseClaude turns a finished executor attempt into a result. It needs the
// session's init event, a result event, a clean exit and schema-valid
// structured output. The reported model must equal the requested one; the
// permission mode must be acceptEdits; the session may offer and use only the
// executor tools, and any delegation is a protocol violation.
func ParseClaude(stdoutPath, stderrPath string, o Outcome, r ClaudeRequest) (*Result, *Error) {
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
	allowed := append(slices.Clone(ExecutorTools), "StructuredOutput")
	res := &Result{Reported: Reported{Model: Unknown, Effort: Unknown, PermissionMode: Unknown, Profile: Unknown}}
	var final *claudeEvent
	sawInit, rateLimited := false, false
	for _, line := range lines {
		var ev claudeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fail(ClassTransport, "malformed event: %v", err)
		}
		switch ev.Type {
		case "system":
			if ev.Subtype != "init" {
				continue
			}
			sawInit = true
			res.Reported.Model, res.Reported.PermissionMode = ev.Model, ev.PermissionMode
			if ev.PermissionMode != "acceptEdits" {
				return nil, fail(ClassProtocol, "permission mode %q, want acceptEdits", ev.PermissionMode)
			}
			for _, t := range ev.Tools {
				if !slices.Contains(allowed, t) {
					return nil, fail(ClassProtocol, "tool %q is available to the model; the executor profile allows only %s", t, strings.Join(ExecutorTools, ","))
				}
			}
		case "assistant":
			var m assistantMessage
			_ = json.Unmarshal(ev.Message, &m)
			if m.Model != "" && m.Model != "<synthetic>" && m.Model != res.Reported.Model {
				return nil, fail(ClassProtocol, "assistant message from %q, session model %q", m.Model, res.Reported.Model)
			}
			for _, c := range m.Content {
				if c.Type != "tool_use" {
					continue
				}
				if delegationTools[c.Name] {
					return nil, fail(ClassProtocol, "the model tried to delegate with %q", c.Name)
				}
				if !slices.Contains(allowed, c.Name) {
					return nil, fail(ClassProtocol, "the model used %q, outside the executor profile", c.Name)
				}
			}
		case "rate_limit_event":
			if ev.RateLimitInfo != nil && ev.RateLimitInfo.Status == "rejected" {
				rateLimited = true
			}
		case "result":
			e := ev
			final = &e
		case "user":
		default:
			res.Degraded = append(res.Degraded, "unknown event "+ev.Type)
		}
	}
	if final == nil {
		if rateLimited {
			return nil, fail(ClassRateLimit, "rate limit rejected before a result")
		}
		return nil, fail(ClassTransport, "no result event (exit %d): %s", o.Exit, firstLine(stderr))
	}
	res.Finish, res.Usage = final.TerminalReason, final.ModelUsage
	failU := func(c Class, format string, a ...any) *Error {
		e := fail(c, format, a...)
		e.Usage = final.ModelUsage
		return e
	}
	for _, d := range final.PermissionDenials {
		res.Degraded = append(res.Degraded, "permission denied: "+d.ToolName)
	}
	switch {
	case rateLimited || final.APIErrorStatus == 429:
		return nil, failU(ClassRateLimit, "%s", final.Result)
	case final.StopReason == "refusal":
		return nil, failU(ClassRefusal, "%s", final.Result)
	case final.IsError && (final.APIErrorStatus == 401 || final.APIErrorStatus == 403 || final.APIErrorStatus == 404):
		return nil, failU(ClassConfig, "%s", final.Result)
	case strings.HasPrefix(final.Subtype, "error_max_structured_output"):
		return nil, failU(ClassPayload, "structured output retries exhausted: %s", final.Subtype)
	case final.IsError || final.Subtype != "success" || o.Exit != 0 || o.Signal != "":
		return nil, failU(ClassTransport, "subtype=%s is_error=%v exit=%d: %s", final.Subtype, final.IsError, o.Exit, final.Result)
	case !sawInit:
		return nil, failU(ClassProtocol, "no init event: the session model and permission mode are unverified")
	case res.Reported.Model != r.Model:
		return nil, failU(ClassProtocol, "requested model %q, the CLI reports %q", r.Model, res.Reported.Model)
	case len(final.StructuredOutput) == 0 || string(final.StructuredOutput) == "null":
		return nil, failU(ClassPayload, "the result has no structured_output")
	}
	if r.Validate == nil {
		return nil, failU(ClassPayload, "no payload validator")
	}
	if err := r.Validate(final.StructuredOutput); err != nil {
		return nil, failU(ClassPayload, "%v", err)
	}
	res.Payload = final.StructuredOutput
	if o.Stray {
		res.Degraded = append(res.Degraded, "a descendant kept the output open and was killed")
	}
	return res, nil
}

// String renders the class and message, for logs.
func (r *Result) String() string { return fmt.Sprintf("%s (%d bytes)", r.Finish, len(r.Payload)) }
