package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Unknown marks a value the CLI did not report. It is never a measurement.
const Unknown = "unknown"

// Class tells the caller what a failed attempt means.
type Class string

const (
	ClassTransport Class = "transport"  // crash, truncated or missing stream, limit, exit error
	ClassPayload   Class = "payload"    // missing or schema-invalid structured output
	ClassRateLimit Class = "rate_limit" // subscription limit: pause, never retried here
	ClassConfig    Class = "config"     // auth, unknown model, rejected arguments
	ClassRefusal   Class = "refusal"    // the model refused
	ClassProtocol  Class = "protocol"   // contract violation: wrong model or mode, delegation, a tool outside the profile
	ClassTimeout   Class = "timeout"    // the attempt deadline was reached
	ClassCanceled  Class = "canceled"   // the coordinator canceled the attempt
)

// Error is a failed attempt. A failed attempt is never a result.
type Error struct {
	Class Class           `json:"class"`
	Msg   string          `json:"msg"`
	Usage json.RawMessage `json:"usage,omitempty"`
}

func (e *Error) Error() string { return string(e.Class) + ": " + e.Msg }

func fail(c Class, format string, a ...any) *Error {
	return &Error{Class: c, Msg: fmt.Sprintf(format, a...)}
}

// Reported is what the CLI said it actually used.
type Reported struct {
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	PermissionMode string `json:"permission_mode"`
	Profile        string `json:"profile"`
}

// Result is a successful, schema-valid attempt.
type Result struct {
	Payload  json.RawMessage `json:"payload"`
	Reported Reported        `json:"reported"`
	Usage    json.RawMessage `json:"usage,omitempty"`
	Finish   string          `json:"finish"`
	Degraded []string        `json:"degraded,omitempty"`
}

// fromOutcome classifies an attempt the supervisor did not let finish cleanly
// for reasons that no stream content can override.
func fromOutcome(o Outcome) *Error {
	switch {
	case o.Canceled:
		return fail(ClassCanceled, "the attempt was canceled")
	case o.TimedOut:
		return fail(ClassTimeout, "the attempt deadline was reached")
	case o.Limit != "":
		return fail(ClassTransport, "%s", o.Limit)
	}
	return nil
}

// readEvents returns the complete lines of a JSONL stream and whether the
// stream ended in the middle of an event.
func readEvents(path string) (lines [][]byte, truncated bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	if len(b) == 0 {
		return nil, false, nil
	}
	truncated = b[len(b)-1] != '\n'
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			lines = append(lines, l)
		}
	}
	if truncated {
		lines = lines[:len(lines)-1]
	}
	return lines, truncated, nil
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return s
}
