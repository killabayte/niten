package plan

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// GrammarVersion names the renderer grammar this importer recognizes: Shogun's
// internal/pipeline/render.go as of the S0 manifest sidecar (commit bb3f7e6, branch
// s0-manifest-sidecar). A renderer change that alters the grammar must change this value.
const GrammarVersion = "shogun-render-s0"

// Section names in rendering order. "Review notes" is emitted only when minor findings stayed open.
const (
	SecGoal         = "Goal and success criteria"
	SecScope        = "Scope and non-goals"
	SecInputs       = "Inputs and versions"
	SecRequirements = "Requirements"
	SecDecisions    = "Decisions and assumptions"
	SecContext      = "Context"
	SecApproach     = "Approach"
	SecSteps        = "Steps"
	SecEndToEnd     = "End-to-end verification"
	SecTrace        = "Traceability"
	SecReviewNotes  = "Review notes"
	SecReviewHist   = "Review history"
)

var sectionOrder = []struct {
	name     string
	optional bool
}{
	{SecGoal, false}, {SecScope, false}, {SecInputs, false}, {SecRequirements, false}, {SecDecisions, false},
	{SecContext, false}, {SecApproach, false}, {SecSteps, false}, {SecEndToEnd, false}, {SecTrace, false},
	{SecReviewNotes, true}, {SecReviewHist, false},
}

// FormatError reports input that does not follow the supported grammar. Line is 1-based and
// relative to the approved body; 0 means the whole body.
type FormatError struct {
	Line    int
	Section string
	Msg     string
}

func (e *FormatError) Error() string {
	loc := "approved body"
	if e.Section != "" {
		loc = "section " + strconv.Quote(e.Section)
	}
	if e.Line > 0 {
		return fmt.Sprintf("%v: %s, body line %d: %s", ErrFormat, loc, e.Line, e.Msg)
	}
	return fmt.Sprintf("%v: %s: %s", ErrFormat, loc, e.Msg)
}

func (e *FormatError) Unwrap() error { return ErrFormat }

type line struct {
	text       string
	start, end int // byte offsets in the body; end excludes the newline
	no         int // 1-based body line number
}

func splitLines(body []byte) []line {
	var out []line
	for off, no := 0, 1; off < len(body); no++ {
		end, next := len(body), len(body)
		if nl := bytes.IndexByte(body[off:], '\n'); nl >= 0 {
			end, next = off+nl, off+nl+1
		}
		out = append(out, line{text: string(body[off:end]), start: off, end: end, no: no})
		off = next
	}
	return out
}

// atxHeading matches a CommonMark ATX heading line: up to three spaces of indentation, one
// to six '#', then a space or the end. Only exact unindented "## <name>" lines are section
// boundaries; any other heading-shaped line is rejected as ambiguous.
var atxHeading = regexp.MustCompile(`^ {0,3}#{1,6}(\s|$)`)

var (
	reStepHeader  = regexp.MustCompile(`^### (S-[0-9]{3,}) — (.+)$`)
	reReqItem     = regexp.MustCompile(`^- \*\*(R-[0-9]{3,})\*\* (.+)$`)
	reScopeItem   = regexp.MustCompile(`^- \*\*(R-[0-9]{3,})\*\* \((constraint|non-goal)\) (.+)$`)
	reCritItem    = regexp.MustCompile(`^  - (R-[0-9]{3,}\.C[0-9]+): (.*)$`)
	reInputLine   = regexp.MustCompile(`^- ([A-Za-z0-9._-]+): (.+)$`)
	reDecision    = regexp.MustCompile(`^- ([A-Za-z0-9._-]+) \(([a-z-]+)\): (.+)$`)
	reObjective   = regexp.MustCompile(`^- Objective: (.*)$`)
	reDependsOn   = regexp.MustCompile(`^- Depends on: (none|S-[0-9]{3,}(?:, S-[0-9]{3,})*)$`)
	reStepReqs    = regexp.MustCompile(`^- Requirements: (none|R-[0-9]{3,}(?:, R-[0-9]{3,})*); criteria: (none|R-[0-9]{3,}\.C[0-9]+(?:, R-[0-9]{3,}\.C[0-9]+)*)$`)
	reTarget      = regexp.MustCompile("^- ([A-Za-z0-9._-]+) `(.+)` \\((inspect|create|modify|delete)\\)$")
	reAction      = regexp.MustCompile(`^([0-9]+)\. (.*)$`)
	reVerif       = regexp.MustCompile(`^- (V-[0-9]{3,}) \((test|command|inspect|measure), ([A-Za-z0-9._-]*)\): (.*)$`)
	reRisk        = regexp.MustCompile(`^- (.+) — mitigation: (.+)$`)
	reRollback    = regexp.MustCompile(`^Rollback: (.*)$`)
	reFinalItem   = regexp.MustCompile(`^- (R-[0-9]{3,}\.C[0-9]+): (.*)$`)
	reStepID      = regexp.MustCompile(`^S-[0-9]{3,}$`)
	reScopedVerif = regexp.MustCompile(`^S-[0-9]{3,}/V-[0-9]{3,}$`)
	reReqID       = regexp.MustCompile(`^R-[0-9]{3,}$`)
	reCritID      = regexp.MustCompile(`^R-[0-9]{3,}\.C[0-9]+$`)
)

