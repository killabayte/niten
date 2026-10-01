# Niten's Contract with Shogun

Status: v0.1 draft, 2026-09-30. Shogun was studied at commit
`8ca1289283689adaf5aece60eb6d1fa7eef7c708`. A scoped
[manifest sidecar publication change](shogun-manifest-sidecar.md) in Shogun is a
prerequisite for portable v0.1 input; it is implemented on the Shogun branch
`s0-manifest-sidecar`. Original run directories are optional. The import described
here is implemented as `niten prepare`; the decisions the implementation fixed are in
[import](import.md).

## What already exists

| Artifact | What it contains | What it does not prove |
|---|---|---|
| `PLAN.md` | Frontmatter, approved body between markers, execution log | That the described implementation exists |
| `PLAN.approval.json` | Body hash, immutable metadata, manifest digest, review ID, models | Author authenticity or implementation correctness |
| `manifest.json` in the run | Full repo HEADs, fingerprints, inputs, source mapping | That the current working copy still matches |
| Proposed `PLAN.manifest.json` | Published manifest snapshot tied to the existing receipt digest | Recovery of missing historical snapshot bytes |
| Structured plan/research/outline/steps | Internal generation data that depends on the Shogun mode | That an arbitrarily chosen JSON matches the published revision |

Facts from the code: `internal/library/plan.go` allows changing `status`, `tags`,
`updated` and the trailer outside the markers. `internal/pipeline/render.go` prints
only the first 12 characters of the repo HEAD/fingerprint into Markdown.
`verification` contains `id`, `repo_id`, `method`, `expected`; there is no mandatory
`command`. The same `V-001` may appear in several steps, so the check key is
`S-001/V-001`. See [sources and links](research.md).

## v0.1 input

```text
niten prepare /plans/feature.md --repo repo-1=/workspace/project
```

The default input is `feature.md`, `feature.approval.json` and
`feature.manifest.json`. Niten validates the manifest against the existing
receipt's `manifest_digest`; no run directory is required. For an older plan,
`--shogun-run RUN_DIR` may supply a preserved manifest as a compatibility source.
If both sources exist, disagreement is an error, not an implicit preference.

Archived planning inputs are not a universal execution prerequisite: the verified
approved body is the implementation contract. If a step explicitly needs an
external specification or file, that is a required execution input and must be
supplied and checked against its recorded digest. Missing required content yields
`needs_input`; Niten does not silently fetch a current URL or invent the missing
specification. A self-contained plan needs no full planning-input archive.

The four locally inspected legacy plans have no manifest sidecar and the checked
run stores are empty. A digest cannot reconstruct a lost manifest. They remain
verifiable Shogun plans but cannot be executed strictly by Niten until a matching
manifest is recovered or a new plan is approved after the publication change.
New publication does not backfill history.

The repo ID to path binding is explicit or proposed from manifest locators and
shown at prepare. Paths are not repository identities or permission grants:
full commits, fingerprints and remaps are checked. Local paths in the manifest
are not copied into public reports without need.

## Input validation

1. Copy the inputs into a private staging run, check sizes, the absence of symlink
   escapes and the correct receipt/manifest format. Work with these bytes from then on.
2. Invoke a compatible `shogun verify` on the plan copy and its sidecar. This is an
   offline operation. Save the binary version, exit code and result. On `changed`,
   `unverifiable` or `invalid_format`, do not start the models.
3. Check `plan_id`, revision and schema versions. Recompute the manifest fingerprint
   with Shogun's algorithm and compare it with `approval.manifest_digest`. This is
   **not** the SHA-256 of the `manifest.json` bytes: the algorithm hashes repo
   fingerprints, input IDs/status/hash and the optional role in order. Additionally
   save the hash of the manifest bytes for our own input immutability.
4. Check the internal consistency of the repo fingerprint with the full HEAD and the
   change hashes, then take the current repo fingerprint again and compare. Input
   metadata participates in the manifest digest without requiring all archived
   input bytes. Separately verify required execution inputs by full hashes.
   Root/origin/exclude fields are locators, not authority: the legacy digest does
   not bind every manifest field. Validate/remap them explicitly.
5. Check that the source writable repo is clean and that Shogun planned against a
   clean repo. Only explicitly verified own plan/receipt/manifest files and service artifacts
   by adapter rules are excluded from the comparison, not arbitrary paths from an
   unverified `exclude`. Any other drift stops prepare.
6. Parse the supported Markdown and check references, coverage, DAG, targets,
   repo IDs, the absence of duplicate step/criterion IDs and path traversal.
7. Save the execution contract and the effective config. Neither prepare nor import
   modifies the approved body or the original execution log.

The Shogun receipt is an integrity control, not a digital signature. A combination
of a forged plan and a forged receipt is not authenticated by this protocol. The
check is designed for the user's local inputs and protection against accidental
drift, not as proof of trust in an arbitrary sender.

To start with, we use the `shogun verify` subprocess, do not copy its YAML
canonicalizer and do not try to import a foreign `internal` Go package. Fingerprint
verification is implemented as a small compatible adapter with golden fixtures;
extracting a shared public package from Shogun is not required yet.

## Plan parsing

The source of meaning is the verified approved body. We support the structural
sections of the current renderer: Requirements, Steps, End-to-end verification,
Traceability, and keep the remaining approved sections in full as context.
Normalized fields always carry references to the source spans and text hashes.

