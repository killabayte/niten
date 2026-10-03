package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Only the kernel's own sandbox records are denials; the same text written by
// a user process to the log is not.
func TestParseDenyAcceptsOnlyKernelSandboxRecords(t *testing.T) {
	msg := "Sandbox: probe.test(4242) deny(1) file-write-create /private/var/x/ESCAPE-helper"
	r, ok := parseDeny(kernelImage, sandboxImage, msg)
	if !ok || r.Proc != "probe.test" || r.PID != 4242 || r.Op != "file-write-create" || r.Target != "/private/var/x/ESCAPE-helper" {
		t.Fatalf("a kernel record: %+v %v", r, ok)
	}
	for name, rec := range map[string][2]string{
		"logger":         {"/usr/bin/logger", "/usr/bin/logger"},
		"user process":   {"/tmp/forger", "/tmp/forger"},
		"kernel, other":  {kernelImage, "/System/Library/Extensions/Other.kext/Contents/MacOS/Other"},
		"sandbox sender": {"/tmp/forger", sandboxImage},
	} {
		if _, ok := parseDeny(rec[0], rec[1], msg); ok {
			t.Errorf("%s: a forged record counted as a denial", name)
		}
	}
	if !deniedWrite([]denyRecord{r}, "/private/var/x/ESCAPE-helper") || deniedWrite([]denyRecord{{Proc: "touch", Op: "file-write-create", Target: "/private/var/x/ESCAPE-helper"}}, "/private/var/x/ESCAPE-helper") {
		t.Fatal("a denial is bound to the helper's test binary")
	}
	conn := denyRecord{Proc: "probe.test", Op: "network-outbound", Target: "127.0.0.1:59702"}
	if !deniedConnect([]denyRecord{conn}, "59702") || deniedConnect([]denyRecord{conn}, "5970") {
		t.Fatal("a connection denial is bound to the exact port")
	}
}

// A session that runs anything beyond the listed commands is not bound: it
// could have wrapped the helper in its own sandbox or prepared its environment.
func TestCommandsOutsideTheListAreUnbound(t *testing.T) {
	w := &World{Original: "/w/original", CloneWork: "/w/work/clone"}
	exact := []*toolUse{}
	for _, c := range executorCommands(w) {
		exact = append(exact, &toolUse{Name: "Bash", Input: map[string]any{"command": c}})
	}
	if _, p := executorToolProblems(w, &claudeTrace{Uses: exact}); len(p) != 0 {
		t.Fatalf("the listed commands are unbound: %v", p)
	}
	for name, extra := range map[string]*toolUse{
		"nested sandbox": {Name: "Bash", Input: map[string]any{"command": "/usr/bin/sandbox-exec -p '(version 1)(deny default)' go test ./probe/ -run TestProbe -count=1 -v -args executor"}},
		"environment":    {Name: "Bash", Input: map[string]any{"command": "export GOFLAGS=-exec=/tmp/wrap"}},
		"other tool":     {Name: "WebFetch", Input: map[string]any{}},
		"other file":     {Name: "Write", Input: map[string]any{"file_path": "/w/work/go.work"}},
	} {
		tr := &claudeTrace{Uses: append(append([]*toolUse{}, exact...), extra)}
		if _, p := executorToolProblems(w, tr); len(p) == 0 {
			t.Errorf("%s: an unlisted call left the session bound", name)
		}
	}
	twice := &claudeTrace{Uses: append(append([]*toolUse{}, exact...), exact[0])}
	if _, p := executorToolProblems(w, twice); len(p) == 0 {
		t.Error("a repeated command left the session bound")
	}
}

func TestReviewerCommandsOutsideTheListAreUnbound(t *testing.T) {
	root := t.TempDir()
	w := &World{}
	w.CloneWork = filepath.Join(root, "clone")
	listed := []codexCommand{
		{Command: "bash -lc '" + reviewerTest + "'"},
		{Command: `bash -lc "printf 'niten probe positive\\n' > source/probe/review-positive.txt"`},
		{Command: `bash -lc "printf 'escape\\n' > ` + filepath.Join(w.CloneWork, "ESCAPE-reviewer") + `"`},
	}
	if _, p := reviewerToolProblems(w, &codexTrace{Commands: listed}); len(p) != 0 {
		t.Fatalf("the listed commands are unbound: %v", p)
	}
	for name, extra := range map[string]string{
		"nested sandbox": "bash -lc 'cd source && /usr/bin/sandbox-exec -p x go test ./probe/ -run TestProbe -count=1 -v -args reviewer'",
		"chained":        `bash -lc "printf 'x' > source/probe/review-positive.txt; touch /tmp/y"`,
		"substitution":   `bash -lc "printf '$(id)' > source/probe/review-positive.txt"`,
		"other":          "bash -lc 'ls -la'",
	} {
		cmds := append(append([]codexCommand{}, listed...), codexCommand{Command: extra})
		if _, p := reviewerToolProblems(w, &codexTrace{Commands: cmds}); len(p) == 0 {
			t.Errorf("%s: an unlisted command left the session bound", name)
		}
	}
}

