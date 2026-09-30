# Niten Contracts

Status: implemented in `internal/contract`, 2026-09-30. The package holds the Go
types, the embedded JSON Schemas (draft 2020-12, `additionalProperties: false`
everywhere), the validation API and offline fixtures for the messages and records
described in the [architecture](architecture.md) and the
[Shogun contract](shogun-contract.md). Where those documents left a shape open,
the choice is recorded here so the documents and the code stay aligned.

## What is embedded

| Schema | Owner | Purpose |
|---|---|---|
| `envelope` | coordinator | Wraps every model message with `run_id`, `sequence`, `message_id`, `reply_to`, `attempt_id`, `step_ids`, `candidate_id`, `from`, `to`, `kind`, `payload` |
| `candidate_ready` | executor | Steps, description, claimed changed paths, off-target justifications, proposed checks, questions, handoff |
| `finding` | reviewer | Stable ID, severity, criteria, location, defect scenario, expected fix, evidence refs |
| `response` | executor | `fixed` or `disputed` for one finding, with explanation and evidence refs |
| `check_request` | reviewer | A proposed additional check with reason and criteria |
| `question` | either model | A missing fact or a decision the user has to make |
| `review_result` | reviewer | `approve`, `revise` or `blocked`, coverage, finding dispositions, summary |
| `candidate_record` | coordinator | Candidate identity, parent, actual changed paths, off-target changes, hard-policy violations |
| `check_evidence` | coordinator | Evidence key plus argv, cwd, tool version, env names, timing, exit code, output refs, assertions, `sources_unchanged`, status |
| `attestation` | user, stored by coordinator | Human evidence for a pre-assigned criterion at an exact candidate SHA |
| `step_continue` | user | The `--gate-per-step` answer bound to gate, run, digests, step and SHA |
| `handoff_record` | coordinator | The executor handoff with `attempt_id` and `verified=false` |
| `common` | shared `$defs` | IDs, SHAs, digests, timestamps, refs |

The API is `ValidateEnvelope`, `ValidatePayload(kind, …)`, `ValidateRecord(name, …)`,
`ValidateValue(name, v)`, `Schemas()` and `Raw(name)`. Validation errors carry JSON
pointers to the failing fields. Enumerations for roles, message kinds, run and step
states, exit codes, execution owners, verification methods, severities, verdicts and
dispositions are Go constants with `Valid()` methods. Exit codes match the
architecture: 0, 1, 2, 3, 4, 5 and 130.

## Shape decisions fixed by the implementation

- Model payloads cannot carry coordinator fields. `from`, `to`, `sender`,
  `attempt_id`, `sequence`, `accepted`, `accepted_at`, `message_id`, `candidate_id`
  and a step `status` are rejected in every payload kind; `verdict` is accepted only
  inside `review_result`.
- Every model message is addressed to the coordinator: `to` must be `coordinator`.
  The sender is fixed per kind: `candidate_ready` and `response` come from the
  executor, `finding`, `check_request` and `review_result` from the reviewer,
  `question` from either model. A user-role sender on a model kind is rejected.
- `candidate_ready.steps` must be a subset of the envelope's `step_ids`; the model
  cannot claim a step it was not assigned.
- `candidate_id` is `{commit, tree, plan_digest, generation}`; SHAs are 40 or 64 hex
  characters, digests 64 hex, `generation` an integer of at least 1. Timestamps are
  RFC 3339 strings.
- A proposed check is `{id, method, argv?, cwd?, timeout?, expected{exit_code?,
  stdout_contains?, stdout_regex?}, criterion_ids, verification_ids}`. `argv` is
  required for `test` and `command`; verification IDs are scoped, `S-001/V-001`.
- `handoff` holds `implementation_notes`, `remaining_work` and `risks` as string
  arrays, plus unique `decision_refs` and `evidence_refs`.
- Finding IDs match `F-NNN`; a location is `{path, line_start?, line_end?}`.
- Off-target vocabulary is split: the reviewer's verdict is `accept` or `reject`,
  the coordinator's disposition is `pending`, `accepted` or `rejected`.
- `check_evidence` nests its key under `key{…}`; `finished_at` and `exit_code` are
  nullable; `status: passed` requires `sources_unchanged: true`, an integer exit
  code, a non-null `finished_at` and every assertion with `passed: true`. A record
  that claims `passed` while one assertion failed is rejected at validation
  (review finding, fixed 2026-09-30).
- `attestation` is the stored record: the coordinator fields `submitted_at` and
  `source: "resume_answers"` are required. A separate user-input schema does not
  exist yet and can be derived when `resume --answers` is implemented.
- All records carry `schema_version: 1`.

## Dependencies

The validator is `github.com/santhosh-tekuri/jsonschema/v6`, the same library and
version Shogun uses, so the module cache already had it and no network was needed.
