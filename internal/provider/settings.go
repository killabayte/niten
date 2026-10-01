package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SettingsPaths are the absolute paths substituted into the Claude settings
// template. ContextCopy is empty in single-repository runs: every template
// entry that mentions it is removed as a whole, never substituted with an
// empty path or a wildcard.
type SettingsPaths struct {
	ExecutorScratch string
	OriginalRepo    string
	Store           string
	GitDir          string
	Source          string
	ClaudeHome      string
	CodexHome       string
	ContextCopy     string
	// DenyPatterns are policy path patterns relative to Source (protected
	// paths and untargeted instruction paths); each becomes Edit and Write
	// deny rules for the file tools.
	DenyPatterns []string
}

var rePlaceholder = regexp.MustCompile(`\{\{([A-Z_]+)\}\}`)

// RenderSettings turns the settings template into the JSON the executor gets.
// An unknown placeholder, a relative or unclean path, or a placeholder left
// unresolved refuses the render.
func RenderSettings(template []byte, p SettingsPaths) ([]byte, error) {
	values := map[string]string{
		"EXECUTOR_SCRATCH": p.ExecutorScratch, "ORIGINAL_REPO": p.OriginalRepo, "STORE": p.Store, "GITDIR": p.GitDir,
		"SOURCE": p.Source, "CLAUDE_HOME": p.ClaudeHome, "CODEX_HOME": p.CodexHome, "CONTEXT_COPY": p.ContextCopy,
	}
	for k, v := range values {
		if v == "" && k == "CONTEXT_COPY" {
			continue
		}
		if !filepath.IsAbs(v) || filepath.Clean(v) != v || strings.ContainsAny(v, "*?[{}\"\n") {
			return nil, fmt.Errorf("settings path %s=%q must be a clean absolute path", k, v)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(template))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("settings template: %w", err)
	}
	var bad []string
	out := walk(doc, func(s string) (string, bool) {
		if strings.Contains(s, "{{CONTEXT_COPY}}") && p.ContextCopy == "" {
			return "", false
		}
		return rePlaceholder.ReplaceAllStringFunc(s, func(m string) string {
			name := rePlaceholder.FindStringSubmatch(m)[1]
			v, ok := values[name]
			if !ok || v == "" {
				bad = append(bad, m)
				return m
			}
			return v
		}), true
	})
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, fmt.Errorf("settings template: unresolved placeholder(s) %s", strings.Join(bad, ", "))
	}
	if root, ok := out.(map[string]any); ok && len(p.DenyPatterns) > 0 {
		perms, _ := root["permissions"].(map[string]any)
		if perms == nil {
			perms = map[string]any{}
			root["permissions"] = perms
		}
		deny, _ := perms["deny"].([]any)
		for _, pat := range p.DenyPatterns {
			if strings.HasPrefix(pat, "/") || strings.Contains(pat, "..") {
				return nil, fmt.Errorf("deny pattern %q must be relative to the source", pat)
			}
			for _, tool := range []string{"Edit", "Write"} {
				deny = append(deny, fmt.Sprintf("%s(/%s/%s)", tool, p.Source, pat))
			}
		}
		perms["deny"] = deny
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	if rePlaceholder.Match(b) {
		return nil, fmt.Errorf("settings: a placeholder survived the render")
	}
	return append(b, '\n'), nil
}

// walk rebuilds a decoded JSON value, mapping every string; f returning false
// drops the string from its array (or the key from its object).
func walk(v any, f func(string) (string, bool)) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			if s, ok := x.(string); ok {
				if ns, keep := f(s); keep {
					out[k] = ns
				}
				continue
			}
			out[k] = walk(x, f)
		}
		return out
	case []any:
		out := []any{}
		for _, x := range t {
			if s, ok := x.(string); ok {
				if ns, keep := f(s); keep {
					out = append(out, ns)
				}
				continue
			}
			out = append(out, walk(x, f))
		}
		return out
	}
	return v
}