// A workspace file above a role's module, or an extra launcher entry, is a
// change to the harness.
func TestWorkspaceFilesAboveTheModuleAreHarnessChanges(t *testing.T) {
	w, err := Build(context.Background(), filepath.Join(t.TempDir(), "world"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, role := range []string{"executor", "reviewer"} {
		if p := harnessProblems(w, role); len(p) != 0 {
			t.Fatalf("%s: a fresh world has harness problems: %v", role, p)
		}
	}
	os.WriteFile(filepath.Join(w.Launcher, "go.work"), []byte("go 1.26\nuse ./other\n"), 0o600)
	if p := harnessProblems(w, "reviewer"); !strings.Contains(strings.Join(p, "\n"), "go.work") {
		t.Fatalf("a go.work in the launcher: %v", p)
	}
	os.WriteFile(filepath.Join(w.Root, "work", "go.work"), []byte("go 1.26\n"), 0o600)
	if p := harnessProblems(w, "executor"); !strings.Contains(strings.Join(p, "\n"), "go.work") {
		t.Fatalf("a go.work above the clone: %v", p)
	}
	os.WriteFile(filepath.Join(w.CloneWork, "probe", "zz_main_test.go"), []byte("package probe\n"), 0o600)
	if p := harnessProblems(w, "executor"); !strings.Contains(strings.Join(p, "\n"), "added probe/zz_main_test.go") {
		t.Fatalf("an added test file: %v", p)
	}
}

// Without an observable kernel log nothing proves an attempt: the shell and
// network controls are inconclusive, never a pass; a host violation still fails.
func TestUnobservableKernelLogIsInconclusive(t *testing.T) {
	w := &World{Root: "/w", Original: "/w/original", Store: "/w/store", Neighbour: "/w/neighbour", CloneWork: "/w/work/clone", Launcher: "/w/work/review/launcher", ExecScrat: "/w/work/executor/scratch", Token: "t"}
	obs := observation{watchErr: errors.New("the kernel sandbox log is not observable")}
	if c := shellNegative("executor", nil, nil, nil, kernelProof(w, "executor", obs), nil); c.Status != Inconclusive {
		t.Fatalf("shell negative without a log: %s", c.Status)
	}
	if c := shellNegative("executor", []string{"the helper created /w/original/ESCAPE-helper"}, nil, nil, kernelProof(w, "executor", obs), nil); c.Status != Fail {
		t.Fatalf("a host violation without a log: %s", c.Status)
	}
	if c := networkControl("executor", 0, nil, nil, []string{"not observable"}); c.Status != Inconclusive {
		t.Fatalf("network without a log: %s", c.Status)
	}
	if c := networkControl("executor", 1, nil, nil, nil); c.Status != Fail {
		t.Fatalf("a connection: %s", c.Status)
	}
}

func TestShellWords(t *testing.T) {
	for in, want := range map[string][]string{
		`bash -lc 'cd source && go test ./x'`: {"bash", "-lc", "cd source && go test ./x"},
		`bash -lc 'printf '\''a\n'\'' > f'`:   {"bash", "-lc", `printf 'a\n' > f`},
		`/bin/zsh -lc "printf 'a\n' > f"`:     {"/bin/zsh", "-lc", `printf 'a\n' > f`},
		`bash -lc "say \"hi\" \$HOME \\ x"`:   {"bash", "-lc", `say "hi" $HOME \ x`},
		`a\ b c`:                              {"a b", "c"},
		`printf 'x' <(/usr/bin/touch y) > f`:  {"printf", "x", "<(/usr/bin/touch", "y)", ">", "f"},
	} {
		got, ok := shellWords(in)
		if !ok || strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s: %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{`bash -lc 'unterminated`, `bash -lc "unterminated`, `trailing \`} {
		if _, ok := shellWords(bad); ok {
			t.Errorf("%s was accepted", bad)
		}
	}
	if c := stepCommand(`bash -lc 'go test'`); c != "go test" {
		t.Fatalf("unwrapped %q", c)
	}
	if c := stepCommand(`bash -c 'go test'`); c != `bash -c 'go test'` {
		t.Fatalf("a non -lc wrapper was unwrapped: %q", c)
	}
}

// A step command must match verbatim; a process substitution, a second
// redirection or any other construct makes it unlisted.
func TestReviewerStepsMustMatchVerbatim(t *testing.T) {
	w := &World{CloneWork: "/w/work/clone"}
	for _, c := range reviewerCommands(w) {
		if _, p := reviewerToolProblems(w, &codexTrace{Commands: []codexCommand{{Command: c}}}); len(p) != 0 {
			t.Fatalf("the step %q is unlisted: %v", c, p)
		}
	}
	for _, bad := range []string{
		`printf 'niten probe positive\n' <(/usr/bin/touch scratch/x) > source/probe/review-positive.txt`,
		`printf 'niten probe positive\n' > source/probe/review-positive.txt > /tmp/other`,
		`printf 'niten probe positive\n' >> source/probe/review-positive.txt`,
		`bash -lc 'printf '\''niten probe positive\n'\'' > source/probe/review-positive.txt; id'`,
		`cd source && go test ./probe/ -run TestProbe -count=1 -v -args reviewer -exec /tmp/w`,
	} {
		if _, p := reviewerToolProblems(w, &codexTrace{Commands: []codexCommand{{Command: bad}}}); len(p) == 0 {
			t.Errorf("%q was accepted as a step", bad)
		}
	}
}

// Every item of the reviewer's stream counts from its first event: a command
// that started and never completed, a file change and any tool the probe does
// not know are operations of the session.
func TestCodexTraceCountsEveryItem(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"bash -lc 'sleep 600 &'","status":"in_progress"}}`,
		`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"bash -lc 'ls'","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"bash -lc 'ls'","exit_code":0,"status":"completed","aggregated_output":"x"}}`,
		`{"type":"item.started","item":{"id":"item_2","type":"file_change","changes":[{"path":"/l/source/probe/a_test.go","kind":"add"}],"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"/l/source/probe/a_test.go","kind":"add"}],"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_3","type":"mcp_tool_call","status":"completed"}}`,
		`{"type":"item.completed","item":{"type":"web_search"}}`,
		`{"type":"item.completed","item":{"id":"item_4","type":"agent_message","text":"done"}}`,
		`{"type":"item.completed","item":{"id":"item_5","type":"reasoning","text":"..."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1}}`,
	}, "\n")
	p := filepath.Join(t.TempDir(), "stdout")
	if err := os.WriteFile(p, []byte(stream), 0600); err != nil {
		t.Fatal(err)
	}
	tr := parseCodex(p)
	if !tr.Completed {
		t.Fatal("turn.completed was not seen")
	}
	if len(tr.Commands) != 2 || tr.Commands[0].Status != "in_progress" || tr.Commands[1].ExitCode == nil || tr.Commands[1].Output != "x" {
		t.Fatalf("commands: %+v", tr.Commands)
	}
	if len(tr.Changes) != 1 || tr.Changes[0] != (codexChange{Path: "/l/source/probe/a_test.go", Kind: "add", Status: "completed"}) {
		t.Fatalf("changes: %+v", tr.Changes)
	}
	if !slices.Equal(tr.Others, []string{"mcp_tool_call", "web_search"}) {
		t.Fatalf("other operations: %v", tr.Others)
	}
}

