package probe

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// toolUse is one tool call the executor's stream shows, with its result.
type toolUse struct {
	ID      string
	Name    string
	Input   map[string]any
	Done    bool // a tool_result arrived
	IsError bool
	Denied  bool // listed in the result's permission_denials
	Output  string
}

// claudeTrace is what the executor's stream-json shows.
type claudeTrace struct {
	Init         bool
	Tools        []string
	MCPServers   int
	APIKeySource string
	HasResult    bool
	Uses         []*toolUse
}

func readLines(path string) [][]byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for sc.Scan() {
		if l := bytes.TrimSpace(sc.Bytes()); len(l) > 0 {
			out = append(out, append([]byte(nil), l...))
		}
	}
	return out
}

func parseClaude(stdout string) *claudeTrace {
	tr := &claudeTrace{}
	byID := map[string]*toolUse{}
	for _, line := range readLines(stdout) {
		var ev struct {
			Type         string          `json:"type"`
			Subtype      string          `json:"subtype"`
			Tools        []string        `json:"tools"`
			MCPServers   []any           `json:"mcp_servers"`
			APIKeySource string          `json:"apiKeySource"`
			Message      json.RawMessage `json:"message"`
			Denials      []struct {
				ToolUseID string `json:"tool_use_id"`
			} `json:"permission_denials"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				tr.Init, tr.Tools, tr.MCPServers, tr.APIKeySource = true, ev.Tools, len(ev.MCPServers), ev.APIKeySource
			}
		case "assistant", "user":
			var m struct {
				Content []struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Name      string          `json:"name"`
					Input     map[string]any  `json:"input"`
					ToolUseID string          `json:"tool_use_id"`
					IsError   bool            `json:"is_error"`
					Content   json.RawMessage `json:"content"`
				} `json:"content"`
			}
			if json.Unmarshal(ev.Message, &m) != nil {
				continue
			}
			for _, c := range m.Content {
				switch c.Type {
				case "tool_use":
					u := &toolUse{ID: c.ID, Name: c.Name, Input: c.Input}
					tr.Uses = append(tr.Uses, u)
					byID[c.ID] = u
				case "tool_result":
					if u := byID[c.ToolUseID]; u != nil {
						u.Done, u.IsError, u.Output = true, c.IsError, contentText(c.Content)
					}
				}
			}
		case "result":
			tr.HasResult = true
			for _, d := range ev.Denials {
				if u := byID[d.ToolUseID]; u != nil {
					u.Denied = true
				}
			}
		}
	}
	return tr
}

// contentText flattens a tool_result content, a string or a list of text blocks.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, x := range blocks {
			b.WriteString(x.Text)
		}
		return b.String()
	}
	return string(raw)
}

// resolved reports whether the CLI accounted for a tool call: a result
// arrived, or permissions denied it. A tool_use with neither is unresolved,
// and a negative control cannot be certified on it.
func (u *toolUse) resolved() bool { return u != nil && (u.Done || u.Denied) }

// refusedByCLI reports whether the CLI confirmed that a tool call did not do
// its work: permissions denied it, or its result is an error.
func (u *toolUse) refusedByCLI() bool { return u != nil && (u.Denied || (u.Done && u.IsError)) }

// succeeded reports whether the CLI ran a tool call and returned a result
// without an error or a permission denial.
func (u *toolUse) succeeded() bool { return u != nil && u.Done && !u.IsError && !u.Denied }

func inputString(u *toolUse, key string) string {
	if u == nil || u.Input == nil {
		return ""
	}
	s, _ := u.Input[key].(string)
	return s
}

// find returns the first use of a tool whose input satisfies match.
func (tr *claudeTrace) find(name string, match func(*toolUse) bool) *toolUse {
	for _, u := range tr.Uses {
		if u.Name == name && match(u) {
			return u
		}
	}
	return nil
}

// codexCommand is one command the reviewer's stream shows.
type codexCommand struct {
	Command  string
	ExitCode *int
	Status   string
	Output   string
}

// finished reports whether the CLI ran a command to its end and reported its
// exit code. A command that only started, or that the CLI declined, proves
// nothing about what it would have done.
func (c *codexCommand) finished() bool {
	return c != nil && c.ExitCode != nil && (c.Status == "completed" || c.Status == "failed")
}

// refusedByCLI reports whether the CLI confirmed that a command failed: it
// finished with a non-zero exit code.
func (c *codexCommand) refusedByCLI() bool { return c.finished() && *c.ExitCode != 0 }

// codexChange is one file a file_change item of the reviewer's stream names.
type codexChange struct {
	Path   string
	Kind   string
	Status string
}

// codexTrace is what the reviewer's --json stream shows. Every item counts
// from its first event: a command or file change that started and never
// completed was still an operation of the session.
type codexTrace struct {
	Completed bool
	Commands  []codexCommand
	Changes   []codexChange
	Collab    bool
	Others    []string // item types that are tool operations the probe does not list
}

// codexMessages are item types that report the session's output, not an
// operation of a tool.
var codexMessages = []string{"agent_message", "reasoning", "todo_list", "error"}

func parseCodex(stdout string) *codexTrace {
	tr := &codexTrace{}
	type change struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	}
	type item struct {
		ID         string   `json:"id"`
		Type       string   `json:"type"`
		Command    string   `json:"command"`
		ExitCode   *int     `json:"exit_code"`
		Status     string   `json:"status"`
		Aggregated string   `json:"aggregated_output"`
		Changes    []change `json:"changes"`
	}
	var order []string
	items := map[string]*item{}
	for n, line := range readLines(stdout) {
		var ev struct {
			Type string `json:"type"`
			Item *item  `json:"item"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type == "turn.completed" {
			tr.Completed = true
		}
		if !strings.HasPrefix(ev.Type, "item.") || ev.Item == nil {
			continue
		}
		// The events of one item share its id and type; an event without an
		// id is an item of its own.
		key := ev.Item.Type + "/" + ev.Item.ID
		if ev.Item.ID == "" {
			key = fmt.Sprintf("%s/#%d", ev.Item.Type, n)
		}
		cur, ok := items[key]
		if !ok {
			order = append(order, key)
			items[key] = ev.Item
			continue
		}
		// A later event updates the item; the files it names accumulate.
		changes := append(cur.Changes, ev.Item.Changes...)
		if ev.Item.Command == "" {
			ev.Item.Command = cur.Command
		}
		*cur = *ev.Item
		cur.Changes = changes
	}
	for _, key := range order {
		it := items[key]
		switch {
		case it.Type == "command_execution":
			tr.Commands = append(tr.Commands, codexCommand{Command: it.Command, ExitCode: it.ExitCode, Status: it.Status, Output: it.Aggregated})
		case it.Type == "file_change":
			if len(it.Changes) == 0 {
				tr.Changes = append(tr.Changes, codexChange{Status: it.Status})
			}
			seen := map[change]bool{}
			for _, c := range it.Changes {
				if !seen[c] {
					seen[c] = true
					tr.Changes = append(tr.Changes, codexChange{Path: c.Path, Kind: c.Kind, Status: it.Status})
				}
			}
		case it.Type == "collab_tool_call":
			tr.Collab = true
			tr.Others = append(tr.Others, it.Type)
		case slices.Contains(codexMessages, it.Type):
		default:
			tr.Others = append(tr.Others, it.Type)
		}
	}
	return tr
}

func (tr *codexTrace) find(sub string) *codexCommand {
	for i := range tr.Commands {
		if strings.Contains(tr.Commands[i].Command, sub) {
			return &tr.Commands[i]
		}
	}
	return nil
}
