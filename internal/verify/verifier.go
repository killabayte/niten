// Package verify runs coordinator-owned checks of a candidate. Every check
// gets new roots: the candidate is materialized into a fresh copy, run under
// the certified sandbox backend, and the roots are sealed before anything is
// read from them. Evidence is recorded only when the store accepted it.
package verify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/killabayte/niten/internal/contract"
	"github.com/killabayte/niten/internal/store"
	"github.com/killabayte/niten/internal/verify/sandbox"
	"github.com/killabayte/niten/internal/workspace"
)

// DefaultMaxOutput bounds each captured stream.
const DefaultMaxOutput = 4 << 20

// Verifier runs checks of one run's candidates.
type Verifier struct {
	Backend      *sandbox.Seatbelt
	Clone        *workspace.Clone
	Store        *store.Run
	Root         string            // parent of the per-attempt roots; outside every model write root
	Toolchains   []string          // read-only toolchain roots; bare command names resolve in their bin directories
	ReadOnly     []string          // immutable dependency snapshots
	EnvOverrides map[string]string // passed to sandbox.Environment
	ToolVersion  string            // recorded in the evidence, e.g. "go version go1.26.3 darwin/arm64"
	MaxOutput    int64
	Now          func() time.Time
	// newID is a test hook for the attempt id.
	newID func() string
}

// Check is a validated check_spec: what to run and what to expect.
type Check struct {
	ID       string
	Argv     []string
	Cwd      string // relative to the repository root
	Timeout  time.Duration
	Expected contract.CheckExpected
	// Outputs are repository-relative patterns of files the check may create
	// in the source tree (a coverage profile, say). Any other new file
	// invalidates the evidence: it is code the check added.
	Outputs []string
}

// Request binds a check to a candidate and the plan it serves.
type Request struct {
	Check          Check
	Candidate      workspace.Candidate
	PlanDigest     string
	ContractDigest string
}

// Result is the recorded outcome of one check attempt.
type Result struct {
	AttemptID      string
	Evidence       contract.CheckEvidence
	EvidenceRef    string
	EvidenceDigest string
	Reasons        []string // why the status is not passed
	Sealed         sandbox.Sealed
}

