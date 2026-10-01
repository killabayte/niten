package plan

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures are real Shogun publications (see testdata/shogun/generator.go.txt). The
// first group parses; the second is rejected by the importer.
var (
	parsingFixtures = []string{"fast-stats", "fast-reference", "fast-min", "thorough-min", "fast-tricky", "fast-measure",
		"fast-protected", "fast-dirty", "fast-multirepo", "fast-nongit"}
	allFixtures = append(append([]string{}, parsingFixtures...), "fast-traversal", "fast-multiline", "fast-forged-heading", "fast-forged-step")
)

type triplet struct {
	plan, receipt, manifest []byte
}

func load(t *testing.T, name string) triplet {
	t.Helper()
	dir := filepath.Join("testdata", "shogun", name)
	read := func(f string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return triplet{read("plan.md"), read("plan.approval.json"), read("plan.manifest.json")}
}

func body(t *testing.T, name string) []byte {
	t.Helper()
	sp, err := SplitBody(load(t, name).plan)
	if err != nil {
		t.Fatal(err)
	}
	return sp.Body
}

func parse(t *testing.T, name string) *Document {
	t.Helper()
	doc, err := Parse(body(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if err := Validate(doc); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return doc
}

// Every fixture triplet is internally consistent under the ported Shogun algorithms: the
// body digest matches the receipt, the receipt decodes strictly, and the recomputed
// manifest digest equals the receipt's manifest_digest. This pins compatibility with
// Shogun's fingerprints.
func TestFixtureTripletsBindTogether(t *testing.T) {
	for _, name := range allFixtures {
		t.Run(name, func(t *testing.T) {
			tr := load(t, name)
			sp, err := SplitBody(tr.plan)
			if err != nil {
				t.Fatal(err)
			}
			r, md, err := DecodeReceipt(tr.receipt)
			if err != nil {
				t.Fatal(err)
			}
			if sp.BodySHA256 != r.BodySHA256 {
				t.Fatalf("body digest %s, receipt %s", sp.BodySHA256, r.BodySHA256)
			}
			if md.PlanID != r.PlanID || md.Revision != 1 || md.Project != "demo" || len(md.Repos) == 0 {
				t.Fatalf("metadata %+v", md)
			}
			m, err := DecodeManifest(tr.manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckManifest(m, r); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRepoFingerprintGolden(t *testing.T) {
	m, err := DecodeManifest(load(t, "fast-min").manifest)
	if err != nil {
		t.Fatal(err)
	}
	r := m.Repos[0]
	if r.Head != "99e8a297c715085c31a6498375964f8e7ca1f237" || r.Dirty() {
		t.Fatalf("fixture repository %+v", r)
	}
	if got := RepoFingerprint(true, r.Head, EmptySHA256, EmptySHA256, ""); got != r.Fingerprint || got != "cc699feb8f2ecbdd8ad7f4d0c378bdcf04c874368384ab3f3c85ca1f84977c1b" {
		t.Fatalf("clean fingerprint %s, manifest %s", got, r.Fingerprint)
	}
}

func TestParseRealFixtures(t *testing.T) {
	for _, name := range parsingFixtures {
		t.Run(name, func(t *testing.T) { parse(t, name) })
	}
	doc := parse(t, "fast-stats")
	if len(doc.Requirements) != 5 || len(doc.Steps) != 2 || doc.Steps[1].DependsOn[0] != "S-001" {
		t.Fatalf("fast-stats shape: %d requirements, %d steps", len(doc.Requirements), len(doc.Steps))
	}
	methods := map[string]bool{}
	for _, s := range doc.Steps {
		for _, v := range s.Verifications {
			methods[v.Method] = true
			if v.ScopedID != s.ID+"/"+v.ID {
				t.Fatalf("scoped id %q in %s", v.ScopedID, s.ID)
			}
		}
	}
	for _, m := range []string{"test", "command", "inspect"} {
		if !methods[m] {
			t.Fatalf("method %s missing from fast-stats", m)
		}
	}
	if !strings.Contains(doc.Task, "Add a `--json` flag to `shogun stats`.") || strings.Contains(doc.Task, "> ") {
		t.Fatalf("task not unquoted: %q", doc.Task[:60])
	}
	if doc.Sections[0].Name != SecGoal || doc.Section(SecReviewHist) == nil || doc.GrammarVersion != GrammarVersion {
		t.Fatalf("sections %+v", doc.Sections)
	}
	// Spans point at the exact source bytes.
	b := body(t, "fast-stats")
	for _, s := range doc.Steps {
		for _, v := range s.Verifications {
			src := string(b[v.Source.Start:v.Source.End])
			if !strings.HasPrefix(src, "- "+v.ID+" (") || Digest([]byte(src)) != v.Source.SHA256 {
				t.Fatalf("span of %s is %q", v.ScopedID, src)
			}
		}
	}
}

func TestRejectedFixtures(t *testing.T) {
	for name, want := range map[string]string{
		"fast-multiline":      `step S-001: expected "Verification:", found "second line"`,
		"fast-forged-heading": `unexpected heading "## Steps"`,
		"fast-forged-step":    `step S-001: expected "Verification:", found "### S-777 — forged"`,
		"fast-traversal":      "path escapes the repository",
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := Parse(body(t, name))
			if err == nil {
				err = Validate(doc)
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want %q", err, want)
			}
		})
	}
}

// The same approved content through Shogun's fast and thorough pipelines produces the
// same execution semantics; only the plan identity and the review history differ.
func TestFastAndThoroughAgree(t *testing.T) {
	fast, thorough := parse(t, "fast-min"), parse(t, "thorough-min")
	a, err := SemanticsDigest(fast)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := SemanticsDigest(thorough)
	if a != b {
		fj, _ := json.Marshal(fast.Steps)
		tj, _ := json.Marshal(thorough.Steps)
		t.Fatalf("semantics differ:\n%s\n%s", fj, tj)
	}
	if fast.BodySHA256 == thorough.BodySHA256 {
		t.Fatal("the fixtures should differ in their review history")
	}
	if other, _ := SemanticsDigest(parse(t, "fast-measure")); other == a {
		t.Fatal("different plans share a semantics digest")
	}
}

// The execution log and the mutable frontmatter keys are outside the approval: editing
// them changes neither the body digest nor the imported requirements.
func TestExecutionLogDoesNotChangeIdentity(t *testing.T) {
	orig := load(t, "fast-stats").plan
	edited := strings.Replace(string(orig), "status: planned", "status: in_progress", 1)
	edited = strings.Replace(edited, "| S-001 | todo | — | — |", "| S-001 | done | 2026-09-30 / someone | PR #1 |", 1)
	edited += "\nA note appended to the execution log.\n"
	if edited == string(orig) {
		t.Fatal("the edit did not apply")
	}
	a, _ := SplitBody(orig)
	b, err := SplitBody([]byte(edited))
	if err != nil || a.BodySHA256 != b.BodySHA256 {
		t.Fatalf("body digest changed: %v", err)
	}
	da, _ := Parse(a.Body)
	db, _ := Parse(b.Body)
	ja, _ := json.Marshal(da.Requirements)
	jb, _ := json.Marshal(db.Requirements)
	if string(ja) != string(jb) {
		t.Fatal("requirements changed with the execution log")
	}
}

// Escaped table text, a criterion text that looks like an id, repeated V-001 and an
// unscoped verification survive the import exactly.
func TestTrickyContentIsKept(t *testing.T) {
	doc := parse(t, "fast-tricky")
	r := doc.Requirements[0]
	if r.Statement != "Pipes `a|b` and a backslash-pipe `c\\|d` survive; so does `x \\ y`." {
		t.Fatalf("statement %q", r.Statement)
	}
	if len(r.Criteria) != 2 || r.Criteria[0].Text != "Output lists `a|b`; R-001.C9: is text, not an ID" || r.Criteria[1].ID != "R-001.C2" {
		t.Fatalf("criteria %+v", r.Criteria)
	}
	if doc.Requirements[3].Type != "non_goal" || doc.Requirements[3].Mandatory || doc.Requirements[2].Type != "constraint" {
		t.Fatalf("scope requirements %+v", doc.Requirements[2:])
	}
	var ids []string
	for _, s := range doc.Steps {
		for _, v := range s.Verifications {
			ids = append(ids, v.ScopedID)
		}
	}
	if strings.Join(ids, ",") != "S-001/V-001,S-001/V-002,S-001/V-003,S-002/V-001,S-002/V-002,S-003/V-001" {
		t.Fatalf("scoped ids %v", ids)
	}
	if v := doc.Verification("S-002/V-002"); v == nil || v.RepoID != "" || v.Method != "command" {
		t.Fatalf("unscoped verification %+v", v)
	}
	if v := doc.Verification("S-001/V-002"); v == nil || v.RepoID != "in-1" {
		t.Fatalf("input verification %+v", v)
	}
	if doc.Steps[0].Title != "Edit | pipes" || len(doc.FinalCriteria) != 2 || doc.FinalCriteria[0].ID != "R-001.C1" {
		t.Fatalf("title %q, final %+v", doc.Steps[0].Title, doc.FinalCriteria)
	}
	row := doc.Traceability[2]
	if row.CriterionID != "R-002.C1" || strings.Join(row.Steps, ",") != "S-002,S-003" || len(row.Verifications) != 3 {
		t.Fatalf("traceability %+v", row)
	}
	if doc.Traceability[4].CriterionID != "R-004.C1" || len(doc.Traceability[4].Steps) != 0 {
		t.Fatalf("unassigned non-goal row %+v", doc.Traceability[4])
	}
	if got := Order(doc); strings.Join(got, ",") != "S-001,S-002,S-003" {
		t.Fatalf("order %v", got)
	}
}

// mutate applies one textual change to the fast-min body and returns the parse error.
func mutate(t *testing.T, name, old, new string) error {
	t.Helper()
	b := string(body(t, name))
	if !strings.Contains(b, old) {
		t.Fatalf("%s body does not contain %q", name, old)
	}
	doc, err := Parse([]byte(strings.Replace(b, old, new, 1)))
	if err == nil {
		err = Validate(doc)
	}
	return err
}

func TestGrammarIsNotGuessed(t *testing.T) {
	for _, tc := range []struct {
		name, old, new, want string
	}{
		{"unknown section", "## Approach", "## Extra\n\ntext\n\n## Approach", `unexpected heading "## Extra"`},
		{"missing section", "## Decisions and assumptions\n\nNone.\n\n", "", `expected "## Decisions and assumptions"`},
		{"level-3 heading in context", "## Approach", "### Note\n\n## Approach", `heading "### Note" is not part of the renderer grammar`},
		{"indented heading in context", "## Approach", "   ## Steps\n\n## Approach", `heading "   ## Steps" is not part of the renderer grammar`},
		{"traceability row edited", "| R-001 | R-001.C1 | S-001 | S-001/V-001 |", "| R-001 | R-001.C1 | S-001 | S-001/V-002 |", "the table lists verifications"},
		{"traceability row missing", "| R-001 | R-001.C1 | S-001 | S-001/V-001 |\n", "", "has no traceability row"},
		{"criteria cell differs", "| s | R-001.C1: c | task |", "| s | R-001.C1: d | task |", "criteria differ between the list and the table"},
		{"statement differs", "| R-001 | functional | true | s |", "| R-001 | functional | true | t |", "statement differs"},
		{"type differs", "| R-001 | functional |", "| R-001 | constraint |", "is functional in the list but constraint in the table"},
		{"action numbering", "1. edit a.go", "2. edit a.go", "action numbered 2 where 1 was expected"},
		{"unescaped pipe", "| s | R-001.C1: c | task |", "| s|x | R-001.C1: c | task |", "unrecognized table row"},
		{"criterion under another requirement", "  - R-001.C1: c", "  - R-002.C1: c", "is listed under requirement R-001"},
		{"verification repo with a comma", "(command, repo-1)", "(command, repo-1, x)", `step S-001: expected a verification line, found "- V-001 (command, repo-1, x): ok"`},
		{"unknown dependency", "- Depends on: none", "- Depends on: S-009", "depends on unknown step S-009"},
		{"no quote", "> Add a --version", "Add a --version", "the verbatim task quote is missing"},
		{"title differs from nothing", "# Add a --version", "Add a --version", "does not start with a level-1 title"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := mutate(t, "fast-min", tc.old, tc.new); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	// A hash sign that is not a heading is ordinary context text.
	if err := mutate(t, "fast-min", "## Approach", "#hashtag, not a heading\n\n## Approach"); err != nil {
		t.Fatalf("a non-heading # line was rejected: %v", err)
	}
	var fe *FormatError
	if err := mutate(t, "fast-min", "1. edit a.go", "2. edit a.go"); !errors.As(err, &fe) || fe.Line == 0 || !errors.Is(err, ErrFormat) {
		t.Fatalf("format errors carry a line and ErrFormat: %v", err)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	crit := func(id string) Criterion { return Criterion{ID: id, Text: "c"} }
	doc := &Document{
		Requirements: []Requirement{
			{ID: "R-001", Mandatory: true, Criteria: []Criterion{crit("R-001.C1"), crit("R-001.C2")}},
			{ID: "R-002", Mandatory: true, Criteria: []Criterion{crit("R-001.C3")}},
		},
		Steps: []Step{
			{ID: "S-001", DependsOn: []string{"S-002"}, CriterionIDs: []string{"R-001.C1"}, RequirementIDs: []string{"R-009"},
				Verifications: []Verification{{ID: "V-001"}, {ID: "V-001"}}, Targets: []Target{{Path: "/etc/passwd"}, {Path: "a/./b"}}},
			{ID: "S-002", DependsOn: []string{"S-001", "S-001", "S-002", "S-404"}, CriterionIDs: []string{"R-001.C9"}},
			{ID: "S-003"},
		},
		FinalCriteria: []FinalCriterion{{ID: "R-001.C1"}, {ID: "R-001.C1"}},
	}
	err := Validate(doc)
	var pe *ProblemsError
	if !errors.As(err, &pe) || !errors.Is(err, ErrSemantic) {
		t.Fatalf("got %v", err)
	}
	joined := strings.Join(pe.Problems, "\n")
	for _, want := range []string{
		"criterion R-001.C3 does not belong to R-002", "S-001 references unknown requirement R-009",
		"S-001: duplicate verification id V-001", `target "/etc/passwd": absolute path`, `target "a/./b": path is not in clean form`,
		"S-002 lists dependency S-001 twice", "S-002 depends on itself", "S-002 depends on unknown step S-404",
		"S-002 references unknown criterion R-001.C9", "S-003 references no criterion",
		"mandatory criterion R-001.C2 is not assigned", "end-to-end criterion R-001.C1 is listed twice", "dependency cycle: S-001 -> S-002 -> S-001",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing problem %q in:\n%s", want, joined)
		}
	}
}

func TestBadPath(t *testing.T) {
	for p, bad := range map[string]bool{
		"a.go": false, "cmd/niten/main.go": false, "docs/**/*.md": false, "dir/": false, ".github/workflows/ci.yml": false,
		"": true, "/abs": true, "../x": true, "a/../../x": true, "a\\b": true, "a`b": true, "a//b": true, "./a": true, "a\x00b": true,
	} {
		if got := badPath(p) != ""; got != bad {
			t.Errorf("badPath(%q) = %q", p, badPath(p))
		}
	}
}

func TestOrderRepairsRenderedOrder(t *testing.T) {
	doc := &Document{Steps: []Step{{ID: "S-002", DependsOn: []string{"S-001"}}, {ID: "S-001"}, {ID: "S-003", DependsOn: []string{"S-002"}}}}
	if got := strings.Join(Order(doc), ","); got != "S-001,S-002,S-003" {
		t.Fatalf("order %s", got)
	}
}

func TestTableRow(t *testing.T) {
	for _, tc := range []struct {
		row  string
		want []string
	}{
		{`| a | b |`, []string{"a", "b"}},
		{`| a\|b | c |`, []string{"a|b", "c"}},
		{`| c\\|d | e |`, []string{`c\|d`, "e"}},
		{`| x \ y | z |`, []string{`x \ y`, "z"}},
		{`|  | z |`, []string{"", "z"}},
	} {
		got, ok := tableRow(tc.row, 2)
		if !ok || strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("tableRow(%q) = %q %v, want %q", tc.row, got, ok, tc.want)
		}
	}
	for _, bad := range []string{`| a | b`, `| a|b | c |`, `| a | b | c |`, `a | b`} {
		if _, ok := tableRow(bad, 2); ok {
			t.Errorf("tableRow(%q) accepted", bad)
		}
	}
}

func TestSplitBodyRules(t *testing.T) {
	good := "---\ntitle: x\n---\n\n" + MarkerBegin + "\nbody\n" + MarkerEnd + "\ntrailer\n"
	sp, err := SplitBody([]byte(good))
	if err != nil || string(sp.Body) != "body\n" || sp.BodyLine != 6 {
		t.Fatalf("split: %v %q line %d", err, sp.Body, sp.BodyLine)
	}
	for name, doc := range map[string]string{
		"no frontmatter":   MarkerBegin + "\nbody\n" + MarkerEnd + "\n",
		"unterminated":     "---\ntitle: x\n" + MarkerBegin + "\n",
		"two begins":       "---\nt: x\n---\n" + MarkerBegin + "\n" + MarkerBegin + "\n" + MarkerEnd + "\n",
		"end before begin": "---\nt: x\n---\n" + MarkerEnd + "\n" + MarkerBegin + "\n",
		"text before":      "---\nt: x\n---\nhello\n" + MarkerBegin + "\nb\n" + MarkerEnd + "\n",
	} {
		if _, err := SplitBody([]byte(doc)); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestReceiptAndManifestAreStrict(t *testing.T) {
	tr := load(t, "fast-min")
	var rec map[string]any
	json.Unmarshal(tr.receipt, &rec)
	for name, edit := range map[string]func(map[string]any){
		"unknown field":   func(m map[string]any) { m["signature"] = "x" },
		"schema version":  func(m map[string]any) { m["schema_version"] = 2 },
		"short digest":    func(m map[string]any) { m["body_sha256"] = "abc" },
		"no metadata":     func(m map[string]any) { delete(m, "immutable_metadata") },
		"plan id differs": func(m map[string]any) { m["plan_id"] = "other" },
	} {
		cp := map[string]any{}
		b, _ := json.Marshal(rec)
		json.Unmarshal(b, &cp)
		edit(cp)
		b, _ = json.Marshal(cp)
		if _, _, err := DecodeReceipt(b); err == nil {
			t.Errorf("receipt %s accepted", name)
		}
	}
	if _, _, err := DecodeReceipt(append(append([]byte{}, tr.receipt...), []byte(" {}")...)); err == nil {
		t.Error("receipt with trailing data accepted")
	}
	for name, doc := range map[string]string{
		"unknown field": strings.Replace(string(tr.manifest), `"version": 1,`, `"version": 1, "extra": true,`, 1),
		"version":       strings.Replace(string(tr.manifest), `"version": 1,`, `"version": 2,`, 1),
		"truncated":     string(tr.manifest[:len(tr.manifest)/2]),
	} {
		if _, err := DecodeManifest([]byte(doc)); !errors.Is(err, ErrFormat) {
			t.Errorf("manifest %s: %v", name, err)
		}
	}
}

// Only the digest-bound fields decide the manifest match; the root is a locator.
func TestManifestBinding(t *testing.T) {
	tr := load(t, "fast-min")
	r, _, _ := DecodeReceipt(tr.receipt)
	fresh := func() *Manifest {
		m, err := DecodeManifest(tr.manifest)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := fresh()
	m.Repos[0].Root, m.Workspace, m.Exclude = "/elsewhere/demo", "/elsewhere", nil
	if err := CheckManifest(m, r); err != nil {
		t.Fatalf("unbound locator change rejected: %v", err)
	}
	m = fresh()
	m.Repos[0].Head = strings.Repeat("a", 40)
	if err := CheckManifest(m, r); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "inconsistent with its recorded digests") {
		t.Fatalf("head change: %v", err)
	}
	m.Repos[0].Fingerprint = m.Repos[0].ComputeFingerprint()
	m.Fingerprint = m.ComputeFingerprint()
	if err := CheckManifest(m, r); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "does not match the receipt") {
		t.Fatalf("consistent but different base: %v", err)
	}
	m = fresh()
	m.Fingerprint = strings.Repeat("0", 64)
	if err := CheckManifest(m, r); err == nil {
		t.Fatal("stored fingerprint field mismatch accepted")
	}
	noDigest := *r
	noDigest.ManifestDigest = ""
	if err := CheckManifest(fresh(), &noDigest); err == nil {
		t.Fatal("a receipt without manifest digest pinned a manifest")
	}
}

func TestCheckInputsAgainstManifest(t *testing.T) {
	tr := load(t, "fast-tricky")
	doc := parse(t, "fast-tricky")
	m, _ := DecodeManifest(tr.manifest)
	if notes, err := CheckInputs(doc, m); err != nil || len(notes) != 0 {
		t.Fatalf("tricky inputs: %v %v", notes, err)
	}
	m.Repos[0].Root = "/x/renamed"
	if notes, err := CheckInputs(doc, m); err != nil || len(notes) != 1 {
		t.Fatalf("renamed locator should only note: %v %v", notes, err)
	}
	m, _ = DecodeManifest(tr.manifest)
	m.Inputs[0].SHA256 = strings.Repeat("f", 64)
	if _, err := CheckInputs(doc, m); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("input digest mismatch: %v", err)
	}
	m, _ = DecodeManifest(tr.manifest)
	m.Repos[0].Head = strings.Repeat("b", 40)
	if _, err := CheckInputs(doc, m); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("head mismatch: %v", err)
	}
	m, _ = DecodeManifest(tr.manifest)
	m.Inputs = nil
	if _, err := CheckInputs(doc, m); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("an input line the manifest does not know: %v", err)
	}
}

// NITEN_PLAN_ARCHIVE points at a Shogun plan library (e.g. ~/workspace/shogun-plans). Every
// published plan in it must parse and validate. The plans stay outside this repository.
func TestArchivePlansParse(t *testing.T) {
	dir := os.Getenv("NITEN_PLAN_ARCHIVE")
	if dir == "" {
		t.Skip("NITEN_PLAN_ARCHIVE unset")
	}
	plans, _ := filepath.Glob(filepath.Join(dir, "*", "*.md"))
	if len(plans) == 0 {
		t.Fatalf("no plans under %s", dir)
	}
	for _, p := range plans {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		sp, err := SplitBody(data)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		doc, err := Parse(sp.Body)
		if err == nil {
			err = Validate(doc)
		}
		if err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}
