// Package prepare implements `niten prepare`: it turns a published Shogun triplet into a
// run with a frozen execution contract. It works on private copies of the inputs, calls
// `shogun verify` offline, recomputes the manifest digest, checks the source repository
// against the approved planning base, and records who owns every criterion and check.
// It starts no model, runs nothing from the plan text, and never writes to the source
// plan or the source repository.
package prepare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/config"
	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/pathglob"
	"github.com/killabayte/niten/internal/plan"
	"github.com/killabayte/niten/internal/store"
	"github.com/killabayte/niten/internal/workspace"
)

// Size limits for the copied inputs.
const (
	maxPlanBytes     = 4 << 20
	maxReceiptBytes  = 1 << 20
	maxManifestBytes = 4 << 20
	maxInputBytes    = 10 << 20 // Shogun's own per-input limit
)

// Options are the inputs of one prepare.
type Options struct {
	PlanPath    string
	Repos       map[string]string // repo id → local path; empty means the manifest locator
	Inputs      map[string]string // input id → local file with the recorded bytes
	Human       []string          // criterion ids or scoped verification ids owned by a human
	GatePerStep *bool             // nil keeps the configured value
	ShogunRun   string            // optional legacy run directory holding manifest.json
	Config      *config.Loaded
	Version     string
	Now         func() time.Time
}

// Result is a prepared run.
type Result struct {
	RunID    string
	RunDir   string
	Contract *plan.Contract
	Notes    []string
}

// Failure is a refusal with a stable reason code and the CLI exit code it maps to.
type Failure struct {
	Exit    contract.ExitCode
	Reason  string
	Details []string
}

func (f *Failure) Error() string {
	if len(f.Details) == 0 {
		return f.Reason
	}
	return f.Reason + ": " + strings.Join(f.Details, "; ")
}

func fail(exit contract.ExitCode, reason string, format string, a ...any) *Failure {
	return &Failure{Exit: exit, Reason: reason, Details: []string{fmt.Sprintf(format, a...)}}
}

// Reason codes.
const (
	ReasonConfig             = "invalid_arguments"
	ReasonInputFile          = "input_file_rejected"
	ReasonManifestMissing    = "manifest_missing"
	ReasonManifestConflict   = "manifest_disagreement"
	ReasonShogunVerify       = "shogun_verify_failed"
	ReasonPlanChanged        = "plan_changed"
	ReasonInvalidFormat      = "invalid_format"
	ReasonManifestMismatch   = "manifest_mismatch"
	ReasonContract           = "plan_contract_invalid"
	ReasonUnsupported        = "unsupported_mode"
	ReasonRepo               = "repository_rejected"
	ReasonDrift              = "repository_drift"
	ReasonUnknownRef         = "unknown_reference"
	ReasonProtectedTarget    = "protected_target"
	ReasonNeedsInput         = "needs_input"
	ReasonStore              = "store_error"
	NeedsMeasure             = "unsupported_method_measure"
	NeedsInputMissing        = "required_input_missing"
	NeedsInputMismatch       = "required_input_mismatch"
	NeedsInputUnavailable    = "required_input_unavailable_at_planning"
	contractFile             = "contract.json"
	stateFile                = "state.json"
	configFile               = "config.json"
	stagedPlan               = "inputs/plan.md"
	stagedReceipt            = "inputs/plan.approval.json"
	stagedManifest           = "inputs/plan.manifest.json"
	manifestSourceSidecar    = "sidecar"
	manifestSourceShogunRun  = "shogun_run"
	boundByFlag              = "flag"
	boundByManifestLocator   = "manifest_locator"
	suppliedFromFlag         = "flag"
	suppliedFromShogunRunDir = "shogun_run"
)

