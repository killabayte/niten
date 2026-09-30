package plan

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ContractKind and ContractVersion identify contract.json.
const (
	ContractKind    = "niten.execution_contract"
	ContractVersion = 1
)

// Contract is the frozen execution contract written by `niten prepare`: the plan identity,
// the normalized approved body, the repository binding, the policy, the owners of every
// criterion and check, and the initial limits. Neither prepare nor any later stage edits
// the approved body or the source execution log.
type Contract struct {
	SchemaVersion   int             `json:"schema_version"`
	Kind            string          `json:"kind"`
	RunID           string          `json:"run_id"`
	CreatedAt       string          `json:"created_at"`
	NitenVersion    string          `json:"niten_version"`
	Plan            Identity        `json:"plan"`
	PlanDigest      string          `json:"plan_digest"`
	SemanticsDigest string          `json:"semantics_digest"`
	Document        *Document       `json:"document"`
	Order           []string        `json:"order"`
	Repos           []RepoBinding   `json:"repos"`
	Inputs          Inputs          `json:"inputs"`
	ShogunVerify    VerifyRecord    `json:"shogun_verify"`
	Criteria        []CriterionPlan `json:"criteria"`
	Checks          []CheckPlan     `json:"checks"`
	Targets         []TargetPlan    `json:"targets"`
	Policy          Policy          `json:"policy"`
	Limits          Limits          `json:"limits"`
	Models          Models          `json:"models"`
	GatePerStep     bool            `json:"gate_per_step"`
}

// Identity pins the approved plan: identifiers from the receipt's immutable metadata and
// the digests of the exact bytes that were verified.
type Identity struct {
	PlanID               string            `json:"plan_id"`
	Revision             int               `json:"revision"`
	Title                string            `json:"title"`
	Project              string            `json:"project"`
	ShogunRunID          string            `json:"shogun_run_id"`
	BodySHA256           string            `json:"body_sha256"`
	ReceiptSHA256        string            `json:"receipt_sha256"`
	ManifestSHA256       string            `json:"manifest_sha256"`
	ManifestDigest       string            `json:"manifest_digest"`
	ManifestSource       string            `json:"manifest_source"` // "sidecar" or "shogun_run"
	RequirementsRevision string            `json:"requirements_revision"`
	ReviewID             string            `json:"review_id"`
	ApprovedAt           string            `json:"approved_at"`
	Planner              string            `json:"planner"`
	Reviewer             string            `json:"reviewer"`
	CLIVersions          map[string]string `json:"cli_versions"`
	Reported             map[string]string `json:"reported"`
	GrammarVersion       string            `json:"grammar_version"`
}

// PlanDigest binds the approved body, the receipt and the manifest bytes into the plan
// hash carried by every candidate id.
func PlanDigest(bodySHA, receiptSHA, manifestSHA string) string {
	return Digest([]byte("niten-plan/1\n" + bodySHA + "\n" + receiptSHA + "\n" + manifestSHA + "\n"))
}

// RepoBinding is the writable repository of the run. Path is a local binding, not an
// identity: identity is the base commit and the fingerprint.
type RepoBinding struct {
	ID           string          `json:"id"`
	Role         string          `json:"role"` // "write"; read-only context repositories need P0b
	Path         string          `json:"path"`
	BoundBy      string          `json:"bound_by"` // "flag" or "manifest_locator"
	ManifestRoot string          `json:"manifest_root"`
	BaseCommit   string          `json:"base_commit"`
	BaseTree     string          `json:"base_tree"`
	Fingerprint  string          `json:"fingerprint"`
	Instructions []BaseFileEntry `json:"instructions"`
	Protected    []BaseFileEntry `json:"protected"`
}

// BaseFileEntry is an instruction or protected entry of the base commit. Instruction file
// copies are stored under inputs/instructions/ and taken from the commit, never from the
// working tree, so a candidate cannot change the next invocation's instructions.
type BaseFileEntry struct {
	Path          string `json:"path"`
	Mode          string `json:"mode"`
	Blob          string `json:"blob"`
	SHA256        string `json:"sha256,omitempty"`
	SymlinkTarget string `json:"symlink_target,omitempty"`
	Pattern       string `json:"pattern"`
	Stored        string `json:"stored,omitempty"`
}