// Run executes one check attempt and records its evidence. An error means
// nothing was recorded (for example the store refused a write); a recorded
// result can still be failed, unknown or invalidated.
func (v *Verifier) Run(ctx context.Context, req Request) (*Result, error) {
	now := v.Now
	if now == nil {
		now = time.Now
	}
	max := v.MaxOutput
	if max <= 0 {
		max = DefaultMaxOutput
	}
	id := v.attemptID(now())
	base := filepath.Join(v.Root, id)
	if _, err := os.Lstat(base); err == nil {
		return nil, fmt.Errorf("check attempt root %s already exists; every attempt needs new roots", base)
	}
	argv, err := v.resolve(req.Check.Argv)
	if err != nil {
		return nil, err
	}
	src, scratch := filepath.Join(base, "src"), filepath.Join(base, "scratch")
	if err := v.Clone.Materialize(ctx, req.Candidate.Commit, src); err != nil {
		return nil, fmt.Errorf("materialize the candidate: %w", err)
	}
	if err := sandbox.PrepareScratch(scratch); err != nil {
		return nil, err
	}
	np, err := sandbox.Policy{SourceRoot: src, ScratchRoot: scratch, Toolchains: v.Toolchains, ReadOnly: v.ReadOnly}.Normalize()
	if err != nil {
		return nil, err
	}
	env, err := sandbox.Environment(np, v.EnvOverrides)
	if err != nil {
		return nil, err
	}
	cwd, err := within(np.SourceRoot, req.Check.Cwd)
	if err != nil {
		return nil, err
	}
	osVer, osBuild := v.Backend.OS()
	key := contract.CheckKey{
		PlanDigest: req.PlanDigest, CandidateCommit: req.Candidate.Commit, CandidateTree: req.Candidate.Tree,
		ContractDigest: req.ContractDigest, CheckID: req.Check.ID,
		CheckSpecDigest:   digestJSON(map[string]any{"argv": req.Check.Argv, "cwd": req.Check.Cwd, "timeout": req.Check.Timeout.String(), "expected": req.Check.Expected, "outputs": req.Check.Outputs}),
		EnvironmentDigest: environmentDigest(env, np, v.Toolchains, v.ReadOnly, v.ToolVersion, osVer, osBuild),
	}

	stdout, stderr := &capped{max: max}, &capped{max: max}
	started := now()
	res, runErr := v.Backend.Run(ctx, np, sandbox.Command{Argv: argv, Dir: cwd, Env: env, Stdout: stdout, Stderr: stderr, Timeout: req.Check.Timeout})
	finished := now()
	sealed, sealErr := v.Backend.Seal(np)

	var reasons []string
	var asserts []contract.Assertion
	assert := func(name string, ok bool, detail string) {
		asserts = append(asserts, contract.Assertion{Name: name, Passed: ok, Detail: detail})
		if !ok {
			reasons = append(reasons, name+": "+detail)
		}
	}
	status := contract.CheckPassed
	var changed []string
	switch {
	case runErr != nil:
		assert("sandboxed run completed", false, runErr.Error())
		status = contract.CheckUnknown
	case sealErr != nil:
		assert("roots sealed before reading outputs", false, sealErr.Error())
		status = contract.CheckUnknown
	default:
		var cerr error
		changed, cerr = v.Clone.SourcesChanged(ctx, req.Candidate.Commit, sealed.SourceRoot, req.Check.Outputs)
		if cerr != nil {
			assert("sources compared after the check", false, cerr.Error())
			status = contract.CheckUnknown
			break
		}
		assert("no process outlived the check", !res.Stragglers, fmt.Sprintf("group members killed after exit: %v", res.SurvivorPIDs))
		assert("sources unchanged by the check", len(changed) == 0, strings.Join(changed, "; "))
		if res.Stragglers || len(changed) > 0 {
			status = contract.CheckInvalidated
		}
		assert("finished within the timeout", !res.TimedOut, req.Check.Timeout.String())
		want := 0
		if req.Check.Expected.ExitCode != nil {
			want = *req.Check.Expected.ExitCode
		}
		assert("exit code", res.Signal == "" && res.ExitCode == want, fmt.Sprintf("got %d%s, want %d", res.ExitCode, sigSuffix(res.Signal), want))
		assert("output captured completely", !stdout.truncated && !stderr.truncated, fmt.Sprintf("limit %d bytes per stream", max))
		for _, s := range req.Check.Expected.StdoutContains {
			assert("stdout contains "+strconvQuote(s), bytes.Contains(stdout.Bytes(), []byte(s)), "")
		}
		if re := req.Check.Expected.StdoutRegex; re != "" {
			rx, err := regexp.Compile(re)
			assert("stdout matches "+strconvQuote(re), err == nil && rx.Match(stdout.Bytes()), errString(err))
		}
		if status == contract.CheckPassed && len(reasons) > 0 {
			status = contract.CheckFailed
		}
	}

	ev := contract.CheckEvidence{
		SchemaVersion: contract.SchemaVersion, Key: key, Argv: argv, Cwd: req.Check.Cwd,
		ToolVersion: v.ToolVersion, EnvNames: envNames(env), StartedAt: started.UTC().Format(time.RFC3339Nano),
		StdoutRef: "checks/" + id + "/stdout", StdoutSHA256: digestBytes(stdout.Bytes()),
		StderrRef: "checks/" + id + "/stderr", StderrSHA256: digestBytes(stderr.Bytes()), Assertions: asserts,
		SourcesUnchanged: runErr == nil && sealErr == nil && len(changed) == 0, Status: status,
	}
	if runErr == nil && !res.Finished.IsZero() {
		f := finished.UTC().Format(time.RFC3339Nano)
		ev.FinishedAt = &f
		if res.Signal == "" && !res.TimedOut {
			code := res.ExitCode
			ev.ExitCode = &code
		}
	}
	if ev.Assertions == nil {
		ev.Assertions = []contract.Assertion{}
	}
	body, err := json.MarshalIndent(ev, "", " ")
	if err != nil {
		return nil, err
	}
	if err := contract.ValidateRecord(contract.RecordCheck, body); err != nil {
		return nil, fmt.Errorf("evidence does not satisfy its schema: %w", err)
	}
	if _, err := v.Store.WriteArtifact(ev.StdoutRef, stdout.Bytes(), 0o600); err != nil {
		return nil, fmt.Errorf("store stdout: %w", err)
	}
	if _, err := v.Store.WriteArtifact(ev.StderrRef, stderr.Bytes(), 0o600); err != nil {
		return nil, fmt.Errorf("store stderr: %w", err)
	}
	ref := "checks/" + id + "/evidence.json"
	d, err := v.Store.WriteArtifact(ref, append(body, '\n'), 0o600)
	if err != nil {
		return nil, fmt.Errorf("store evidence: %w", err)
	}
	if _, err := v.Store.Append("check.recorded", map[string]any{
		"attempt_id": id, "check_id": req.Check.ID, "status": status, "evidence_ref": ref, "evidence_sha256": d,
		"candidate": req.Candidate.Commit, "sealed_source": sealed.SourceRoot, "sealed_scratch": sealed.ScratchRoot,
	}); err != nil {
		return nil, err
	}
	return &Result{AttemptID: id, Evidence: ev, EvidenceRef: ref, EvidenceDigest: d, Reasons: reasons, Sealed: sealed}, nil
}