// Prepare runs the whole intake. A returned *Failure says why no run was created; nothing
// is left in the store in that case.
func Prepare(ctx context.Context, o Options) (*Result, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	now := o.Now().UTC()
	cfg := o.Config.Config
	var notes []string

	planPath, err := filepath.Abs(o.PlanPath)
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonConfig, "%v", err)
	}
	if !strings.HasSuffix(planPath, ".md") {
		return nil, fail(contract.ExitFormat, ReasonConfig, "the plan %s is not a .md file", planPath)
	}
	stem := strings.TrimSuffix(planPath, ".md")

	// 1. Private copies. From here on only these bytes are used.
	planBytes, err := readInput(planPath, maxPlanBytes)
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonInputFile, "plan: %v", err)
	}
	receiptBytes, err := readInput(stem+".approval.json", maxReceiptBytes)
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonInputFile, "receipt: %v", err)
	}
	manifestBytes, source, err := readManifest(stem+".manifest.json", o.ShogunRun)
	if err != nil {
		return nil, err
	}

	// Nothing is written anywhere before the store and the temporary directory are known
	// to lie outside every repository the plan could bind: the explicit bindings and the
	// manifest locators. The store itself is created only once every check has passed.
	storeRoot, err := store.Locate(cfg.StoreDir)
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonStore, "%v", err)
	}
	tmpBase, err := workspace.Canonical(os.TempDir())
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonStore, "temporary directory: %v", err)
	}
	var candidates []string
	for _, p := range o.Repos {
		candidates = append(candidates, p)
	}
	if m, err := plan.DecodeManifest(manifestBytes); err == nil {
		for _, r := range m.Repos {
			candidates = append(candidates, r.Root)
		}
	}
	for _, c := range candidates {
		if canon, err := workspace.Canonical(c); err == nil {
			if f := outside(storeRoot, tmpBase, canon); f != nil {
				return nil, f
			}
		}
	}
	runID, err := store.NewRunID(now)
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonStore, "%v", err)
	}
	var files []pending
	files = append(files, pending{stagedPlan, planBytes, 0o400}, pending{stagedReceipt, receiptBytes, 0o400}, pending{stagedManifest, manifestBytes, 0o400})

	// 2. Shogun's own verification of a private copy of the triplet.
	verifyDir, err := os.MkdirTemp("", "niten-verify-")
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonStore, "temporary directory: %v", err)
	}
	defer os.RemoveAll(verifyDir)
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(verifyDir, filepath.Base(f.rel)), f.data, 0o400); err != nil {
			return nil, fail(contract.ExitFormat, ReasonStore, "temporary copy: %v", err)
		}
	}
	vrec, err := runShogunVerify(ctx, cfg.ShogunCommand, filepath.Join(verifyDir, filepath.Base(stagedPlan)))
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonShogunVerify, "%v", err)
	}
	switch vrec.Result {
	case "valid":
	case "changed":
		return nil, fail(contract.ExitRejected, ReasonPlanChanged, "shogun verify: %s", vrec.Note)
	default:
		return nil, fail(contract.ExitFormat, ReasonShogunVerify, "shogun verify: %s: %s", vrec.Result, vrec.Note)
	}

	// 3. Independent checks of the same bytes: body digest, receipt, manifest digest.
	split, err := plan.SplitBody(planBytes)
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonInvalidFormat, "%v", err)
	}
	receipt, md, err := plan.DecodeReceipt(receiptBytes)
	if err != nil {
		return nil, classify(err, ReasonInvalidFormat)
	}
	if split.BodySHA256 != receipt.BodySHA256 {
		return nil, fail(contract.ExitRejected, ReasonPlanChanged, "the approved body digest %.12s differs from the receipt's %.12s", split.BodySHA256, receipt.BodySHA256)
	}
	manifest, err := plan.DecodeManifest(manifestBytes)
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonInvalidFormat, "%v", err)
	}
	if err := plan.CheckManifest(manifest, receipt); err != nil {
		return nil, fail(contract.ExitRejected, ReasonManifestMismatch, "%v", err)
	}

	// 4. The approved body in the supported grammar, and its semantic contract.
	doc, err := plan.Parse(split.Body)
	if err != nil {
		var fe *plan.FormatError
		if errors.As(err, &fe) && fe.Line > 0 {
			return nil, fail(contract.ExitFormat, ReasonInvalidFormat, "%v (plan file line %d)", err, split.BodyLine+fe.Line-1)
		}
		return nil, fail(contract.ExitFormat, ReasonInvalidFormat, "%v", err)
	}
	if doc.Title != md.Title {
		return nil, fail(contract.ExitRejected, ReasonPlanChanged, "the body title %q differs from the approved title %q", doc.Title, md.Title)
	}
	if err := plan.Validate(doc); err != nil {
		var pe *plan.ProblemsError
		if errors.As(err, &pe) {
			return nil, &Failure{Exit: contract.ExitFormat, Reason: ReasonContract, Details: pe.Problems}
		}
		return nil, fail(contract.ExitFormat, ReasonContract, "%v", err)
	}
	inputNotes, err := plan.CheckInputs(doc, manifest)
	if err != nil {
		return nil, fail(contract.ExitRejected, ReasonManifestMismatch, "%v", err)
	}
	notes = append(notes, inputNotes...)

	// 5. The v0.1 scope: one clean git repository.
	if len(manifest.Repos) != 1 {
		var ids []string
		for _, r := range manifest.Repos {
			ids = append(ids, r.ID)
		}
		return nil, fail(contract.ExitFormat, ReasonUnsupported, "the plan studied %d repositories (%s); v0.1 executes exactly one writable repository, and read-only context repositories need the separate P0b certification", len(ids), strings.Join(ids, ", "))
	}
	mrepo := manifest.Repos[0]
	switch {
	case !mrepo.IsGit:
		return nil, fail(contract.ExitFormat, ReasonUnsupported, "%s was planned as a non-git directory; v0.1 executes git repositories only", mrepo.ID)
	case mrepo.Head == "":
		return nil, fail(contract.ExitFormat, ReasonUnsupported, "%s was planned with an unborn HEAD; v0.1 needs an existing commit", mrepo.ID)
	case mrepo.Dirty():
		return nil, fail(contract.ExitFormat, ReasonUnsupported, "%s was planned with local changes (fingerprint %.12s); the manifest pins digests, not the dirty bytes, so the planning base cannot be reproduced (dirty snapshots are v0.2 work)", mrepo.ID, mrepo.Fingerprint)
	}

	// 6. The repository binding and drift against the approved base.
	for id := range o.Repos {
		if id != mrepo.ID {
			return nil, fail(contract.ExitFormat, ReasonUnknownRef, "--repo %s: the plan has no repository %s (known: %s)", id, id, mrepo.ID)
		}
	}
	repoPath, boundBy := o.Repos[mrepo.ID], boundByFlag
	if repoPath == "" {
		repoPath, boundBy = mrepo.Root, boundByManifestLocator
		notes = append(notes, fmt.Sprintf("%s bound to %s from the manifest locator; pass --repo %s=PATH to choose another checkout", mrepo.ID, repoPath, mrepo.ID))
	}
	exclude := ownOutputs(planPath)
	src, err := workspace.Inspect(ctx, repoPath, exclude)
	if errors.Is(err, workspace.ErrUnsupported) {
		return nil, fail(contract.ExitFormat, ReasonUnsupported, "%s at %s: %v", mrepo.ID, repoPath, err)
	}
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonRepo, "%s at %s: %v", mrepo.ID, repoPath, err)
	}
	if f := outside(storeRoot, tmpBase, src.Root); f != nil {
		return nil, f
	}
	if err := drift(mrepo, src); err != nil {
		return nil, err
	}
	base, err := workspace.InventoryBase(ctx, src.Root, src.Head, cfg.Policy.InstructionPaths, cfg.Policy.ProtectedPaths)
	if errors.Is(err, workspace.ErrUnsupported) {
		return nil, fail(contract.ExitFormat, ReasonUnsupported, "%s: %v", mrepo.ID, err)
	}
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonRepo, "%s base inventory: %v", mrepo.ID, err)
	}
	if len(base.Unsupported) > 0 {
		return nil, &Failure{Exit: contract.ExitFormat, Reason: ReasonUnsupported, Details: append([]string{"the base commit uses features v0.1 does not execute"}, base.Unsupported...)}
	}

	// 7. References, targets and owners.
	inputsByID := map[string]plan.ManifestInput{}
	for _, in := range manifest.Inputs {
		inputsByID[in.ID] = in
	}
	required := map[string][]string{} // input id → referencing ids
	var targets []plan.TargetPlan
	var bad []string
	for _, s := range doc.Steps {
		for _, t := range s.Targets {
			tp := plan.TargetPlan{StepID: s.ID, RepoID: t.RepoID, Path: t.Path, Operation: t.Operation,
				Instruction: pathglob.MatchAny(cfg.Policy.InstructionPaths, t.Path), Protected: pathglob.MatchAny(cfg.Policy.ProtectedPaths, t.Path)}
			switch _, isInput := inputsByID[t.RepoID]; {
			case t.RepoID == mrepo.ID:
			case isInput && t.Operation == "inspect":
				required[t.RepoID] = append(required[t.RepoID], s.ID+" target "+t.Path)
			default:
				bad = append(bad, fmt.Sprintf("%s target %s names unknown repository %q", s.ID, t.Path, t.RepoID))
			}
			if tp.Protected != "" && t.Operation != "inspect" {
				return nil, fail(contract.ExitFormat, ReasonProtectedTarget, "%s would %s %s, a protected path (%s); editing protected paths needs a separately verified mode that v0.1 does not have", s.ID, t.Operation, t.Path, tp.Protected)
			}
			if tp.Instruction != "" && t.Operation != "inspect" {
				notes = append(notes, fmt.Sprintf("%s explicitly targets the instruction path %s (%s); its new text stays review material and is not loaded as instructions", s.ID, t.Path, t.Operation))
			}
			targets = append(targets, tp)
		}
		for _, v := range s.Verifications {
			switch _, isInput := inputsByID[v.RepoID]; {
			case v.RepoID == "" || v.RepoID == mrepo.ID:
			case isInput:
				required[v.RepoID] = append(required[v.RepoID], v.ScopedID)
			default:
				bad = append(bad, fmt.Sprintf("%s names unknown repository or input %q", v.ScopedID, v.RepoID))
			}
		}
	}
	if len(bad) > 0 {
		return nil, &Failure{Exit: contract.ExitFormat, Reason: ReasonUnknownRef, Details: bad}
	}
	human := map[string]bool{}
	for _, id := range o.Human {
		c, _ := doc.Criterion(id)
		if c == nil && doc.Verification(id) == nil {
			return nil, fail(contract.ExitFormat, ReasonUnknownRef, "--human %s: no such criterion or scoped verification (use R-001.C1 or S-001/V-001)", id)
		}
		human[id] = true
	}
	var criteria []plan.CriterionPlan
	for _, r := range doc.Requirements {
		for _, c := range r.Criteria {
			cp := plan.CriterionPlan{ID: c.ID, RequirementID: r.ID, Mandatory: r.Mandatory, Owner: string(contract.OwnerNiten), Steps: []string{}}
			if human[c.ID] {
				cp.Owner = string(contract.OwnerHuman)
			}
			for _, s := range doc.Steps {
				if slices.Contains(s.CriterionIDs, c.ID) {
					cp.Steps = append(cp.Steps, s.ID)
				}
			}
			for _, f := range doc.FinalCriteria {
				cp.EndToEnd = cp.EndToEnd || f.ID == c.ID
			}
			criteria = append(criteria, cp)
		}
	}
	var checks []plan.CheckPlan
	var needs []string
	for _, s := range doc.Steps {
		for _, v := range s.Verifications {
			cp := plan.CheckPlan{ScopedID: v.ScopedID, StepID: s.ID, Method: v.Method, RepoID: v.RepoID, Expected: v.Expected, Owner: string(contract.OwnerNiten)}
			if human[v.ScopedID] {
				cp.Owner = string(contract.OwnerHuman)
			}
			if v.Method == string(contract.MethodMeasure) && cp.Owner == string(contract.OwnerNiten) {
				needs = append(needs, fmt.Sprintf("%s: %s uses the measure method, which v0.1 cannot execute; assign it to a human with --human %s or approve a new plan revision", NeedsMeasure, v.ScopedID, v.ScopedID))
			}
			checks = append(checks, cp)
		}
	}

	// 8. Execution inputs: required ones must be supplied with the recorded bytes.
	for id := range o.Inputs {
		if _, ok := inputsByID[id]; !ok {
			return nil, fail(contract.ExitFormat, ReasonUnknownRef, "--input %s: the manifest has no input %s", id, id)
		}
	}
	var execInputs []plan.ExecutionInput
	var planning []plan.PlanningInput
	for _, in := range manifest.Inputs {
		refs, isRequired := required[in.ID]
		path, from := o.Inputs[in.ID], suppliedFromFlag
		if path == "" && isRequired && o.ShogunRun != "" && in.StoredPath != "" {
			path, from = filepath.Join(o.ShogunRun, filepath.FromSlash(in.StoredPath)), suppliedFromShogunRunDir
			if !workspace.Within(filepath.Clean(path), filepath.Clean(o.ShogunRun)) {
				path = ""
			}
		}
		if path == "" {
			if isRequired {
				if in.Status != "ok" {
					needs = append(needs, fmt.Sprintf("%s: %s (referenced by %s) was unavailable when Shogun planned (%s); a new plan revision is needed", NeedsInputUnavailable, in.ID, strings.Join(refs, ", "), in.Error))
				} else {
					needs = append(needs, fmt.Sprintf("%s: %s (%s, referenced by %s) must be supplied with --input %s=PATH; its sha256 must be %s", NeedsInputMissing, in.ID, in.Origin, strings.Join(refs, ", "), in.ID, in.SHA256))
				}
			} else {
				planning = append(planning, plan.PlanningInput{ID: in.ID, Status: in.Status, Role: in.Role, SHA256: in.SHA256})
			}
			continue
		}
		if in.Status != "ok" {
			needs = append(needs, fmt.Sprintf("%s: %s was unavailable when Shogun planned; there is no recorded digest to check a supplied file against", NeedsInputUnavailable, in.ID))
			continue
		}
		data, err := readInput(path, maxInputBytes)
		if err != nil {
			return nil, fail(contract.ExitFormat, ReasonInputFile, "input %s: %v", in.ID, err)
		}
		if got := plan.Digest(data); got != in.SHA256 {
			needs = append(needs, fmt.Sprintf("%s: %s supplied from %s has sha256 %.12s, the plan was made with %.12s", NeedsInputMismatch, in.ID, path, got, in.SHA256))
			continue
		}
		rel := "inputs/execution/" + in.ID
		files = append(files, pending{rel, data, 0o400})
		if refs == nil {
			refs = []string{}
		}
		execInputs = append(execInputs, plan.ExecutionInput{ID: in.ID, Role: in.Role, Required: isRequired, ReferencedBy: refs, SuppliedFrom: from,
			FileRecord: plan.FileRecord{Stored: rel, SHA256: in.SHA256, Size: int64(len(data))}})
	}
	if len(needs) > 0 {
		sort.Strings(needs)
		return nil, &Failure{Exit: contract.ExitNeedsInput, Reason: ReasonNeedsInput, Details: needs}
	}

	// 9. Base instructions come from the pinned commit.
	binding := plan.RepoBinding{ID: mrepo.ID, Role: "write", Path: src.Root, BoundBy: boundBy, ManifestRoot: mrepo.Root,
		BaseCommit: src.Head, BaseTree: base.Tree, Fingerprint: src.Fingerprint, Instructions: []plan.BaseFileEntry{}, Protected: []plan.BaseFileEntry{}}
	for _, f := range base.Instructions {
		e := entry(f)
		if f.Content != nil {
			e.Stored = "inputs/instructions/" + f.Path
			files = append(files, pending{e.Stored, f.Content, 0o400})
		}
		binding.Instructions = append(binding.Instructions, e)
	}
	for _, f := range base.Protected {
		binding.Protected = append(binding.Protected, entry(f))
	}

	// 10. The contract.
	gate := cfg.GatePerStep
	if o.GatePerStep != nil {
		gate = *o.GatePerStep
	}
	commands, _ := json.Marshal(cfg.Policy.Commands)
	requiredChecks, _ := json.Marshal(cfg.Checks.Required)
	sem, err := plan.SemanticsDigest(doc)
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonStore, "%v", err)
	}
	rsha, msha := plan.Digest(receiptBytes), plan.Digest(manifestBytes)
	c := &plan.Contract{
		SchemaVersion: plan.ContractVersion, Kind: plan.ContractKind, RunID: runID,
		CreatedAt: now.Format(time.RFC3339), NitenVersion: o.Version,
		Plan: plan.Identity{PlanID: md.PlanID, Revision: md.Revision, Title: md.Title, Project: md.Project, ShogunRunID: md.RunID,
			BodySHA256: split.BodySHA256, ReceiptSHA256: rsha, ManifestSHA256: msha, ManifestDigest: receipt.ManifestDigest,
			ManifestSource: source, RequirementsRevision: receipt.RequirementsRevision, ReviewID: receipt.ReviewID,
			ApprovedAt: receipt.ApprovedAt, Planner: md.Planner, Reviewer: md.Reviewer,
			CLIVersions: receipt.CLIVersions, Reported: receipt.Reported, GrammarVersion: plan.GrammarVersion},
		PlanDigest: plan.PlanDigest(split.BodySHA256, rsha, msha), SemanticsDigest: sem,
		Document: doc, Order: plan.Order(doc), Repos: []plan.RepoBinding{binding},
		Inputs: plan.Inputs{
			Plan:      plan.FileRecord{Stored: stagedPlan, SHA256: plan.Digest(planBytes), Size: int64(len(planBytes))},
			Receipt:   plan.FileRecord{Stored: stagedReceipt, SHA256: rsha, Size: int64(len(receiptBytes))},
			Manifest:  plan.FileRecord{Stored: stagedManifest, SHA256: msha, Size: int64(len(manifestBytes))},
			Execution: nonNil(execInputs), Planning: nonNil(planning)},
		ShogunVerify: *vrec, Criteria: criteria, Checks: checks, Targets: nonNil(targets),
		Policy: plan.Policy{ToolNetwork: cfg.Policy.ToolNetwork, WriteRoots: cfg.Policy.WriteRoots, ProtectedPaths: cfg.Policy.ProtectedPaths,
			InstructionPaths: cfg.Policy.InstructionPaths, Commands: commands, RequiredChecks: requiredChecks, StripEnv: cfg.StripEnv,
			VerifierBackend: cfg.VerifierBackend},
		Limits: plan.Limits{MaxInvocations: cfg.MaxInvocations, MaxActiveTime: cfg.MaxActiveTime.D().String(), InvocationDeadline: cfg.InvocationDeadline.D().String(),
			MaxRepairsPerStep: cfg.MaxRepairsPerStep, MaxAheadSteps: cfg.MaxAheadSteps, FinalReserveInvocations: cfg.FinalReserveInvocation,
			FinalReserveTime: cfg.FinalReserveTime.D().String(), ClaudeMaxBudgetUSD: cfg.ClaudeMaxBudgetUSD},
		Models:      plan.Models{Executor: cfg.Executor, Reviewer: cfg.Reviewer, ClaudeCommand: cfg.ClaudeCommand, CodexCommand: cfg.CodexCommand},
		GatePerStep: gate,
	}
	cbytes, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return nil, fail(contract.ExitFormat, ReasonStore, "%v", err)
	}
	cbytes = append(cbytes, '\n')
	cfgBytes, _ := json.MarshalIndent(o.Config, "", " ")
	state := map[string]any{"schema_version": 1, "run_id": runID, "state": string(contract.RunPrepared),
		"contract_sha256": plan.Digest(cbytes), "plan_digest": c.PlanDigest, "updated_at": c.CreatedAt}
	sbytes, _ := json.MarshalIndent(state, "", " ")
	files = append(files, pending{contractFile, cbytes, 0o600}, pending{configFile, append(cfgBytes, '\n'), 0o600}, pending{stateFile, append(sbytes, '\n'), 0o600})

	// 11. Every check passed: only now the store is created and the run written.
	dir, err := publish(cfg.StoreDir, storeRoot, tmpBase, src.Root, runID, files)
	if err != nil {
		return nil, err
	}
	return &Result{RunID: runID, RunDir: dir, Contract: c, Notes: notes}, nil
}