const (
	noScope    = "No constraints or non-goals beyond the requirements."
	noDecision = "None."
	noFinal    = "Every criterion is verified inside its step (see the traceability table)."
	reqHeader  = "| ID | Type | Mandatory | Statement | Criteria | Sources |"
	reqSep     = "|---|---|---|---|---|---|"
	trHeader   = "| Requirement | Criterion | Steps | Verification |"
	trSep      = "|---|---|---|---|"
)

type parser struct {
	body  []byte
	lines []line
}

func (p *parser) span(from, to line) Span {
	return Span{Start: from.start, End: to.end, LineStart: from.no, LineEnd: to.no, SHA256: Digest(p.body[from.start:to.end])}
}

func errAt(sec string, l line, format string, a ...any) error {
	return &FormatError{Line: l.no, Section: sec, Msg: fmt.Sprintf(format, a...)}
}

// section is the lines of one top-level section, heading excluded.
type section struct {
	name    string
	heading line
	lines   []line
}

// Parse recognizes an approved body rendered by Shogun and returns it in normalized form.
// It does not guess: a line that does not match the grammar of its section, a heading
// that is not one of the known sections in their order, a repeated section or an
// inconsistency between the lists and the tables that render the same data is a
// FormatError. Free-text sections (Context, Approach, Review notes, Review history) are
// kept by span without interpretation, but may not contain heading lines, so quoted text
// cannot fake structure.
func Parse(body []byte) (*Document, error) {
	p := &parser{body: body, lines: splitLines(body)}
	doc := &Document{BodySHA256: Digest(body), GrammarVersion: GrammarVersion}

	i := 0
	for i < len(p.lines) && strings.TrimSpace(p.lines[i].text) == "" {
		i++
	}
	if i == len(p.lines) || !strings.HasPrefix(p.lines[i].text, "# ") || strings.TrimSpace(p.lines[i].text[2:]) == "" {
		return nil, &FormatError{Msg: "the body does not start with a level-1 title heading"}
	}
	doc.Title = p.lines[i].text[2:]
	i++

	var sections []section
	next := 0 // index into sectionOrder of the next expected section
	for ; i < len(p.lines); i++ {
		l := p.lines[i]
		if !atxHeading.MatchString(l.text) {
			if len(sections) == 0 {
				if strings.TrimSpace(l.text) != "" {
					return nil, errAt("", l, "text between the title and the first section: %q", l.text)
				}
				continue
			}
			sections[len(sections)-1].lines = append(sections[len(sections)-1].lines, l)
			continue
		}
		if len(sections) > 0 && sections[len(sections)-1].name == SecSteps && strings.HasPrefix(l.text, "### ") {
			sections[len(sections)-1].lines = append(sections[len(sections)-1].lines, l)
			continue
		}
		name, ok := strings.CutPrefix(l.text, "## ")
		if !ok {
			return nil, errAt(currentName(sections), l, "heading %q is not part of the renderer grammar; quoted or multi-line text that forms a heading is ambiguous", l.text)
		}
		found := -1
		for j := next; j < len(sectionOrder); j++ {
			if sectionOrder[j].name == name {
				found = j
				break
			}
			if !sectionOrder[j].optional {
				break
			}
		}
		if found < 0 {
			for _, s := range sections {
				if s.name == name {
					return nil, errAt(currentName(sections), l, "section %q appears a second time; quoted or multi-line text that forms a heading is ambiguous", name)
				}
			}
			return nil, errAt(currentName(sections), l, "unexpected heading %q; expected %q", l.text, "## "+sectionOrder[min(next, len(sectionOrder)-1)].name)
		}
		sections = append(sections, section{name: name, heading: l})
		next = found + 1
	}
	for j := next; j < len(sectionOrder); j++ {
		if !sectionOrder[j].optional {
			return nil, &FormatError{Msg: fmt.Sprintf("section %q is missing", sectionOrder[j].name)}
		}
	}

	byName := map[string]section{}
	for _, s := range sections {
		byName[s.name] = s
		last := s.heading
		for k := len(s.lines) - 1; k >= 0; k-- {
			if strings.TrimSpace(s.lines[k].text) != "" {
				last = s.lines[k]
				break
			}
		}
		doc.Sections = append(doc.Sections, Section{Name: s.name, Source: p.span(s.heading, last)})
	}

	listReqs, err := p.parseGoal(byName[SecGoal], doc)
	if err != nil {
		return nil, err
	}
	scopeReqs, err := p.parseScope(byName[SecScope])
	if err != nil {
		return nil, err
	}
	listReqs = append(listReqs, scopeReqs...)
	if doc.Inputs, err = p.parseInputs(byName[SecInputs]); err != nil {
		return nil, err
	}
	tableReqs, err := p.parseRequirementsTable(byName[SecRequirements])
	if err != nil {
		return nil, err
	}
	if doc.Requirements, err = p.mergeRequirements(listReqs, tableReqs, byName[SecRequirements]); err != nil {
		return nil, err
	}
	if doc.Decisions, err = p.parseDecisions(byName[SecDecisions]); err != nil {
		return nil, err
	}
	if doc.Steps, err = p.parseSteps(byName[SecSteps]); err != nil {
		return nil, err
	}
	if doc.FinalCriteria, err = p.parseFinal(byName[SecEndToEnd], doc); err != nil {
		return nil, err
	}
	if doc.Traceability, err = p.parseTraceability(byName[SecTrace]); err != nil {
		return nil, err
	}
	if err := checkTraceability(doc, byName[SecTrace]); err != nil {
		return nil, err
	}
	return doc, nil
}

