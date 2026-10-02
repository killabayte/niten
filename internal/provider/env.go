package provider

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Built-in names and prefixes that never reach a model's process: API keys
// (they would switch from subscription auth to metered API access), model or
// effort overrides that would silently replace the requested model, and git's
// own variables (GIT_DIR or GIT_WORK_TREE would point the model's git at
// another repository than its working copy). The
// configured strip_env names are added on top; they cannot remove these.
var (
	BuiltinStripNames = []string{
		"CLAUDECODE", "CODEX_API_KEY", "MAX_THINKING_TOKENS", "RUST_LOG",
		"CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_EFFORT_LEVEL", "ANTHROPIC_MODEL",
	}
	BuiltinStripPrefixes = []string{"ANTHROPIC_", "OPENAI_", "CLAUDE_CODE_", "GIT_"}
)

// FilterEnv returns base without the built-in and extra names, and the sorted
// names it removed. Values are never returned for the removed variables, so
// the list can be recorded with the attempt.
func FilterEnv(base []string, extra []string) (env []string, stripped []string) {
	names := map[string]bool{}
	for _, n := range BuiltinStripNames {
		names[n] = true
	}
	for _, n := range extra {
		names[n] = true
	}
	seen := map[string]bool{}
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		drop := names[k]
		for _, p := range BuiltinStripPrefixes {
			drop = drop || strings.HasPrefix(k, p)
		}
		if drop {
			if !seen[k] {
				stripped = append(stripped, k)
				seen[k] = true
			}
			continue
		}
		env = append(env, kv)
	}
	sort.Strings(stripped)
	if env == nil {
		env = []string{}
	}
	return env, stripped
}

// ChildEnv is a model process environment: base without API keys, model
// overrides and the configured names, with the temporary and Go cache
// directories pinned to the attempt's scratch, plus extra. It returns the
// removed names, never their values.
func ChildEnv(base, strip []string, scratch string, extra ...string) (env, stripped []string, err error) {
	env, stripped = FilterEnv(base, strip)
	for _, d := range []string{"tmp", "gocache"} {
		if err := os.MkdirAll(filepath.Join(scratch, d), 0o700); err != nil {
			return nil, nil, err
		}
	}
	set := map[string]string{
		"TMPDIR": filepath.Join(scratch, "tmp"), "GOTMPDIR": filepath.Join(scratch, "tmp"),
		"GOCACHE": filepath.Join(scratch, "gocache"), "GOTOOLCHAIN": "local",
	}
	out := make([]string, 0, len(env)+len(set)+len(extra))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, replaced := set[k]; !replaced {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	return append(out, extra...), stripped, nil
}

// BashRules turns allowed check commands into Claude Bash permission
// prefixes, plus read-only git inspection. Prefixes are a convenience, not an
// isolation boundary: the settings sandbox is.
func BashRules(argvs [][]string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, argv := range argvs {
		n := min(2, len(argv))
		if n > 0 {
			add("Bash(" + strings.Join(argv[:n], " ") + ":*)")
		}
	}
	for _, g := range []string{"git diff", "git log", "git blame", "git status"} {
		add("Bash(" + g + ":*)")
	}
	return out
}