// pending is a run file held in memory until the run is published.
type pending struct {
	rel  string
	data []byte
	mode os.FileMode
}

// outside refuses a store or temporary directory that overlaps the repository.
func outside(storeRoot, tmpBase, repo string) *Failure {
	if workspace.Within(storeRoot, repo) || workspace.Within(repo, storeRoot) {
		return fail(contract.ExitFormat, ReasonStore, "the store %s and the repository %s must not contain each other", storeRoot, repo)
	}
	if workspace.Within(tmpBase, repo) {
		return fail(contract.ExitFormat, ReasonStore, "the temporary directory %s lies inside the repository %s; point TMPDIR elsewhere", tmpBase, repo)
	}
	return nil
}

// publish opens the store, re-checks its canonical root against the repository, writes
// every file into a staging directory and renames it into place.
func publish(storeDir, located, tmpBase, repo, runID string, files []pending) (string, error) {
	st, err := store.Open(storeDir)
	if err != nil {
		return "", fail(contract.ExitFormat, ReasonStore, "%v", err)
	}
	if st.Root != located {
		if f := outside(st.Root, tmpBase, repo); f != nil {
			return "", f
		}
	}
	stage, err := st.Stage(runID)
	if err != nil {
		return "", fail(contract.ExitFormat, ReasonStore, "%v", err)
	}
	for _, f := range files {
		if err := stage.Write(f.rel, f.data, f.mode); err != nil {
			stage.Abort()
			return "", fail(contract.ExitFormat, ReasonStore, "%v", err)
		}
	}
	dir, err := stage.Commit()
	if err != nil {
		stage.Abort()
		return "", fail(contract.ExitFormat, ReasonStore, "%v", err)
	}
	return dir, nil
}