func currentName(sections []section) string {
	if len(sections) == 0 {
		return ""
	}
	return sections[len(sections)-1].name
}

// nonBlank returns the section lines that carry text.
func nonBlank(s section) []line {
	var out []line
	for _, l := range s.lines {
		if strings.TrimSpace(l.text) != "" {
			out = append(out, l)
		}
	}
	return out
}

// reqList is a requirement as rendered in the Goal and Scope lists.
type reqList struct {
	id, typ, statement string
	criteria           []Criterion
	first, last        line
}

func (p *parser) parseGoal(s section, doc *Document) ([]reqList, error) {
	ls := s.lines
	k := 0
	skip := func() {
		for k < len(ls) && strings.TrimSpace(ls[k].text) == "" {
			k++
		}
	}
	skip()
	if k == len(ls) || ls[k].text != "Task, verbatim:" {
		return nil, &FormatError{Line: s.heading.no, Section: s.name, Msg: `expected the line "Task, verbatim:"`}
	}
	k++
	skip()
	var task []string
	first := k
	for k < len(ls) && strings.HasPrefix(ls[k].text, "> ") {
		task = append(task, ls[k].text[2:])
		k++
	}
	if len(task) == 0 {
		return nil, &FormatError{Line: s.heading.no, Section: s.name, Msg: "the verbatim task quote is missing"}
	}
	doc.Task = strings.Join(task, "\n")
	doc.TaskSource = p.span(ls[first], ls[k-1])
	return p.parseReqItems(s, ls[k:], false)
}

func (p *parser) parseScope(s section) ([]reqList, error) {
	nb := nonBlank(s)
	if len(nb) == 1 && nb[0].text == noScope {
		return nil, nil
	}
	return p.parseReqItems(s, s.lines, true)
}

