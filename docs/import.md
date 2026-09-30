# P1 Import: `niten prepare`

Status: implemented, 2026-09-30. This records what `niten prepare` actually does, so
the [import contract](shogun-contract.md) and the code stay aligned. Where the contract
left a choice open, the decision is stated here. No model is called by anything below.

```text
niten prepare PLAN.md [--repo ID=PATH] [--input ID=PATH]... [--human ID]...
                      [--gate-per-step] [--shogun-run RUN_DIR] [--config FILE] [--json]
```

## Packages

| Package | Responsibility |
|---|---|
| `internal/plan` | Marker split, strict receipt/manifest decoding, Shogun fingerprint ports, the renderer grammar, semantic checks, the execution contract type |
| `internal/workspace` | Read-only inspection of the source repository and inventory of the base commit |
| `internal/config` | Strict TOML configuration, built-in defaults, v0.1 constraints |
| `internal/store` | Private run store, staging and atomic publication of a run |
| `internal/pathglob` | `**` path patterns for protected and instruction paths |
| `internal/prepare` | The intake sequence and its reason codes |

## Intake sequence

1. **Private copies.** The plan, `<stem>.approval.json` and `<stem>.manifest.json` are
   read once, as regular non-symlink files within size limits (plan 4 MiB, receipt 1 MiB,
   manifest 4 MiB), and written read-only into a staging run. Only these bytes are used.
2. **`shogun verify --require-manifest`** runs on the staged copy with PATH, HOME and
   TMPDIR only, a 30-second timeout and the staging directory as cwd. The executable is
   `shogun_command`, resolved, hashed and recorded with its `shogun version` output. Only
   `valid` with exit 0 continues; `changed` is a rejection; anything else, including a
   Shogun without `--require-manifest`, fails closed.
3. **Independent checks** of the same bytes: the approved-body SHA-256 against the
   receipt, a strict receipt decode (unknown fields, schema version, digest shape, plan
   ID and revision agreeing with the immutable metadata), a strict manifest decode, every
   repository fingerprint recomputed from its recorded digests, and the manifest digest
   recomputed with Shogun's algorithm against both the receipt and the stored field.
4. **The approved body** is parsed with the grammar below and validated: unique
   registry, criteria owned by their requirement, unique steps, resolvable and acyclic
   dependencies, every step citing a criterion, every mandatory criterion assigned,
   unique verification IDs per step, and target paths that stay inside the repository.
   The body title must equal the approved title. "Inputs and versions" must agree with
   the manifest: short heads, clean/dirty state and input digests.
5. **v0.1 scope.** Exactly one repository, a git repository with a commit, planned
   clean. Several repositories, a non-git directory, an unborn HEAD or a dirty planning
   base are `unsupported_mode`, never guessed around.
6. **Binding and drift.** `--repo repo-1=PATH` binds the checkout; without it the
   manifest root is used and reported as a note. The checkout must be the work-tree top
   level. Its fingerprint is recomputed with the port of Shogun's intake: HEAD,
   `git diff --binary` against HEAD and the digest of non-ignored untracked files. Left
   out are only the plan triplet actually given to prepare and anything under `.git` or
   `.shogun`; the manifest's own `exclude` list is not honored. Any drift is a rejection;
   Niten does not rebase. The base commit is then inventoried: submodules and Git LFS
   attributes are unsupported, instruction files are copied from the commit (never the
   working tree), protected entries are recorded.
7. **References and owners.** Targets must name the repository. A protected path as a
   non-inspect target is refused. An instruction path as a target is allowed and
   reported. Verifications may name the repository, no repository, or a manifest input.
   Every criterion and verification is owned by `niten` unless the user passes
   `--human ID` before the run.
8. **Execution inputs** (see below), then the contract, `config.json` and `state.json`
   are written and the staging directory is renamed to `runs/<run-id>`.

A refusal at any step removes the staging directory; no partial run is left.

## Grammar

The grammar is Shogun's `internal/pipeline/render.go` at S0, named
`shogun-render-s0` in every contract. The parser recognizes it exactly and rejects
anything else with the section and line:

- The body starts with `# <title>`, then the twelve `## ` sections in fixed order, each
  exactly once; "Review notes" is the only optional one. A second occurrence, an
  unknown `## ` heading or a heading out of order is refused.
- Structured sections are parsed line by line: the requirement lists of "Goal and
  success criteria" and "Scope and non-goals", "Inputs and versions", the Requirements
  table, "Decisions and assumptions", "Steps", "End-to-end verification" and the
  Traceability table. A line that fits no rule of its section is refused, so
  multi-line model text in these sections is refused rather than joined.
- Free-text sections (Context, Approach, Review notes, Review history) are kept by span
  without interpretation. They may not contain heading-shaped lines (an ATX heading,
  indented by up to three spaces); `#hashtag` without a space is ordinary text.
- Table cells are split at unescaped pipes. Shogun escapes every content pipe as `\|`
  and never escapes backslashes, and its delimiters always have a space before the pipe,
  so this is unambiguous: a cell rendered `c\\|d` is the text `c\|d`.