func (v *Verifier) attemptID(now time.Time) string {
	if v.newID != nil {
		return v.newID()
	}
	var b [4]byte
	rand.Read(b[:])
	return "check-" + now.UTC().Format("20060102T150405.000000") + "-" + hex.EncodeToString(b[:])
}

// resolve makes argv[0] absolute: an absolute path is kept, a bare name is
// looked up only in the toolchains' bin directories, never in PATH.
func (v *Verifier) resolve(argv []string) ([]string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("empty check argv")
	}
	out := append([]string(nil), argv...)
	if filepath.IsAbs(argv[0]) {
		return out, nil
	}
	if strings.ContainsRune(argv[0], '/') {
		return nil, fmt.Errorf("check command %q must be a bare name or an absolute path", argv[0])
	}
	for _, tc := range v.Toolchains {
		p := filepath.Join(tc, "bin", argv[0])
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			out[0] = p
			return out, nil
		}
	}
	return nil, fmt.Errorf("check command %q is not in a declared toolchain", argv[0])
}

// within resolves a repository-relative cwd inside root.
func within(root, rel string) (string, error) {
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("check cwd %q must be relative", rel)
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	if r, err := filepath.Rel(root, p); err != nil || r == ".." || strings.HasPrefix(r, "../") {
		return "", fmt.Errorf("check cwd %q leaves the repository", rel)
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("check cwd %q is not a directory of the candidate", rel)
	}
	return p, nil
}

// environmentDigest describes the check environment without the per-attempt
// root paths, so equal environments get equal digests.
func environmentDigest(env []string, p sandbox.Policy, toolchains, readOnly []string, toolVersion, osVer, osBuild string) string {
	var norm []string
	for _, kv := range env {
		kv = strings.ReplaceAll(kv, p.SourceRoot, "$SOURCE")
		norm = append(norm, strings.ReplaceAll(kv, p.ScratchRoot, "$SCRATCH"))
	}
	profile := strings.ReplaceAll(strings.ReplaceAll(sandbox.Render(p), p.SourceRoot, "$SOURCE"), p.ScratchRoot, "$SCRATCH")
	return digestJSON(map[string]any{"env": norm, "toolchains": toolchains, "read_only": readOnly, "tool": toolVersion,
		"os": osVer + " " + osBuild, "backend": sandbox.BackendSeatbelt, "profile": sandbox.Digest(profile)})
}

func envNames(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func digestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func digestJSON(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// capped keeps at most max bytes and remembers whether it dropped any.
type capped struct {
	bytes.Buffer
	max       int64
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.max - int64(c.Len())
	switch {
	case room <= 0:
		c.truncated = c.truncated || len(p) > 0
	case int64(len(p)) > room:
		c.Buffer.Write(p[:room])
		c.truncated = true
	default:
		c.Buffer.Write(p)
	}
	return len(p), nil
}

func sigSuffix(sig string) string {
	if sig == "" {
		return ""
	}
	return " (signal " + sig + ")"
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