// FileRecord is a copied input file inside the run.
type FileRecord struct {
	Stored string `json:"stored"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ExecutionInput is a planning input whose bytes the run carries. Required inputs are
// named by a verification or a target; the others were supplied voluntarily.
type ExecutionInput struct {
	ID           string   `json:"id"`
	Role         string   `json:"role,omitempty"`
	Required     bool     `json:"required"`
	ReferencedBy []string `json:"referenced_by"`
	SuppliedFrom string   `json:"supplied_from"` // "flag" or "shogun_run"
	FileRecord
}

// PlanningInput records a manifest input the run does not carry: a planning input the
// approved body is self-contained without.
type PlanningInput struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Role   string `json:"role,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// Inputs are the bytes the run was prepared from.
type Inputs struct {
	Plan      FileRecord       `json:"plan"`
	Receipt   FileRecord       `json:"receipt"`
	Manifest  FileRecord       `json:"manifest"`
	Execution []ExecutionInput `json:"execution"`
	Planning  []PlanningInput  `json:"planning"`
}

// VerifyRecord is the recorded `shogun verify --require-manifest` run on the staged copy.
type VerifyRecord struct {
	Command      string   `json:"command"`
	Path         string   `json:"path"`
	RealPath     string   `json:"real_path"`
	BinarySHA256 string   `json:"binary_sha256"`
	Version      string   `json:"version"`
	Args         []string `json:"args"`
	ExitCode     int      `json:"exit_code"`
	Result       string   `json:"result"`
	Note         string   `json:"note"`
}

// CriterionPlan says who closes a criterion and where it can be proved. Owner is "niten"
// unless the user assigned "human" before the run.
type CriterionPlan struct {
	ID            string   `json:"id"`
	RequirementID string   `json:"requirement_id"`
	Mandatory     bool     `json:"mandatory"`
	Owner         string   `json:"owner"`
	Steps         []string `json:"steps"`
	EndToEnd      bool     `json:"end_to_end"`
}

// CheckPlan is one planned verification. CheckSpec is null at prepare: the concrete argv is
// derived later, validated against the policy, and never taken for part of the approved plan.
type CheckPlan struct {
	ScopedID  string           `json:"scoped_id"`
	StepID    string           `json:"step_id"`
	Method    string           `json:"method"`
	RepoID    string           `json:"repo_id"`
	Expected  string           `json:"expected"`
	Owner     string           `json:"owner"`
	CheckSpec *json.RawMessage `json:"check_spec"`
}

// TargetPlan classifies a planned target against the policy. Targets are focus, not grants.
type TargetPlan struct {
	StepID      string `json:"step_id"`
	RepoID      string `json:"repo_id"`
	Path        string `json:"path"`
	Operation   string `json:"operation"`
	Instruction string `json:"instruction_pattern,omitempty"`
	Protected   string `json:"protected_pattern,omitempty"`
}

// Policy is the hard boundary copied from the effective configuration.
type Policy struct {
	ToolNetwork      string          `json:"tool_network"`
	WriteRoots       []string        `json:"write_roots"`
	ProtectedPaths   []string        `json:"protected_paths"`
	InstructionPaths []string        `json:"instruction_paths"`
	Commands         json.RawMessage `json:"commands"`
	RequiredChecks   json.RawMessage `json:"required_checks"`
	StripEnv         []string        `json:"strip_env"`
	VerifierBackend  string          `json:"verifier_backend"`
}

// Limits are the initial limits; later increases are separate events, never rewrites.
type Limits struct {
	MaxInvocations          int      `json:"max_invocations"`
	MaxActiveTime           string   `json:"max_active_time"`
	InvocationDeadline      string   `json:"invocation_deadline"`
	MaxRepairsPerStep       int      `json:"max_repairs_per_step"`
	MaxAheadSteps           int      `json:"max_ahead_steps"`
	FinalReserveInvocations int      `json:"final_reserve_invocations"`
	FinalReserveTime        string   `json:"final_reserve_time"`
	ClaudeMaxBudgetUSD      *float64 `json:"claude_max_budget_usd_per_invocation"`
}

// Models are the requested model specs and commands; P2 certifies them.
type Models struct {
	Executor      string `json:"executor"`
	Reviewer      string `json:"reviewer"`
	ClaudeCommand string `json:"claude_command"`
	CodexCommand  string `json:"codex_command"`
}

// SemanticsDigest hashes what the plan asks to be executed, independent of how Shogun
// produced it: title, task, requirements, decisions, steps, end-to-end criteria and
// traceability without source positions, plus the digests of the Context and Approach
// sections. Plan identity, the inputs list and the review history are excluded, so the
// fast and thorough pipelines yield the same digest for the same approved content.
func SemanticsDigest(doc *Document) (string, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	for _, k := range []string{"sections", "body_sha256", "inputs", "grammar_version"} {
		delete(v, k)
	}
	stripSpans(v)
	ctx := map[string]string{}
	for _, name := range []string{SecContext, SecApproach} {
		if s := doc.Section(name); s != nil {
			ctx[name] = s.Source.SHA256
		}
	}
	v["context_sections"] = ctx
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return Digest(out), nil
}

func stripSpans(v any) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range []string{"source", "table_source", "task_source"} {
			delete(t, k)
		}
		for _, x := range t {
			stripSpans(x)
		}
	case []any:
		for _, x := range t {
			stripSpans(x)
		}
	}
}

