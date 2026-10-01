package provider

import (
	"sort"
	"strings"
)

// Built-in names and prefixes that never reach a model's process: API keys
// (they would switch from subscription auth to metered API access) and model
// or effort overrides that would silently replace the requested model. The
// configured strip_env names are added on top; they cannot remove these.
var (
	BuiltinStripNames = []string{
		"CLAUDECODE", "CODEX_API_KEY", "MAX_THINKING_TOKENS", "RUST_LOG",
		"CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_EFFORT_LEVEL", "ANTHROPIC_MODEL",
	}
	BuiltinStripPrefixes = []string{"ANTHROPIC_", "OPENAI_", "CLAUDE_CODE_"}
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