We do not build an arbitrary NLP parser and do not ask a model to "rewrite the plan
into our format". The importer recognizes the current renderer grammar and rejects
ambiguity. Special attention goes to escaped Markdown cells, text with delimiters,
code fences, repeated IDs and fake section headings inside content. Unrecognized
structure means an incompatible format; ordinary action text is kept without being
interpreted as a command.

Structured JSON from the Shogun run may be used as supporting material, but is not
declared the execution source on the "take the newest file" principle. Fast and
thorough store the plan differently; the receipt does not directly attest an
arbitrary `plan/1.json`. The MVP does not depend on these internal payloads.

If real, correct plans cannot be parsed without loss of meaning, the P1 gate does
not pass. The next decision is a small versioned export in Shogun, tied to the
approved body and receipt, not silent guessing or extending the parser into NLP.

## Normalized execution contract

| Entity | Required data |
|---|---|
| Plan identity | plan ID/revision, approved body hash, immutable metadata, receipt and manifest |
| Repository | repo ID, full base commit, fingerprint, local binding, read/write role |
| Requirement | original ID, type, mandatory, statement and criteria without renaming |
| Step | ID, title, objective, dependencies, criteria, targets, actions, risks, rollback |
| Verification | scoped ID `step/verification`, method, repo ID, expected, source span and execution owner |
| Final criteria | original IDs of the end-to-end criteria and evidence rules |
| Policy | write roots, protected/instruction paths, commands, network, base instructions, limits, models, versions |
| External criteria | mandatory criteria/actions assigned to the user, attestation rules |

All steps of an existing plan are considered required: a Shogun step has no
`optional` field of its own. The set of steps or their dependencies cannot be
changed without a new revision. Targets define the focus of the expected changes.
Within the permitted repo, related additional files are allowed: Niten itself
computes the off-target diff, the executor justifies it, and the reviewer explicitly
accepts or rejects each such edit. This does not weaken the explicit restrictions
of the original plan and does not permit new functionality. A change to a hard
protected path rejects the whole candidate; a change to an instruction path without
an explicit target also rejects the whole candidate. Niten does not assemble a
partial commit from the permitted part of a forbidden result.

Copies of the source repo instructions are taken from the base commit; instructions
written by the executor in the candidate remain review material. The contract
records the list of protected/instruction paths and their digests, not just the
overall repo root.

## From expected result to check

Shogun may describe `V-001 (command): CLI prints the version` without the command
itself. At prepare time this is a valid requirement, but not yet an executable
check. The contract keeps method and expected; the concrete argv/cwd/assertion is a
separate derived artifact, `check_spec`, which must not be passed off as part of
the approved plan.

Opus proposes a check_spec as part of the implementation. Niten checks permissions,
timeout, paths and syntax, runs the permitted check in a copy of the candidate, and
Astra checks whether such a result really confirms the original criterion.
Choosing `go test ./...` in an ordinary Go repo does not require a new product
decision. An unknown success metric, external access or a missing specification
leads to `needs_input`. An unverified check_spec cannot close a criterion.

The final evidence manifest links every mandatory criterion ID to a scoped
verification ID, a check_spec digest, a result and a candidate. Shogun's
traceability shows the possible checks of a step, but does not prove that any of
them is sufficient for any criterion: sufficiency and the real link are confirmed
by Astra.

The `inspect` method produces structured review evidence. `measure` remains in
the imported schema, but its backend is deferred beyond v0.1. A plan requiring it
receives `needs_input: unsupported_method_measure` before model execution; it is
not silently converted to inspect or a human attestation.
We do not demand heavy E2E for a simple edit unless it follows from the plan itself.
At prepare time every criterion/check receives an execution owner, `niten` or
`human`. Shogun does not emit such a field: it is an explicit execution setting
shown to the user before the run. An ambiguous classification requires an answer;
a model cannot rename a failed automated test into a manual criterion.

Human ownership is permissible for deployment, verification in an external
environment and manual acceptance, keeping the original expected. If the local part
passed and only these actions remain, the `implemented` status and the
`pending_external` list preserve the result without claiming that the whole plan is
fulfilled. An unknown specification or an unavailable ordinary automated test still
yields `needs_input`/a failed gate.

Through `resume --answers` the user submits an `attestation`: criterion ID,
plan/contract digests, candidate SHA, result, observation, environment,
observed_at, actor and evidence refs. The coordinator adds submitted_at and the
submission channel. An attestation is accepted only for a pre-assigned human
criterion and the same version of the result; a change of SHA requires a new
confirmation. It is a user-provided attestation, not automatically verified truth.
Mandatory steps with an external part remain `awaiting_external` until confirmed.

The final code review is bound to the local evidence digest. Adding a human
attestation updates the final receipt without paying again for an unchanged code
review; the `done` gate checks both parts. Answer fields coming from a model do not
have these powers.

## Drift and portability

Niten does not perform an automatic rebase onto a new HEAD. When the base does not
match, either a current Shogun revision or a separate recorded decision is needed,
and the latter is not labeled as execution of the previous unchanged contract. In
v0.1 we start a new run with the current plan, keeping the old artifacts.

An already completed result is not spoiled by changes to the source working
directory: it is bound to the saved clone and snapshot. New requirements, the plan's
mutable status and a green `shogun verify` do not automatically re-approve the
implementation.

A future bundle may also include a typed execution payload and the bytes of
required execution inputs. The smaller manifest sidecar is the v0.1 decision;
a general Shogun export command and a shared public Go package are not required.
