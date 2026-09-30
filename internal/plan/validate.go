package plan

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
)

// ErrSemantic marks a plan that parses but whose references, coverage or graph are invalid.
var ErrSemantic = errors.New("plan contract check failed")

// Problems collects semantic violations; the importer reports all of them at once.
type Problems []string

func (p Problems) Err() error {
	if len(p) == 0 {
		return nil
	}
	return &ProblemsError{Problems: p}
}

// ProblemsError carries the list of semantic violations.
type ProblemsError struct{ Problems Problems }

func (e *ProblemsError) Error() string {
	return fmt.Sprintf("%v:\n  - %s", ErrSemantic, strings.Join(e.Problems, "\n  - "))
}

func (e *ProblemsError) Unwrap() error { return ErrSemantic }

// Validate applies the checks Shogun applies before approval, plus the ones execution
// needs: a unique registry, criterion ids owned by their requirement, unique steps,
// resolvable and acyclic dependencies, known criteria and requirements, every step
// citing a criterion, every mandatory criterion assigned to a step or the end-to-end
// check, unique verification ids within a step, and target paths that stay inside their
// repository.
func Validate(doc *Document) error {
	var p Problems
	if len(doc.Requirements) == 0 {
		p = append(p, "the requirements registry is empty")
	}
	reqs, crits := map[string]bool{}, map[string]bool{}
	for _, r := range doc.Requirements {
		if reqs[r.ID] {
			p = append(p, "duplicate requirement id "+r.ID)
		}
		reqs[r.ID] = true
		if len(r.Criteria) == 0 {
			p = append(p, r.ID+" has no criteria")
		}
		for _, c := range r.Criteria {
			if !strings.HasPrefix(c.ID, r.ID+".C") {
				p = append(p, fmt.Sprintf("criterion %s does not belong to %s", c.ID, r.ID))
			}
			if crits[c.ID] {
				p = append(p, "duplicate criterion id "+c.ID)
			}
			crits[c.ID] = true
		}
	}
	steps := map[string]*Step{}
	for i := range doc.Steps {
		s := &doc.Steps[i]
		if steps[s.ID] != nil {
			p = append(p, "duplicate step id "+s.ID)
		}
		steps[s.ID] = s
	}
	assigned := map[string]bool{}
	for _, s := range doc.Steps {
		seen := map[string]bool{}
		for _, d := range s.DependsOn {
			switch {
			case d == s.ID:
				p = append(p, s.ID+" depends on itself")
			case seen[d]:
				p = append(p, fmt.Sprintf("%s lists dependency %s twice", s.ID, d))
			case steps[d] == nil:
				p = append(p, fmt.Sprintf("%s depends on unknown step %s", s.ID, d))
			}
			seen[d] = true
		}
		for _, r := range s.RequirementIDs {
			if !reqs[r] {
				p = append(p, fmt.Sprintf("%s references unknown requirement %s", s.ID, r))
			}
		}
		if len(s.CriterionIDs) == 0 {
			p = append(p, s.ID+" references no criterion")
		}
		for _, c := range s.CriterionIDs {
			if !crits[c] {
				p = append(p, fmt.Sprintf("%s references unknown criterion %s", s.ID, c))
			}
			assigned[c] = true
		}
		vs := map[string]bool{}
		for _, v := range s.Verifications {
			if vs[v.ID] {
				p = append(p, fmt.Sprintf("%s: duplicate verification id %s", s.ID, v.ID))
			}
			vs[v.ID] = true
		}
		for _, t := range s.Targets {
			if why := badPath(t.Path); why != "" {
				p = append(p, fmt.Sprintf("%s target %q: %s", s.ID, t.Path, why))
			}
		}
	}
	finals := map[string]bool{}
	for _, f := range doc.FinalCriteria {
		if finals[f.ID] {
			p = append(p, "end-to-end criterion "+f.ID+" is listed twice")
		}
		finals[f.ID] = true
		assigned[f.ID] = true
	}
	for _, r := range doc.Requirements {
		if !r.Mandatory {
			continue
		}
		for _, c := range r.Criteria {
			if !assigned[c.ID] {
				p = append(p, "mandatory criterion "+c.ID+" is not assigned to any step or the end-to-end verification")
			}
		}
	}
	if cyc := findCycle(doc.Steps); cyc != "" {
		p = append(p, "dependency cycle: "+cyc)
	}
	return p.Err()
}

// badPath explains why a target path cannot be a repository-relative path: absolute,
// escaping with "..", empty, not in clean slash form, or carrying control characters,
// backslashes or backticks. Glob characters are allowed: a target is a focus, not a grant.
func badPath(p string) string {
	switch {
	case p == "":
		return "empty path"
	case strings.HasPrefix(p, "/"):
		return "absolute path"
	case strings.ContainsAny(p, "\\`"):
		return "backslash or backtick in path"
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return "control character in path"
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "path escapes the repository"
		}
	}
	if path.Clean(p) != p && path.Clean(p)+"/" != p {
		return "path is not in clean form"
	}
	return ""
}

// findCycle returns one dependency cycle as "S-001 -> S-002 -> S-001", or "".
func findCycle(steps []Step) string {
	deps := map[string][]string{}
	for _, s := range steps {
		deps[s.ID] = append([]string(nil), s.DependsOn...)
		sort.Strings(deps[s.ID])
	}
	ids := make([]string, 0, len(deps))
	for id := range deps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var visit func(string) string
	visit = func(id string) string {
		color[id] = grey
		stack = append(stack, id)
		for _, d := range deps[id] {
			if _, ok := deps[d]; !ok {
				continue
			}
			switch color[d] {
			case grey:
				return strings.Join(append(stack, d), " -> ")
			case white:
				if c := visit(d); c != "" {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return ""
	}
	for _, id := range ids {
		if color[id] == white {
			if c := visit(id); c != "" {
				return c
			}
		}
	}
	return ""
}

// Order returns the steps in a dependency order: the rendered order where it already
// satisfies the dependencies, otherwise the earliest rendered step whose dependencies are
// done. Validate must have passed.
func Order(doc *Document) []string {
	done := map[string]bool{}
	var out []string
	for len(out) < len(doc.Steps) {
		progressed := false
		for _, s := range doc.Steps {
			if done[s.ID] {
				continue
			}
			ready := true
			for _, d := range s.DependsOn {
				ready = ready && done[d]
			}
			if ready {
				done[s.ID] = true
				out = append(out, s.ID)
				progressed = true
				break
			}
		}
		if !progressed {
			return out // a cycle; Validate reports it
		}
	}
	return out
}