func (p *parser) parseReqItems(s section, ls []line, scope bool) ([]reqList, error) {
	var out []reqList
	for _, l := range ls {
		if strings.TrimSpace(l.text) == "" {
			continue
		}
		if scope {
			if m := reScopeItem.FindStringSubmatch(l.text); m != nil {
				out = append(out, reqList{id: m[1], typ: strings.ReplaceAll(m[2], "-", "_"), statement: m[3], first: l, last: l})
				continue
			}
		} else if m := reReqItem.FindStringSubmatch(l.text); m != nil {
			out = append(out, reqList{id: m[1], typ: "functional", statement: m[2], first: l, last: l})
			continue
		}
		if m := reCritItem.FindStringSubmatch(l.text); m != nil {
			if len(out) == 0 {
				return nil, errAt(s.name, l, "criterion %s appears before any requirement", m[1])
			}
			r := &out[len(out)-1]
			if !strings.HasPrefix(m[1], r.id+".C") {
				return nil, errAt(s.name, l, "criterion %s is listed under requirement %s", m[1], r.id)
			}
			r.criteria = append(r.criteria, Criterion{ID: m[1], Text: m[2], Source: p.span(l, l)})
			r.last = l
			continue
		}
		return nil, errAt(s.name, l, "unrecognized line %q; multi-line requirement or criterion text is ambiguous", l.text)
	}
	return out, nil
}

func (p *parser) parseInputs(s section) ([]InputLine, error) {
	var out []InputLine
	for _, l := range nonBlank(s) {
		m := reInputLine.FindStringSubmatch(l.text)
		if m == nil {
			return nil, errAt(s.name, l, "unrecognized line %q", l.text)
		}
		out = append(out, InputLine{ID: m[1], Text: m[2], Source: p.span(l, l)})
	}
	if len(out) == 0 {
		return nil, &FormatError{Line: s.heading.no, Section: s.name, Msg: "no repository is listed"}
	}
	return out, nil
}

// tableRow splits a rendered table row. Shogun's cell() escapes every '|' in content as
// `\|` and replaces newlines with spaces, and the delimiters are " | " with a space before
// the pipe, so a pipe preceded by a backslash is always content.
func tableRow(text string, want int) ([]string, bool) {
	if !strings.HasPrefix(text, "| ") || !strings.HasSuffix(text, " |") || len(text) < 4 {
		return nil, false
	}
	inner := text[2 : len(text)-2]
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(inner); i++ {
		switch {
		case inner[i] == '\\' && i+1 < len(inner) && inner[i+1] == '|':
			cur.WriteByte('|')
			i++
		case inner[i] == '|':
			c := cur.String()
			if !strings.HasSuffix(c, " ") || i+1 >= len(inner) || inner[i+1] != ' ' {
				return nil, false
			}
			cells = append(cells, c[:len(c)-1])
			cur.Reset()
			i++ // the space after the delimiter
		default:
			cur.WriteByte(inner[i])
		}
	}
	cells = append(cells, cur.String())
	if len(cells) != want {
		return nil, false
	}
	return cells, true
}

type reqRow struct {
	id, typ, statement, criteria string
	mandatory                    bool
	sources                      []string
	l                            line
}

func (p *parser) parseRequirementsTable(s section) ([]reqRow, error) {
	nb := nonBlank(s)
	if len(nb) < 2 || nb[0].text != reqHeader || nb[1].text != reqSep {
		return nil, &FormatError{Line: s.heading.no, Section: s.name, Msg: "the requirements table header is not the renderer's"}
	}
	var out []reqRow
	for _, l := range nb[2:] {
		c, ok := tableRow(l.text, 6)
		if !ok {
			return nil, errAt(s.name, l, "unrecognized table row %q", l.text)
		}
		if !reReqID.MatchString(c[0]) {
			return nil, errAt(s.name, l, "requirement id %q is not an R-NNN id", c[0])
		}
		switch c[1] {
		case "functional", "constraint", "non_goal":
		default:
			return nil, errAt(s.name, l, "requirement %s has unknown type %q", c[0], c[1])
		}
		var mand bool
		switch c[2] {
		case "true":
			mand = true
		case "false":
		default:
			return nil, errAt(s.name, l, "requirement %s mandatory value %q is not true or false", c[0], c[2])
		}
		var srcs []string
		if c[5] != "" {
			srcs = strings.Split(c[5], ", ")
		}
		out = append(out, reqRow{id: c[0], typ: c[1], mandatory: mand, statement: c[3], criteria: c[4], sources: srcs, l: l})
	}
	if len(out) == 0 {
		return nil, &FormatError{Line: s.heading.no, Section: s.name, Msg: "the requirements table is empty"}
	}
	return out, nil
}