- The lists and tables that render the same data are cross-checked: IDs, types,
  statements and criteria (compared with whitespace normalized, since cells replace
  newlines with spaces), and the Traceability table must be exactly what the parsed
  steps and end-to-end list produce. A step or verification forged inside another field
  cannot also appear in that table, so it shows up as a mismatch.

All four archived plans in the local library parse and validate with this grammar,
including a fact quote spanning several lines in Context. That is a sample, not a
proof; `NITEN_PLAN_ARCHIVE=<dir> go test ./internal/plan -run TestArchivePlansParse`
repeats the check against any library.

## Decisions fixed by the implementation

- **Required execution inputs.** Shogun rejects a target whose repository is not a
  planned repository, and records no other link between a step and an input. An input
  is therefore required when a verification (or, in a future renderer, an inspect
  target) names its ID. It must be supplied with `--input in-N=PATH`, or is taken from
  the `--shogun-run` archive, and its bytes must match the recorded SHA-256. Missing or
  mismatching content is `needs_input` (exit 3). Other planning inputs need nothing;
  they are listed as planning-only. An input that failed at planning time and is
  required cannot be satisfied and asks for a new plan revision.
- **Human ownership** is explicit and set before the run: `--human R-001.C1` or
  `--human S-001/V-001`. Nothing is classified as human automatically.
- **`measure`** stays in the contract as the method. A verification with it owned by
  `niten` is `needs_input: unsupported_method_measure`. The user may instead assign that
  verification to a human; the method is not changed to another one.
- **Legacy runs.** `--shogun-run RUN_DIR` supplies `RUN_DIR/manifest.json` when the
  sidecar is missing; it is staged as the sidecar, so Shogun verifies it the same way.
  When both exist they must be byte-identical.
- **Configuration.** Unknown keys are errors. The built-in protected paths, instruction
  paths and stripped environment names are always included; a config file can add to
  them only. `max_ahead_steps` must be 0 and `policy.tool_network` must be `deny`.
- **Git environment.** Inspection removes every `GIT_*` variable except the global and
  system config selectors, disables the file-system monitor and otherwise runs the same
  commands with the same output settings as Shogun, so fingerprints agree on the same
  machine.

## Exit codes and reasons

| Exit | Reasons |
|---|---|
| 0 | the run is prepared |
| 1 | `plan_changed`, `manifest_mismatch`, `manifest_disagreement`, `repository_drift` |
| 2 | `invalid_arguments`, `input_file_rejected`, `manifest_missing`, `shogun_verify_failed`, `invalid_format`, `plan_contract_invalid`, `unsupported_mode`, `repository_rejected`, `unknown_reference`, `protected_target`, `store_error` |
| 3 | `needs_input` with `required_input_missing`, `required_input_mismatch`, `required_input_unavailable_at_planning` or `unsupported_method_measure` details |
| 130 | interrupted |

`--json` prints `{"status": "prepared", "run_id", "run_dir", ...}` or
`{"status": "refused" | "needs_input", "reason", "details", "exit_code"}`.

## Run layout after prepare

```text
<store_dir>/runs/<run-id>/
  inputs/plan.md, plan.approval.json, plan.manifest.json   read-only staged copies
  inputs/execution/<input-id>                               supplied execution inputs
  inputs/instructions/<path>                                instruction files of the base commit
  contract.json                                             the execution contract
  config.json                                               effective configuration and its source
  state.json                                                state "prepared" and the contract digest
```

The store and every run directory are private to the owner; the store may not contain
the repository or lie inside it. The lock, the event journal and recovery are P2.

`contract.json` carries the plan identity, `plan_digest` (body, receipt and manifest
bytes), `semantics_digest`, the normalized document with source spans and hashes, the
step order, the repository binding with its base commit, tree, fingerprint and base
inventory, the input records, the `shogun verify` record, criterion and check plans
with their owners, target classifications, the policy, the initial limits, the
requested models and the step-gate setting. Every `check_spec` is `null`: the concrete
argv is derived later and is never part of the approved plan.

`semantics_digest` hashes what the plan asks to be executed without source positions,
plan identity, the inputs list or the review history. The fast and thorough Shogun
pipelines give the same digest for the same approved content, which the fixtures test.
Body-relative spans keep the digest and the requirement identity stable when the
mutable frontmatter or the execution log change.

## Fixtures and tests

`internal/plan/testdata/shogun/` holds fourteen triplets published by Shogun's own
intake, renderer and publication code at S0, against a byte-reproducible fixture
repository (`internal/testutil.FixtureRepo`). The models were replaced by Shogun's
recorded replay data or a scripted planner; `generator.go.txt` regenerates them. They
cover the recorded live stats plans, a reference-only input, the same content through
the fast and thorough pipelines, awkward but valid content, a required input, measure,
a protected target, path traversal, multi-line text, forged headings and steps, and
dirty, multi-repository and non-git planning bases.

The prepare tests use a scripted `shogun`. `NITEN_TEST_SHOGUN=/path/to/shogun go test
./internal/prepare` runs the same scenarios against a real Shogun build; on 2026-09-30
they passed against the `s0-manifest-sidecar` branch at `29d03b3`.
