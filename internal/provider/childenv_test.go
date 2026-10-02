package provider

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestChildEnvPinsScratchAndStripsKeys(t *testing.T) {
	scratch := t.TempDir()
	env, stripped, err := ChildEnv([]string{"HOME=/h", "TMPDIR=/var/x", "ANTHROPIC_API_KEY=secret", "GOCACHE=/shared", "EXTRA_SECRET=1"}, []string{"EXTRA_SECRET"}, scratch, "RUST_LOG=x")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"HOME=/h", "TMPDIR=" + filepath.Join(scratch, "tmp"), "GOCACHE=" + filepath.Join(scratch, "gocache"), "GOTOOLCHAIN=local", "RUST_LOG=x"} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %s in\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "secret") || strings.Contains(joined, "/shared") || strings.Contains(joined, "/var/x") || strings.Contains(joined, "EXTRA_SECRET") {
		t.Fatalf("a removed or replaced value survived:\n%s", joined)
	}
	if !slices.Equal(stripped, []string{"ANTHROPIC_API_KEY", "EXTRA_SECRET"}) {
		t.Fatalf("stripped %v", stripped)
	}
}

func TestBashRules(t *testing.T) {
	got := BashRules([][]string{{"go", "test", "./..."}, {"go", "test", "-run", "X"}, {"make"}})
	want := []string{"Bash(go test:*)", "Bash(make:*)", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git blame:*)", "Bash(git status:*)"}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}

// A model's git must work on its own working copy: GIT_DIR, GIT_WORK_TREE and
// every other git variable of the coordinator never reach it.
func TestChildEnvRemovesGitVariables(t *testing.T) {
	env, stripped, err := ChildEnv([]string{"GIT_DIR=/elsewhere/.git", "GIT_WORK_TREE=/elsewhere", "GIT_INDEX_FILE=/x", "HOME=/h"}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_") {
			t.Fatalf("%s reached the model", kv)
		}
	}
	if !slices.Equal(stripped, []string{"GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE"}) {
		t.Fatalf("stripped %v", stripped)
	}
}