// readInput reads a regular file that is not a symlink, up to max bytes.
func readInput(path string, max int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; inputs are not followed through links", path)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, max)
	}
	return data, nil
}

// readManifest returns the manifest bytes from the sidecar or a legacy run directory. When
// both exist they must be byte-identical: the sidecar is the run's frozen manifest.
func readManifest(sidecar, runDir string) ([]byte, string, error) {
	side, sideErr := readInput(sidecar, maxManifestBytes)
	if sideErr != nil && !errors.Is(sideErr, os.ErrNotExist) {
		return nil, "", fail(contract.ExitFormat, ReasonInputFile, "manifest sidecar: %v", sideErr)
	}
	if runDir == "" {
		if sideErr != nil {
			return nil, "", fail(contract.ExitFormat, ReasonManifestMissing, "%s is missing; strict execution needs the approved generation's manifest. Plans published before Shogun S0 have none: pass --shogun-run RUN_DIR if the original run still exists, or approve a new plan", sidecar)
		}
		return side, manifestSourceSidecar, nil
	}
	run, err := readInput(filepath.Join(runDir, "manifest.json"), maxManifestBytes)
	if err != nil {
		return nil, "", fail(contract.ExitFormat, ReasonInputFile, "--shogun-run manifest: %v", err)
	}
	if sideErr == nil {
		if !bytes.Equal(side, run) {
			return nil, "", fail(contract.ExitRejected, ReasonManifestConflict, "the sidecar %s and %s/manifest.json differ; neither is preferred implicitly", sidecar, runDir)
		}
		return side, manifestSourceSidecar, nil
	}
	return run, manifestSourceShogunRun, nil
}