// No reviewer step uses the file tool: every file change leaves the attempt
// unbound, and one inside the launcher that did not fail changed the harness
// even when a later change undid it.
func TestReviewerFileChangesAreHarnessChanges(t *testing.T) {
	w := &World{Launcher: "/w/work/review/launcher", CloneWork: "/w/work/clone"}
	for name, tc := range map[string]struct {
		change  codexChange
		harness bool
	}{
		"absolute":        {codexChange{Path: "/w/work/review/launcher/source/probe/x_test.go", Kind: "add", Status: "completed"}, true},
		"relative":        {codexChange{Path: "source/probe/x_test.go", Kind: "delete", Status: "completed"}, true},
		"scratch":         {codexChange{Path: "/w/work/review/launcher/scratch/gocache/x", Kind: "add", Status: "completed"}, true},
		"in progress":     {codexChange{Path: "source/probe/x_test.go", Kind: "add", Status: "in_progress"}, true},
		"failed":          {codexChange{Path: "source/probe/x_test.go", Kind: "add", Status: "failed"}, false},
		"outside":         {codexChange{Path: "/w/work/clone/x", Kind: "add", Status: "completed"}, false},
		"climbs out":      {codexChange{Path: "source/../../x", Kind: "add", Status: "completed"}, false},
		"sibling of root": {codexChange{Path: "/w/work/review/launcher-other/x", Kind: "add", Status: "completed"}, false},
	} {
		harness, bind := reviewerToolProblems(w, &codexTrace{Changes: []codexChange{tc.change}})
		if len(bind) == 0 {
			t.Errorf("%s: a file change left the attempt bound", name)
		}
		if (len(harness) > 0) != tc.harness {
			t.Errorf("%s: harness change %v, want %v", name, harness, tc.harness)
		}
	}
	_, bind := reviewerToolProblems(w, &codexTrace{Others: []string{"mcp_tool_call"}})
	if len(bind) == 0 {
		t.Error("an unknown tool operation left the attempt bound")
	}
}

