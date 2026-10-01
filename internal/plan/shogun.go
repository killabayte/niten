// Package plan imports approved Shogun plans. It reads the published triplet (plan,
// approval receipt, manifest sidecar), recognizes the renderer's Markdown grammar
// without guessing, checks references, coverage and the dependency graph, and builds
// the normalized execution contract that later stages execute.
//
// The package never executes plan text and never calls a model. The authoritative
// integrity check of the approved body and immutable metadata is `shogun verify`,
// run by the caller; the checks here are an independent second line: the body digest,
// the receipt shape, and the manifest fingerprint recomputed with Shogun's algorithm.
package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Shogun's approved-body markers. Exactly one pair, each on its own line.
const (
	MarkerBegin = "<!-- shogun:plan:begin -->"
	MarkerEnd   = "<!-- shogun:plan:end -->"
)

// ManifestVersion is the only Shogun manifest version this importer understands.
const ManifestVersion = 1

// EmptySHA256 is the digest of no bytes: Shogun's diff and untracked digests of a clean repository.
const EmptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var gitSHA = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// ErrFormat marks input that is not a supported Shogun publication.
var ErrFormat = errors.New("unsupported plan format")

// ErrIntegrity marks input whose bytes disagree with the receipt that approved them.
var ErrIntegrity = errors.New("plan integrity check failed")

// Split is a plan file cut at its approved-body markers.
type Split struct {
	Body []byte // exact bytes between the marker lines, as hashed by Shogun
	// BodyLine is the 1-based line number in the plan file where Body starts; it turns
	// body-relative line numbers into file line numbers for diagnostics only.
	BodyLine   int
	BodySHA256 string
}

// SplitBody applies Shogun's marker rules: a leading "---" frontmatter block, then only
// whitespace, then exactly one begin marker line and one end marker line after it. The
// frontmatter itself is not decoded here; its immutable part is bound by the receipt and
// checked by `shogun verify`.
func SplitBody(data []byte) (*Split, error) {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil, fmt.Errorf("%w: missing YAML frontmatter", ErrFormat)
	}
	rest := data[4:]
	off := 4
	if idx := bytes.Index(rest, []byte("\n---\n")); idx >= 0 {
		off += idx + 5
		rest = rest[idx+5:]
	} else {
		return nil, fmt.Errorf("%w: unterminated frontmatter", ErrFormat)
	}
	begins, ends := markerLines(rest, MarkerBegin), markerLines(rest, MarkerEnd)
	if len(begins) != 1 || len(ends) != 1 {
		return nil, fmt.Errorf("%w: expected exactly one begin and one end marker, found %d/%d", ErrFormat, len(begins), len(ends))
	}
	if begins[0][0] >= ends[0][0] {
		return nil, fmt.Errorf("%w: end marker precedes begin marker", ErrFormat)
	}
	if strings.TrimSpace(string(rest[:begins[0][0]])) != "" {
		return nil, fmt.Errorf("%w: only frontmatter and whitespace are allowed before the begin marker", ErrFormat)
	}
	body := rest[begins[0][1]:ends[0][0]]
	sum := sha256.Sum256(body)
	start := off + begins[0][1]
	return &Split{Body: body, BodyLine: bytes.Count(data[:start], []byte("\n")) + 1, BodySHA256: hex.EncodeToString(sum[:])}, nil
}

// markerLines returns [start, next) of every line whose trimmed text equals marker.
func markerLines(data []byte, marker string) [][2]int {
	var out [][2]int
	for off := 0; off < len(data); {
		end, next := len(data), len(data)
		if nl := bytes.IndexByte(data[off:], '\n'); nl >= 0 {
			end, next = off+nl, off+nl+1
		}
		if strings.TrimSpace(string(data[off:end])) == marker {
			out = append(out, [2]int{off, next})
		}
		off = next
	}
	return out
}