// norm compares list text with table text, which differ only in whitespace: table cells
// replace newlines with spaces and trim the joined criteria once, lists trim each item.
func norm(s string) string { return strings.Join(strings.Fields(s), " ") }

// mergeRequirements checks that the Goal/Scope lists and the Requirements table render the
// same registry and returns it in table order.
func (p *parser) mergeRequirements(list []reqList, table []reqRow, s section) ([]Requirement, error) {
	byID := map[string]reqList{}
	for _, r := range list {
		if _, dup := byID[r.id]; dup {
			return nil, errAt(s.name, r.first, "requirement %s is listed twice", r.id)
		}
		byID[r.id] = r
	}
	seen := map[string]bool{}
	var out []Requirement
	for _, row := range table {
		if seen[row.id] {
			return nil, errAt(s.name, row.l, "requirement %s appears twice in the table", row.id)
		}
		seen[row.id] = true
		lr, ok := byID[row.id]
		if !ok {
			return nil, errAt(s.name, row.l, "requirement %s is in the table but not in the Goal or Scope list", row.id)
		}
		if lr.typ != row.typ {
			return nil, errAt(s.name, row.l, "requirement %s is %s in the list but %s in the table", row.id, lr.typ, row.typ)
		}
		if norm(lr.statement) != norm(row.statement) {
			return nil, errAt(s.name, row.l, "requirement %s statement differs between the list and the table", row.id)
		}
		var cs []string
		for _, c := range lr.criteria {
			cs = append(cs, c.ID+": "+c.Text)
		}
		if norm(strings.Join(cs, "; ")) != norm(row.criteria) {
			return nil, errAt(s.name, row.l, "requirement %s criteria differ between the list and the table", row.id)
		}
		out = append(out, Requirement{ID: row.id, Type: row.typ, Mandatory: row.mandatory, Statement: lr.statement,
			Criteria: lr.criteria, SourceIDs: row.sources, Source: p.span(lr.first, lr.last), TableSource: p.span(row.l, row.l)})
	}
	for _, r := range list {
		if !seen[r.id] {
			return nil, errAt(s.name, r.first, "requirement %s is listed but missing from the table", r.id)
		}
	}
	return out, nil
}

func (p *parser) parseDecisions(s section) ([]Decision, error) {
	nb := nonBlank(s)
	if len(nb) == 1 && nb[0].text == noDecision {
		return nil, nil
	}
	var out []Decision
	for _, l := range nb {
		m := reDecision.FindStringSubmatch(l.text)
		if m == nil {
			return nil, errAt(s.name, l, "unrecognized line %q; multi-line decision text is ambiguous", l.text)
		}
		out = append(out, Decision{ID: m[1], Kind: m[2], Text: m[3], Source: p.span(l, l)})
	}
	return out, nil
}

func splitIDs(s string) []string {
	if s == "none" {
		return []string{}
	}
	return strings.Split(s, ", ")
}