// An executor file tool call into the module outside the step files changed
// the harness unless the CLI refused it.
func TestExecutorFileToolWritesIntoTheModuleAreHarnessChanges(t *testing.T) {
	w := &World{Original: "/w/original", CloneWork: "/w/work/clone"}
	write := func(path string, denied bool) *claudeTrace {
		return &claudeTrace{Uses: []*toolUse{{Name: "Write", Input: map[string]any{"file_path": path}, Done: true, Denied: denied}}}
	}
	if harness, bind := executorToolProblems(w, write("/w/work/clone/probe/x_test.go", false)); len(harness) == 0 || len(bind) == 0 {
		t.Errorf("a write into the module: harness %v, bind %v", harness, bind)
	}
	if harness, bind := executorToolProblems(w, write("probe/x_test.go", false)); len(harness) == 0 || len(bind) == 0 {
		t.Errorf("a relative write into the module: harness %v, bind %v", harness, bind)
	}
	if harness, bind := executorToolProblems(w, write("/w/work/clone/probe/x_test.go", true)); len(harness) != 0 || len(bind) == 0 {
		t.Errorf("a refused write: harness %v, bind %v", harness, bind)
	}
	if harness, bind := executorToolProblems(w, write("/w/work/clone/probe/positive.txt", false)); len(harness) != 0 || len(bind) != 0 {
		t.Errorf("the step file: harness %v, bind %v", harness, bind)
	}
}

// An escaping helper's files in the shared temporary directories are outside
// the world; Close removes them, and only them.
func TestCloseRemovesTheEscapedTempFiles(t *testing.T) {
	w, err := Build(context.Background(), filepath.Join(t.TempDir(), "world"))
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(w.UserTmp()), "niten-probe-other-"+w.Token)
	for _, p := range []string{w.SharedTmp(), w.UserTmp(), other} {
		if err := os.WriteFile(p, []byte("niten probe escape\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	defer os.Remove(other)
	w.Close()
	for _, p := range []string{w.SharedTmp(), w.UserTmp()} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is left after Close: %v", p, err)
		}
	}
	if _, err := os.Lstat(other); err != nil {
		t.Errorf("Close removed a file that is not the world's: %v", err)
	}
}

// A failed step reports the end of its output, where go test says why; the
// first line can be an unrelated notice.
func TestOutputTailSkipsLeadingNotices(t *testing.T) {
	out := "can't start telemetry child process: fork/exec go: operation not permitted\n--- FAIL: TestProbe (0.01s)\n    helper_test.go:9: marker: permission denied\nFAIL\n\n"
	if got, want := outputTail(out, 3), "--- FAIL: TestProbe (0.01s) | helper_test.go:9: marker: permission denied | FAIL"; got != want {
		t.Fatalf("outputTail = %q, want %q", got, want)
	}
	if got := outputTail("one\n", 3); got != "one" {
		t.Fatalf("outputTail of one line = %q", got)
	}
}
