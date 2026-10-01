package plan

// Span locates source text inside the approved body: byte offsets [Start, End), 1-based
// body-relative line numbers and the SHA-256 of the exact bytes. Body-relative positions
// stay stable when the mutable frontmatter or the execution log change.
type Span struct {
	Start     int    `json:"start"`
	End       int    `json:"end"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	SHA256    string `json:"sha256"`
}

// Criterion is one acceptance criterion, verbatim from the approved body.
type Criterion struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Source Span   `json:"source"`
}

// Requirement is one requirement of the approved registry. Type and Mandatory come from
// the Requirements table; statement and criteria from the Goal and Scope lists, which the
// importer cross-checks against the table.
type Requirement struct {
	ID          string      `json:"id"`
	Type        string      `json:"type"`
	Mandatory   bool        `json:"mandatory"`
	Statement   string      `json:"statement"`
	Criteria    []Criterion `json:"criteria"`
	SourceIDs   []string    `json:"source_ids"`
	Source      Span        `json:"source"`
	TableSource Span        `json:"table_source"`
}

// Target is a planned focus path of a step. It is not a permission: the hard write
// boundary is the policy, and Niten computes off-target edits itself.
type Target struct {
	RepoID    string `json:"repo_id"`
	Path      string `json:"path"`
	Operation string `json:"operation"`
	Source    Span   `json:"source"`
}

// Action is one numbered action of a step, kept as text; it is never executed.
type Action struct {
	Text   string `json:"text"`
	Source Span   `json:"source"`
}

// Verification is one planned check of a step. ScopedID ("S-001/V-001") is the key: the
// same V-001 appears in several steps. Expected is a description, not a command.
type Verification struct {
	ID       string `json:"id"`
	ScopedID string `json:"scoped_id"`
	Method   string `json:"method"`
	RepoID   string `json:"repo_id"`
	Expected string `json:"expected"`
	Source   Span   `json:"source"`
}

// Risk is one risk of a step with its mitigation.
type Risk struct {
	Risk       string `json:"risk"`
	Mitigation string `json:"mitigation"`
	Source     Span   `json:"source"`
}

// Step is one approved step. Every imported step is required: Shogun steps have no
// optional flag, and the set of steps and their dependencies cannot change without a new
// plan revision.
type Step struct {
	ID             string         `json:"id"`
	Title          string         `json:"title"`
	Objective      string         `json:"objective"`
	DependsOn      []string       `json:"depends_on"`
	RequirementIDs []string       `json:"requirement_ids"`
	CriterionIDs   []string       `json:"criterion_ids"`
	Targets        []Target       `json:"targets"`
	Actions        []Action       `json:"actions"`
	Verifications  []Verification `json:"verifications"`
	Risks          []Risk         `json:"risks"`
	Rollback       string         `json:"rollback"`
	Source         Span           `json:"source"`
}

// FinalCriterion is a criterion of the end-to-end verification.
type FinalCriterion struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Source Span   `json:"source"`
}

// TraceRow is one row of the Traceability table: the checks a step could use for a
// criterion. It shows possible evidence, not sufficient evidence.
type TraceRow struct {
	RequirementID string   `json:"requirement_id"`
	CriterionID   string   `json:"criterion_id"`
	Steps         []string `json:"steps"`
	EndToEnd      bool     `json:"end_to_end"`
	Verifications []string `json:"verifications"`
	Source        Span     `json:"source"`
}

// InputLine is one line of "Inputs and versions"; it is cross-checked against the manifest.
type InputLine struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Source Span   `json:"source"`
}

// Decision is one entry of "Decisions and assumptions".
type Decision struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Source Span   `json:"source"`
}

// Section is one top-level section of the approved body, kept in full by span.
type Section struct {
	Name   string `json:"name"`
	Source Span   `json:"source"`
}

// Document is the approved body in normalized form. Every field carries the span of the
// text it came from.
type Document struct {
	Title          string           `json:"title"`
	Task           string           `json:"task"`
	TaskSource     Span             `json:"task_source"`
	Requirements   []Requirement    `json:"requirements"`
	Inputs         []InputLine      `json:"inputs"`
	Decisions      []Decision       `json:"decisions"`
	Steps          []Step           `json:"steps"`
	FinalCriteria  []FinalCriterion `json:"final_criteria"`
	Traceability   []TraceRow       `json:"traceability"`
	Sections       []Section        `json:"sections"`
	BodySHA256     string           `json:"body_sha256"`
	GrammarVersion string           `json:"grammar_version"`
}

// Step returns the step with id, or nil.
func (d *Document) Step(id string) *Step {
	for i := range d.Steps {
		if d.Steps[i].ID == id {
			return &d.Steps[i]
		}
	}
	return nil
}

// Criterion returns the criterion with id and its requirement, or nils.
func (d *Document) Criterion(id string) (*Criterion, *Requirement) {
	for i := range d.Requirements {
		for j := range d.Requirements[i].Criteria {
			if d.Requirements[i].Criteria[j].ID == id {
				return &d.Requirements[i].Criteria[j], &d.Requirements[i]
			}
		}
	}
	return nil, nil
}

// Verification returns the verification with a scoped id ("S-001/V-001"), or nil.
func (d *Document) Verification(scoped string) *Verification {
	for i := range d.Steps {
		for j := range d.Steps[i].Verifications {
			if d.Steps[i].Verifications[j].ScopedID == scoped {
				return &d.Steps[i].Verifications[j]
			}
		}
	}
	return nil
}

// Section returns the span of the named section, or nil.
func (d *Document) Section(name string) *Section {
	for i := range d.Sections {
		if d.Sections[i].Name == name {
			return &d.Sections[i]
		}
	}
	return nil
}