func (p *parser) parseSteps(s section) ([]Step, error) {
	nb := nonBlank(s)
	k := 0
	var steps []Step
	peek := func() (line, bool) {
		if k < len(nb) {
			return nb[k], true
		}
		return line{}, false
	}
	expect := func(step, what string, re *regexp.Regexp) (line, []string, error) {
		l, ok := peek()
		if !ok {
			return line{}, nil, &FormatError{Section: s.name, Msg: fmt.Sprintf("step %s ends before its %s", step, what)}
		}
		m := re.FindStringSubmatch(l.text)
		if m == nil {
			return line{}, nil, errAt(s.name, l, "step %s: expected %s, found %q", step, what, l.text)
		}
		k++
		return l, m, nil
	}
	expectLabel := func(step, label string) error {
		l, ok := peek()
		if !ok || l.text != label {
			if !ok {
				return &FormatError{Section: s.name, Msg: fmt.Sprintf("step %s ends before %q", step, label)}
			}
			return errAt(s.name, l, "step %s: expected %q, found %q", step, label, l.text)
		}
		k++
		return nil
	}
	for k < len(nb) {
		hl, m, err := expect("", "a step heading", reStepHeader)
		if err != nil {
			return nil, err
		}
		st := Step{ID: m[1], Title: m[2]}
		_, m, err = expect(st.ID, "the objective", reObjective)
		if err != nil {
			return nil, err
		}
		st.Objective = m[1]
		if _, m, err = expect(st.ID, "the dependencies", reDependsOn); err != nil {
			return nil, err
		}
		st.DependsOn = splitIDs(m[1])
		if _, m, err = expect(st.ID, "the requirements and criteria", reStepReqs); err != nil {
			return nil, err
		}
		st.RequirementIDs, st.CriterionIDs = splitIDs(m[1]), splitIDs(m[2])
		if err := expectLabel(st.ID, "Targets:"); err != nil {
			return nil, err
		}
		st.Targets = []Target{}
		for l, ok := peek(); ok; l, ok = peek() {
			m := reTarget.FindStringSubmatch(l.text)
			if m == nil {
				break
			}
			st.Targets = append(st.Targets, Target{RepoID: m[1], Path: m[2], Operation: m[3], Source: p.span(l, l)})
			k++
		}
		if err := expectLabel(st.ID, "Actions:"); err != nil {
			return nil, err
		}
		for l, ok := peek(); ok; l, ok = peek() {
			m := reAction.FindStringSubmatch(l.text)
			if m == nil {
				break
			}
			if m[1] != strconv.Itoa(len(st.Actions)+1) {
				return nil, errAt(s.name, l, "step %s: action numbered %s where %d was expected", st.ID, m[1], len(st.Actions)+1)
			}
			st.Actions = append(st.Actions, Action{Text: m[2], Source: p.span(l, l)})
			k++
		}
		if len(st.Actions) == 0 {
			return nil, unexpected(s, hl, peek, st.ID, "a numbered action")
		}
		if err := expectLabel(st.ID, "Verification:"); err != nil {
			return nil, err
		}
		for l, ok := peek(); ok; l, ok = peek() {
			m := reVerif.FindStringSubmatch(l.text)
			if m == nil {
				break
			}
			st.Verifications = append(st.Verifications, Verification{ID: m[1], ScopedID: st.ID + "/" + m[1], Method: m[2], RepoID: m[3], Expected: m[4], Source: p.span(l, l)})
			k++
		}
		if len(st.Verifications) == 0 {
			return nil, unexpected(s, hl, peek, st.ID, "a verification line")
		}
		st.Risks = []Risk{}
		if l, ok := peek(); ok && l.text == "Risks:" {
			k++
			for l, ok := peek(); ok; l, ok = peek() {
				m := reRisk.FindStringSubmatch(l.text)
				if m == nil {
					break
				}
				if strings.Count(l.text, " — mitigation: ") != 1 {
					return nil, errAt(s.name, l, "step %s: risk line has more than one mitigation separator", st.ID)
				}
				st.Risks = append(st.Risks, Risk{Risk: m[1], Mitigation: m[2], Source: p.span(l, l)})
				k++
			}
			if len(st.Risks) == 0 {
				return nil, errAt(s.name, l, "step %s: empty risk list", st.ID)
			}
		}
		rl, m, err := expect(st.ID, "the rollback line", reRollback)
		if err != nil {
			return nil, err
		}
		st.Rollback = m[1]
		st.Source = p.span(hl, rl)
		steps = append(steps, st)
	}
	if len(steps) == 0 {
		return nil, &FormatError{Line: s.heading.no, Section: s.name, Msg: "the plan has no steps"}
	}
	return steps, nil
}

// unexpected reports the line where a required list item was expected.
func unexpected(s section, heading line, peek func() (line, bool), step, what string) error {
	if l, ok := peek(); ok {
		return errAt(s.name, l, "step %s: expected %s, found %q", step, what, l.text)
	}
	return errAt(s.name, heading, "step %s: expected %s, found the end of the section", step, what)
}

