// Package testutil holds helpers shared by Niten's tests: the reproducible fixture
// repository behind the Shogun triplets in internal/plan/testdata/shogun, copies of those
// triplets, and a scripted stand-in for the shogun executable. It is imported by tests only.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// FixtureHead is the base commit every fixture triplet was planned against.
const FixtureHead = "99e8a297c715085c31a6498375964f8e7ca1f237"

// IsolateGit makes git ignore the user's global and system configuration for the rest of
// the test, so fixture commits and fingerprints are reproducible.
func IsolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// Root is the repository root of Niten's source tree.
func Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// FixtureDir is internal/plan/testdata/shogun/<name>.
func FixtureDir(name string) string {
	return filepath.Join(Root(), "internal", "plan", "testdata", "shogun", name)
}

// Git runs git in dir with fixed identity and dates and fails the test on error.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=niten@example.invalid", "-c", "user.name=niten"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_DATE=2026-09-30T12:00:00Z", "GIT_COMMITTER_DATE=2026-09-30T12:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// FixtureRepo rebuilds the demo repository the fixtures were planned against, at
// <parent>/demo, and checks that its HEAD is FixtureHead.
func FixtureRepo(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	Git(t, dir, "init", "-q")
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-qm", "fixture base")
	if head := Git(t, dir, "rev-parse", "HEAD"); head != FixtureHead {
		t.Fatalf("fixture repository HEAD %s, want %s: the fixture commit is not reproducible here", head, FixtureHead)
	}
	return dir
}

// CalcHead is the base commit of the go-two-step fixture: a small Go module with a
// passing test, used by the engine's end-to-end tests.
const CalcHead = "bdffc24a02d55307d6907485cf14fe665d5b3c82"

// CalcFiles are the files of the go-two-step fixture repository.
var CalcFiles = map[string]string{
	"go.mod":       "module example.com/calc\n\ngo 1.26\n",
	"calc.go":      "package calc\n\n// Add returns the sum of a and b.\nfunc Add(a, b int) int { return a + b }\n",
	"calc_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"Add(2, 3) != 5\")\n\t}\n}\n",
}

// CalcRepo rebuilds the go-two-step fixture repository at <parent>/calc and checks that
// its HEAD is CalcHead.
func CalcRepo(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "calc")
	for p, c := range CalcFiles {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	Git(t, dir, "init", "-q")
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-qm", "fixture base")
	if head := Git(t, dir, "rev-parse", "HEAD"); head != CalcHead {
		t.Fatalf("calc fixture HEAD %s, want %s: the fixture commit is not reproducible here", head, CalcHead)
	}
	return dir
}

// CopyFixture copies the named triplet into dir as plan.md, plan.approval.json and
// plan.manifest.json and returns the plan path.
func CopyFixture(t *testing.T, name, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"plan.md", "plan.approval.json", "plan.manifest.json"} {
		b, err := os.ReadFile(filepath.Join(FixtureDir(name), f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "plan.md")
}

// Shogun modes of the scripted stand-in.
const (
	ShogunValid   = "valid"   // verify answers valid, exit 0
	ShogunChanged = "changed" // verify answers changed, exit 1
	ShogunOld     = "old"     // a pre-S0 shogun: --require-manifest is an unknown flag
)

// FakeShogun writes an executable script that answers `version` and `verify` in the given
// mode and appends every argument list to log. It returns the script path.
func FakeShogun(t *testing.T, dir, mode, log string) string {
	t.Helper()
	var verify string
	switch mode {
	case ShogunValid:
		verify = `printf 'valid\tscripted\n'; exit 0`
	case ShogunChanged:
		verify = `printf 'changed\tbody changed\n'; exit 1`
	case ShogunOld:
		verify = `[ "$2" = "--require-manifest" ] && { echo 'flag provided but not defined: -require-manifest' >&2; exit 2; }; printf 'valid\tx\n'`
	default:
		t.Fatalf("unknown fake shogun mode %q", mode)
	}
	script := "#!/bin/sh\necho \"$@\" >> '" + log + "'\ncase \"$1\" in\nversion) echo 'shogun 0.0.0-scripted test'; exit 0;;\nverify) " + verify + ";;\nesac\nexit 2\n"
	p := filepath.Join(dir, "shogun-"+mode)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Marker writes an executable that records its invocation by creating marker; used to
// prove that a command such as claude or codex was never started.
func Marker(t *testing.T, dir, name, marker string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