// Receipt is Shogun's <stem>.approval.json, schema version 1.
type Receipt struct {
	SchemaVersion        int               `json:"schema_version"`
	PlanID               string            `json:"plan_id"`
	Revision             int               `json:"revision"`
	BodySHA256           string            `json:"body_sha256"`
	ImmutableMetadata    map[string]any    `json:"immutable_metadata"`
	ManifestDigest       string            `json:"manifest_digest"`
	RequirementsRevision string            `json:"requirements_revision"`
	ReviewID             string            `json:"review_id"`
	CLIVersions          map[string]string `json:"cli_versions"`
	Requested            map[string]string `json:"requested"`
	Reported             map[string]string `json:"reported"`
	ApprovedAt           string            `json:"approved_at"`
}

// Metadata is the part of the immutable frontmatter the importer relies on, read from
// the receipt's canonical form (the receipt is what `shogun verify` compares).
type Metadata struct {
	Title    string   `json:"title"`
	PlanID   string   `json:"plan_id"`
	Revision int      `json:"revision"`
	Project  string   `json:"project"`
	RunID    string   `json:"run_id"`
	Created  string   `json:"created"`
	Repos    []string `json:"repos"`
	Planner  string   `json:"planner"`
	Reviewer string   `json:"reviewer"`
}

// DecodeReceipt parses a receipt strictly: unknown fields, a wrong schema version or a
// malformed digest are format errors.
func DecodeReceipt(data []byte) (*Receipt, *Metadata, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var r Receipt
	if err := dec.Decode(&r); err != nil {
		return nil, nil, fmt.Errorf("%w: receipt: %v", ErrFormat, err)
	}
	if dec.More() {
		return nil, nil, fmt.Errorf("%w: receipt: trailing data after the JSON document", ErrFormat)
	}
	switch {
	case r.SchemaVersion != 1:
		return nil, nil, fmt.Errorf("%w: receipt schema_version %d is not supported", ErrFormat, r.SchemaVersion)
	case r.PlanID == "":
		return nil, nil, fmt.Errorf("%w: receipt has no plan_id", ErrFormat)
	case r.Revision < 1:
		return nil, nil, fmt.Errorf("%w: receipt revision %d is not positive", ErrFormat, r.Revision)
	case !hex64.MatchString(r.BodySHA256):
		return nil, nil, fmt.Errorf("%w: receipt body_sha256 is not a SHA-256 digest", ErrFormat)
	case r.ManifestDigest != "" && !hex64.MatchString(r.ManifestDigest):
		return nil, nil, fmt.Errorf("%w: receipt manifest_digest is not a SHA-256 digest", ErrFormat)
	case r.ImmutableMetadata == nil:
		return nil, nil, fmt.Errorf("%w: receipt has no immutable_metadata", ErrFormat)
	}
	md, err := metadata(r.ImmutableMetadata)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: receipt immutable_metadata: %v", ErrFormat, err)
	}
	if md.PlanID != r.PlanID {
		return nil, nil, fmt.Errorf("%w: receipt plan_id %q differs from its metadata plan_id %q", ErrIntegrity, r.PlanID, md.PlanID)
	}
	if md.Revision != r.Revision {
		return nil, nil, fmt.Errorf("%w: receipt revision %d differs from its metadata revision %d", ErrIntegrity, r.Revision, md.Revision)
	}
	return &r, md, nil
}