func (p *parser) parseFinal(s section, doc *Document) ([]FinalCriterion, error) {
	nb := nonBlank(s)
	if len(nb) == 1 && nb[0].text == noFinal {
		return []FinalCriterion{}, nil
	}
	out := []FinalCriterion{}
	for _, l := range nb {
		m := reFinalItem.FindStringSubmatch(l.text)
		if m == nil {
			return nil, errAt(s.name, l, "unrecognized line %q", l.text)
		}
		c, _ := doc.Criterion(m[1])
		if c == nil {
			return nil, errAt(s.name, l, "end-to-end criterion %s is not in the registry", m[1])
		}
		if norm(c.Text) != norm(m[2]) {
			return nil, errAt(s.name, l, "end-to-end criterion %s text differs from the registry", m[1])
		}
		out = append(out, FinalCriterion{ID: m[1], Text: m[2], Source: p.span(l, l)})
	}
	return out, nil
}

func (p *parser) parseTraceability(s section) ([]TraceRow, error) {
	nb := nonBlank(s)
	if len(nb) < 2 || nb[0].text != trHeader || nb[1].text != trSep {
		return nil, &FormatError{Line: s.heading.no, Section: s.name, Msg: "the traceability table header is not the renderer's"}
	}
	var out []TraceRow
	for _, l := range nb[2:] {
		c, ok := tableRow(l.text, 4)
		if !ok {
			return nil, errAt(s.name, l, "unrecognized table row %q", l.text)
		}
		if !reReqID.MatchString(c[0]) || !reCritID.MatchString(c[1]) {
			return nil, errAt(s.name, l, "row ids %q/%q are not requirement/criterion ids", c[0], c[1])
		}
		row := TraceRow{RequirementID: c[0], CriterionID: c[1], Steps: []string{}, Verifications: []string{}, Source: p.span(l, l)}
		for _, st := range splitIDs(c[2]) {
			switch {
			case st == "end-to-end":
				row.EndToEnd = true
			case reStepID.MatchString(st) && !row.EndToEnd:
				row.Steps = append(row.Steps, st)
			default:
				return nil, errAt(s.name, l, "steps cell %q is not a step list", c[2])
			}
		}
		for _, v := range splitIDs(c[3]) {
			if !reScopedVerif.MatchString(v) {
				return nil, errAt(s.name, l, "verification cell %q is not a list of scoped verification ids", c[3])
			}
			row.Verifications = append(row.Verifications, v)
		}
		out = append(out, row)
	}
	return out, nil
}

// checkTraceability requires the Traceability table to be exactly what the renderer derives
// from the parsed registry, steps and end-to-end list. A step, verification or criterion that
// text forged inside another field cannot also appear in this table, so any forgery shows up
// as a mismatch.
func checkTraceability(doc *Document, s section) error {
	final := map[string]bool{}
	for _, f := range doc.FinalCriteria {
		final[f.ID] = true
	}
	k := 0
	for _, r := range doc.Requirements {
		for _, c := range r.Criteria {
			if k >= len(doc.Traceability) {
				return &FormatError{Section: s.name, Msg: fmt.Sprintf("criterion %s has no traceability row", c.ID)}
			}
			row := doc.Traceability[k]
			k++
			at := &FormatError{Line: row.Source.LineStart, Section: s.name}
			if row.RequirementID != r.ID || row.CriterionID != c.ID {
				at.Msg = fmt.Sprintf("row %s/%s where %s/%s was expected", row.RequirementID, row.CriterionID, r.ID, c.ID)
				return at
			}
			var steps, verifs []string
			for _, st := range doc.Steps {
				if contains(st.CriterionIDs, c.ID) {
					steps = append(steps, st.ID)
					for _, v := range st.Verifications {
						verifs = append(verifs, v.ScopedID)
					}
				}
			}
			if strings.Join(steps, ",") != strings.Join(row.Steps, ",") || final[c.ID] != row.EndToEnd {
				at.Msg = fmt.Sprintf("criterion %s: the table lists steps %v (end-to-end %v), the parsed steps carry it in %v (end-to-end %v)", c.ID, row.Steps, row.EndToEnd, steps, final[c.ID])
				return at
			}
			if strings.Join(verifs, ",") != strings.Join(row.Verifications, ",") {
				at.Msg = fmt.Sprintf("criterion %s: the table lists verifications %v, the parsed steps have %v", c.ID, row.Verifications, verifs)
				return at
			}
		}
	}
	if k != len(doc.Traceability) {
		row := doc.Traceability[k]
		return &FormatError{Line: row.Source.LineStart, Section: s.name, Msg: fmt.Sprintf("row %s/%s has no criterion in the registry", row.RequirementID, row.CriterionID)}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