// ownOutputs are the plan triplet paths, which Shogun excludes from repository fingerprints
// when it publishes into the studied repository. These are the verified paths of this
// prepare, not the manifest's unverified exclude list.
func ownOutputs(planPath string) []string {
	stem := strings.TrimSuffix(planPath, ".md")
	var out []string
	for _, p := range []string{planPath, stem + ".approval.json", stem + ".manifest.json"} {
		if c, err := workspace.Canonical(p); err == nil {
			out = append(out, c)
		}
	}
	return out
}

func drift(m plan.ManifestRepo, s *workspace.Source) *Failure {
	var d []string
	if s.Head != m.Head {
		d = append(d, fmt.Sprintf("HEAD is %.12s, the plan was approved at %.12s", s.Head, m.Head))
	}
	if s.TrackedChanges {
		d = append(d, "tracked files have uncommitted changes")
	}
	if len(s.Untracked) > 0 {
		d = append(d, "untracked files: "+strings.Join(s.Untracked, ", "))
	}
	if len(d) == 0 && s.Fingerprint != m.Fingerprint {
		d = append(d, fmt.Sprintf("fingerprint %.12s differs from the approved %.12s", s.Fingerprint, m.Fingerprint))
	}
	if len(d) == 0 {
		return nil
	}
	return &Failure{Exit: contract.ExitRejected, Reason: ReasonDrift, Details: append([]string{m.ID + " at " + s.Root + " no longer matches the approved planning base; Niten does not rebase: commit or restore the checkout, or approve a new plan revision"}, d...)}
}

func entry(f workspace.BaseFile) plan.BaseFileEntry {
	return plan.BaseFileEntry{Path: f.Path, Mode: f.Mode, Blob: f.Blob, SHA256: f.SHA256, SymlinkTarget: f.SymlinkTarget, Pattern: f.Pattern}
}

func classify(err error, reason string) *Failure {
	if errors.Is(err, plan.ErrIntegrity) {
		return fail(contract.ExitRejected, ReasonPlanChanged, "%v", err)
	}
	return fail(contract.ExitFormat, reason, "%v", err)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
