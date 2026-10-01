// Package pathglob matches repository-relative slash paths against the policy patterns
// used for protected and instruction paths: "**" spans any number of whole segments
// (including none), and every other segment is a path.Match pattern.
package pathglob

import (
	"path"
	"strings"
)

// Valid reports whether pattern is well formed: relative, no empty or ".." segments, and
// every non-"**" segment accepted by path.Match.
func Valid(pattern string) bool {
	if pattern == "" || strings.HasPrefix(pattern, "/") {
		return false
	}
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return false
		}
	}
	return true
}

// Match reports whether the relative slash path name matches pattern.
func Match(pattern, name string) bool {
	return match(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

// MatchAny returns the first pattern that matches name, or "".
func MatchAny(patterns []string, name string) string {
	for _, p := range patterns {
		if Match(p, name) {
			return p
		}
	}
	return ""
}

func match(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			for i := 0; i <= len(segs); i++ {
				if match(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