// metadata reads Shogun's tagged canonical values ({"t":"str","v":…}, {"t":"int","v":"1"},
// {"t":"list","v":[…]}). Required keys must be present with the expected type.
func metadata(m map[string]any) (*Metadata, error) {
	str := func(key string, required bool) (string, error) {
		v, ok := m[key]
		if !ok {
			if required {
				return "", fmt.Errorf("key %q is missing", key)
			}
			return "", nil
		}
		t, ok := v.(map[string]any)
		if !ok || t["t"] != "str" {
			return "", fmt.Errorf("key %q is not a canonical string", key)
		}
		s, ok := t["v"].(string)
		if !ok {
			return "", fmt.Errorf("key %q is not a canonical string", key)
		}
		return s, nil
	}
	var md Metadata
	var err error
	for _, f := range []struct {
		key      string
		dst      *string
		required bool
	}{{"title", &md.Title, true}, {"plan_id", &md.PlanID, true}, {"project", &md.Project, true},
		{"run_id", &md.RunID, false}, {"created", &md.Created, false}, {"planner", &md.Planner, false}, {"reviewer", &md.Reviewer, false}} {
		if *f.dst, err = str(f.key, f.required); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(md.Title) == "" || strings.TrimSpace(md.PlanID) == "" || strings.TrimSpace(md.Project) == "" {
		return nil, errors.New("title, plan_id and project must be non-empty")
	}
	rev, ok := m["revision"].(map[string]any)
	if !ok || rev["t"] != "int" {
		return nil, errors.New(`key "revision" is not a canonical integer`)
	}
	rs, _ := rev["v"].(string)
	n, err := strconv.Atoi(rs)
	if err != nil || n < 1 {
		return nil, fmt.Errorf("revision %q is not a positive integer", rs)
	}
	md.Revision = n
	if v, ok := m["repos"]; ok {
		l, ok := v.(map[string]any)
		items, isList := l["v"].([]any)
		if !ok || l["t"] != "list" || !isList {
			return nil, errors.New(`key "repos" is not a canonical list`)
		}
		for _, it := range items {
			t, ok := it.(map[string]any)
			s, isStr := t["v"].(string)
			if !ok || t["t"] != "str" || !isStr {
				return nil, errors.New(`key "repos" holds a non-string item`)
			}
			md.Repos = append(md.Repos, s)
		}
	}
	return &md, nil
}

// ManifestRepo is one repository entry of a Shogun manifest.
type ManifestRepo struct {
	ID              string `json:"id"`
	Root            string `json:"root"`
	IsGit           bool   `json:"is_git"`
	Head            string `json:"head,omitempty"`
	DiffSHA256      string `json:"diff_sha256,omitempty"`
	UntrackedSHA256 string `json:"untracked_sha256,omitempty"`
	InventorySHA256 string `json:"inventory_sha256,omitempty"`
	Fingerprint     string `json:"fingerprint"`
	Note            string `json:"note,omitempty"`
}

// ManifestInput is one explicit planning input of a Shogun manifest.
type ManifestInput struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Origin      string `json:"origin"`
	StoredPath  string `json:"stored_path,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Size        int64  `json:"size,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	FinalURL    string `json:"final_url,omitempty"`
	FetchedAt   string `json:"fetched_at,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	Role        string `json:"role,omitempty"`
}

// Manifest is Shogun's input manifest (manifest.json in a run, <stem>.manifest.json when published).
// Root, Workspace, Origin and Exclude are locators: the digest does not bind all of them, and
// they never grant anything.
type Manifest struct {
	Version     int             `json:"version"`
	CreatedAt   string          `json:"created_at"`
	Workspace   string          `json:"workspace"`
	Repos       []ManifestRepo  `json:"repos"`
	Inputs      []ManifestInput `json:"inputs"`
	Exclude     []string        `json:"exclude,omitempty"`
	Fingerprint string          `json:"fingerprint"`
}

// DecodeManifest parses a manifest strictly and checks its internal shape.
func DecodeManifest(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: manifest: %v", ErrFormat, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: manifest: trailing data after the JSON document", ErrFormat)
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("%w: manifest version %d is not supported (want %d)", ErrFormat, m.Version, ManifestVersion)
	}
	if len(m.Repos) == 0 {
		return nil, fmt.Errorf("%w: manifest lists no repository", ErrFormat)
	}
	seen := map[string]bool{}
	for _, r := range m.Repos {
		if r.ID == "" || seen[r.ID] {
			return nil, fmt.Errorf("%w: manifest repository id %q is empty or repeated", ErrFormat, r.ID)
		}
		seen[r.ID] = true
		if r.IsGit {
			if r.Head != "" && !gitSHA.MatchString(r.Head) {
				return nil, fmt.Errorf("%w: manifest repository %s head %q is not a full commit id", ErrFormat, r.ID, r.Head)
			}
			if !hex64.MatchString(r.DiffSHA256) || !hex64.MatchString(r.UntrackedSHA256) {
				return nil, fmt.Errorf("%w: manifest repository %s lacks its change digests", ErrFormat, r.ID)
			}
		}
		if !hex64.MatchString(r.Fingerprint) {
			return nil, fmt.Errorf("%w: manifest repository %s fingerprint is not a SHA-256 digest", ErrFormat, r.ID)
		}
	}
	for _, in := range m.Inputs {
		if in.ID == "" || seen[in.ID] {
			return nil, fmt.Errorf("%w: manifest input id %q is empty or repeated", ErrFormat, in.ID)
		}
		seen[in.ID] = true
		if in.Status == "ok" && !hex64.MatchString(in.SHA256) {
			return nil, fmt.Errorf("%w: manifest input %s has status ok without a SHA-256 digest", ErrFormat, in.ID)
		}
	}
	if !hex64.MatchString(m.Fingerprint) {
		return nil, fmt.Errorf("%w: manifest fingerprint is not a SHA-256 digest", ErrFormat)
	}
	return &m, nil
}

// ComputeFingerprint is Shogun's repository fingerprint over the recorded digests
// (internal/inputs.Repo.ComputeFingerprint at S0). Golden fixtures pin the compatibility.
func (r ManifestRepo) ComputeFingerprint() string {
	return RepoFingerprint(r.IsGit, r.Head, r.DiffSHA256, r.UntrackedSHA256, r.InventorySHA256)
}

// RepoFingerprint computes Shogun's repository fingerprint from its components.
func RepoFingerprint(isGit bool, head, diff, untracked, inventory string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%v\x00%s\x00%s\x00%s\x00%s", isGit, head, diff, untracked, inventory)
	return hex.EncodeToString(h.Sum(nil))
}

// ComputeFingerprint is Shogun's manifest digest (internal/inputs.Manifest.ComputeFingerprint):
// ordered repository IDs and fingerprints, then input IDs, statuses, hashes and optional roles.
// It is not a hash of the manifest bytes; unbound fields such as roots or exclusions do not enter it.
func (m *Manifest) ComputeFingerprint() string {
	h := sha256.New()
	for _, r := range m.Repos {
		fmt.Fprintf(h, "repo\x00%s\x00%s\n", r.ID, r.Fingerprint)
	}
	for _, s := range m.Inputs {
		fmt.Fprintf(h, "input\x00%s\x00%s\x00%s\n", s.ID, s.Status, s.SHA256)
		if s.Role != "" {
			fmt.Fprintf(h, "role\x00%s\x00%s\n", s.ID, s.Role)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Dirty reports whether Shogun recorded tracked changes or untracked files for a git repository.
func (r ManifestRepo) Dirty() bool {
	return (r.DiffSHA256 != "" && r.DiffSHA256 != EmptySHA256) || (r.UntrackedSHA256 != "" && r.UntrackedSHA256 != EmptySHA256)
}

// CheckManifest binds a manifest to the receipt that approved the plan: every repository
// fingerprint must match its recorded digests, and the recomputed manifest digest must equal
// both the stored fingerprint field and the receipt's manifest_digest.
func CheckManifest(m *Manifest, r *Receipt) error {
	if r.ManifestDigest == "" {
		return fmt.Errorf("%w: the receipt carries no manifest digest, so no manifest can pin the planning base", ErrIntegrity)
	}
	for _, repo := range m.Repos {
		if repo.ComputeFingerprint() != repo.Fingerprint {
			return fmt.Errorf("%w: manifest repository %s fingerprint is inconsistent with its recorded digests", ErrIntegrity, repo.ID)
		}
	}
	got := m.ComputeFingerprint()
	if got != r.ManifestDigest {
		return fmt.Errorf("%w: manifest digest %.12s does not match the receipt's manifest_digest %.12s", ErrIntegrity, got, r.ManifestDigest)
	}
	if got != m.Fingerprint {
		return fmt.Errorf("%w: manifest fingerprint field is inconsistent with its contents", ErrIntegrity)
	}
	return nil
}

// Digest is the lowercase hex SHA-256 of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