var (
	reRepoGit    = regexp.MustCompile("^`(.*)` at `([0-9a-f]+)`, (clean|with local changes \\(fingerprint ([0-9a-f]+)\\))$")
	reRepoNonGit = regexp.MustCompile("^`(.*)` \\(not a git repository, fingerprint ([0-9a-f]+)\\)$")
	reInputFile  = regexp.MustCompile("^`(.*)`, sha256 `([0-9a-f]+)`$")
)

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// CheckInputs cross-checks the "Inputs and versions" section, which the approval binds,
// against the manifest: every repository and every successful input appears exactly once
// with the recorded short head, state or digest. Differing repository base names are
// reported as notes: a root is a locator, not an identity.
func CheckInputs(doc *Document, m *Manifest) (notes []string, err error) {
	lines := map[string]InputLine{}
	for _, l := range doc.Inputs {
		if _, dup := lines[l.ID]; dup {
			return nil, fmt.Errorf("%w: %s appears twice in %q", ErrIntegrity, l.ID, SecInputs)
		}
		lines[l.ID] = l
	}
	for _, r := range m.Repos {
		l, ok := lines[r.ID]
		if !ok {
			return nil, fmt.Errorf("%w: manifest repository %s is not listed in %q", ErrIntegrity, r.ID, SecInputs)
		}
		var base string
		if r.IsGit {
			mm := reRepoGit.FindStringSubmatch(l.Text)
			if mm == nil {
				return nil, fmt.Errorf("%w: %q line of %s does not describe a git repository", ErrIntegrity, SecInputs, r.ID)
			}
			base = mm[1]
			if mm[2] != short(r.Head) {
				return nil, fmt.Errorf("%w: %s head %s in the plan differs from the manifest head %s", ErrIntegrity, r.ID, mm[2], short(r.Head))
			}
			if (mm[3] != "clean") != r.Dirty() {
				return nil, fmt.Errorf("%w: %s state %q in the plan disagrees with the manifest digests", ErrIntegrity, r.ID, mm[3])
			}
			if mm[4] != "" && mm[4] != short(r.Fingerprint) {
				return nil, fmt.Errorf("%w: %s fingerprint in the plan differs from the manifest", ErrIntegrity, r.ID)
			}
		} else {
			mm := reRepoNonGit.FindStringSubmatch(l.Text)
			if mm == nil || mm[2] != short(r.Fingerprint) {
				return nil, fmt.Errorf("%w: %q line of %s does not match the manifest's non-git repository", ErrIntegrity, SecInputs, r.ID)
			}
			base = mm[1]
		}
		if base != filepath.Base(r.Root) {
			notes = append(notes, fmt.Sprintf("%s is named %q in the plan but the manifest locator ends in %q", r.ID, base, filepath.Base(r.Root)))
		}
		delete(lines, r.ID)
	}
	for _, in := range m.Inputs {
		l, ok := lines[in.ID]
		if in.Status != "ok" {
			if ok {
				return nil, fmt.Errorf("%w: failed input %s is listed in %q", ErrIntegrity, in.ID, SecInputs)
			}
			continue
		}
		if !ok {
			return nil, fmt.Errorf("%w: manifest input %s is not listed in %q", ErrIntegrity, in.ID, SecInputs)
		}
		mm := reInputFile.FindStringSubmatch(l.Text)
		if mm == nil || mm[2] != short(in.SHA256) {
			return nil, fmt.Errorf("%w: %q line of %s does not match the manifest digest", ErrIntegrity, SecInputs, in.ID)
		}
		delete(lines, in.ID)
	}
	for id, l := range lines {
		// Archived web sources are listed here without a manifest entry.
		if !strings.Contains(l.Text, ", archived sha256 `") {
			return nil, fmt.Errorf("%w: %q lists %s, which the manifest does not know", ErrIntegrity, SecInputs, id)
		}
	}
	return notes, nil
}
