package pathglob

import "testing"

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{".git", ".git", true},
		{"**/.git", ".git", true},
		{"**/.git", "sub/dir/.git", true},
		{"**/.git/**", "sub/.git/config", true},
		{".claude/**", ".claude", true},
		{".claude/**", ".claude/settings.json", true},
		{".claude/**", "x/.claude/settings.json", false},
		{"**/.claude/**", "x/.claude/settings.json", true},
		{"AGENTS.md", "AGENTS.md", true},
		{"AGENTS.md", "docs/AGENTS.md", false},
		{"**/AGENTS.md", "docs/AGENTS.md", true},
		{"**/AGENTS.md", "docs/AGENTS.md.bak", false},
		{"**/*.json", "a/b/c.json", true},
		{"cmd/*/main.go", "cmd/niten/main.go", true},
		{"cmd/*/main.go", "cmd/a/b/main.go", false},
		{"**/**/x", "x", true},
		{".mcp.json", "sub/.mcp.json", false},
	} {
		if got := Match(tc.pattern, tc.name); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestValid(t *testing.T) {
	for p, want := range map[string]bool{
		".git": true, "**/.git/**": true, "a/*.go": true, "": false, "/abs": false,
		"a//b": false, "../x": false, "a/./b": false, "[": false,
	} {
		if got := Valid(p); got != want {
			t.Errorf("Valid(%q) = %v, want %v", p, got, want)
		}
	}
}
