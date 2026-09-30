package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPolicy(t *testing.T) (Policy, string) {
	t.Helper()
	base := t.TempDir()
	src := filepath.Join(base, "src")
	scratch := filepath.Join(base, "scratch")
	tool := filepath.Join(base, "toolchain")
	for _, d := range []string{src, scratch, tool} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return Policy{SourceRoot: src, ScratchRoot: scratch, Toolchains: []string{tool}}, base
}

func TestRenderDeterministic(t *testing.T) {
	p, _ := testPolicy(t)
	n, err := p.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	a, b := Render(n), Render(n)
	if a != b {
		t.Fatal("render is not deterministic")
	}
	for _, want := range []string{
		"(version 1)\n(deny default)\n",
		`(literal "/")`,
		`(subpath "` + n.SourceRoot + `")`,
		`(subpath "` + n.ScratchRoot + `")`,
		`(subpath "` + n.Toolchains[0] + `")`,
		`(deny file-read*` + "\n" + `  (subpath "/Library/Keychains")`,
		"(deny network*)\n",
		`(allow mach-lookup (global-name "com.apple.system.opendirectoryd.libinfo"))`,
	} {
		if !strings.Contains(a, want) {
			t.Errorf("profile lacks %q:\n%s", want, a)
		}
	}
	if strings.Contains(a, "(allow mach-lookup)") || strings.Contains(a, "(allow default)") {
		t.Fatalf("profile is too permissive:\n%s", a)
	}
	if len(Digest(a)) != 64 {
		t.Fatal("digest is not hex sha256")
	}
}

func TestNormalizeRejects(t *testing.T) {
	p, base := testPolicy(t)
	nested := filepath.Join(p.SourceRoot, "inner")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	quoted := filepath.Join(base, `we"ird`)
	if err := os.MkdirAll(quoted, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := map[string]Policy{
		"empty source":        {ScratchRoot: p.ScratchRoot},
		"relative scratch":    {SourceRoot: p.SourceRoot, ScratchRoot: "scratch"},
		"same roots":          {SourceRoot: p.SourceRoot, ScratchRoot: p.SourceRoot},
		"nested roots":        {SourceRoot: p.SourceRoot, ScratchRoot: nested},
		"missing toolchain":   {SourceRoot: p.SourceRoot, ScratchRoot: p.ScratchRoot, Toolchains: []string{filepath.Join(base, "nope")}},
		"toolchain in source": {SourceRoot: p.SourceRoot, ScratchRoot: p.ScratchRoot, Toolchains: []string{nested}},
		"relative denyread":   {SourceRoot: p.SourceRoot, ScratchRoot: p.ScratchRoot, DenyRead: []string{"secrets"}},
		"denyread over source": {SourceRoot: p.SourceRoot, ScratchRoot: p.ScratchRoot,
			DenyRead: []string{filepath.Dir(p.SourceRoot)}},
		"quote in path":     {SourceRoot: quoted, ScratchRoot: p.ScratchRoot},
		"system write root": {SourceRoot: "/usr/local", ScratchRoot: p.ScratchRoot},
	}
	for name, c := range cases {
		if _, err := c.Normalize(); !errors.Is(err, ErrPolicy) {
			t.Errorf("%s: want ErrPolicy, got %v", name, err)
		}
	}
	if _, err := p.Normalize(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
}

func TestNormalizeResolvesSymlinks(t *testing.T) {
	p, base := testPolicy(t)
	link := filepath.Join(base, "src-link")
	if err := os.Symlink(p.SourceRoot, link); err != nil {
		t.Fatal(err)
	}
	n, err := Policy{SourceRoot: link, ScratchRoot: p.ScratchRoot}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(p.SourceRoot)
	if n.SourceRoot != real {
		t.Fatalf("symlink not resolved: %s", n.SourceRoot)
	}
}

func TestEnvironment(t *testing.T) {
	p, base := testPolicy(t)
	ro := filepath.Join(base, "modcache")
	if err := os.MkdirAll(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	p.ReadOnly = []string{ro}
	n, err := p.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	env, err := Environment(n, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{
		"\nHOME=" + filepath.Join(n.ScratchRoot, scratchHome) + "\n",
		"\nGOPROXY=off\n", "\nGOTOOLCHAIN=local\n", "\nCGO_ENABLED=0\n", "\nGOENV=off\n",
		"\nGOCACHE=" + filepath.Join(n.ScratchRoot, scratchGoCache) + "\n",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("environment lacks %q", strings.TrimSpace(want))
		}
	}
	for name, ov := range map[string]map[string]string{
		"protected PATH":        {"PATH": "/tmp"},
		"protected GOTOOLCHAIN": {"GOTOOLCHAIN": "auto"},
		"GOCACHE outside":       {"GOCACHE": base},
		"GOMODCACHE outside":    {"GOMODCACHE": base},
		"newline in value":      {"FOO": "a\nb"},
		"equals in name":        {"A=B": "x"},
	} {
		if _, err := Environment(n, ov); !errors.Is(err, ErrCommand) {
			t.Errorf("%s: want ErrCommand, got %v", name, err)
		}
	}
	env, err = Environment(n, map[string]string{"GOMODCACHE": n.ReadOnly[0], "NITEN_X": "1"})
	if err != nil {
		t.Fatal(err)
	}
	joined = "\n" + strings.Join(env, "\n") + "\n"
	if !strings.Contains(joined, "\nGOMODCACHE="+n.ReadOnly[0]+"\n") || !strings.Contains(joined, "\nNITEN_X=1\n") {
		t.Fatalf("overrides not applied: %v", env)
	}
}

func TestWithin(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"/a/b", "/a", true}, {"/a", "/a", false}, {"/ab", "/a", false}, {"/a/b", "/", true}, {"/", "/", false},
	}
	for _, c := range cases {
		if got := within(c.child, c.parent); got != c.want {
			t.Errorf("within(%s, %s) = %v", c.child, c.parent, got)
		}
	}
}
