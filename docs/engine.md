# P3 Engine: the Sequential Executor

Status: implemented offline, 2026-10-01. This records what `internal/engine` and the
`run`, `status` and `resume` commands do, so the [architecture](architecture.md) and the
code stay aligned. Nothing here calls a live model: every scenario runs on scripted
`claude` and `codex` stand-ins (`internal/testutil/fakecli.go`) and the real verifier
sandbox. Live runs need the separately authorized pilot.

## Commands

```text
niten run RUN_ID [--config FILE] [--json]
niten resume RUN_ID [--answers FILE] [--max-invocations N] [--max-time DURATION] [--max-repairs N] [--config FILE] [--json]
niten status RUN_ID [--config FILE] [--json]
```

`run` starts a prepared run. `resume` continues a paused or waiting run after
recovering its attempts. `status` reads the saved projection without the run lock and
never calls a model. Progress lines go to stderr and the result to stdout. SIGINT and
SIGTERM stop the current model call and pause the run with exit 130. Exit codes: 0 only
for `done`, 1 for a failed final gate or a failed attestation, 2 for configuration,
integrity and protocol errors, 3 for needs input, 4 for paused, 5 for implemented with
pending external criteria.

## The loop

Every step goes through the same states, and the final stage (unit `final`) reuses them:

| State | Work |
|---|---|
| `pending` | The final reserve is checked, then the step starts at the current head |
| `implementing` | One executor turn: `implement` for a new step, `repair` after changes were requested |
| `candidate` | The coordinator runs the unit's checks on the candidate in the verifier sandbox |
| `reviewing` | One reviewer turn bound to the candidate and the evidence digest of its checks |
| `changes_requested` | A repair cycle starts, or the run needs input when the repair budget is spent |
| `accepted` | The step is accepted at its candidate; a step gate may hold the run |

The final stage starts on the last accepted candidate. It runs every required check and
every unit's checks on the whole change, then a separate final review of the full diff
from the base against every criterion. A final repair is an executor turn like any
other. The last ordinary review is never reused as the final one, and final checks
always run fresh. That is permitted by the architecture and keeps the first version
simple; reuse by matching evidence keys can come later.

## Journal and recovery

The engine is the only writer of the run's state. Corrupt or tampered run files fail the
run for good; any other engine error (a git or file system failure) pauses it with
`engine_error`, so it can be resumed after the cause is fixed. Each unit change travels in one event
(`turn.processed`, `checks.evaluated`, `final.started`, `step.state`,
`worktree.restored`) that carries the whole transition: candidate, messages, ledger
updates, checks, acceptance and the next step state. One reducer folds the journal,
live and on recovery, and `state.json` is a copy for readers. The store's append
observer feeds the reducer every event, including `attempt.*` from the attempt protocol
and `check.recorded` from the verifier, so model invocations are counted as they start.

Processing is idempotent. Artifacts are written write-once or reused when identical.
Candidate commits are deterministic: the snapshot, the parent, the turn id and the
turn's start time give the same commit. A crash between moving the clone's branch and
recording the candidate is recognized at the next start: an unprocessed executor turn,
the journal's head as parent and the turn's commit subject. On `resume`, attempts are
recovered first (see [runner](runner.md)) and every started turn without an outcome is
resolved without calling the model again:

- **A saved result** is processed now.
- **An attempt that provably never started** lets the turn run anew.
- **An unknown outcome** keeps the executor's worktree as a rejected snapshot, restores
  the worktree and asks the user. The next `resume` is that decision.
- **Processes that still hold an attempt** pause the run without touching them.

## Turns and what is trusted

A turn is one model invocation: the attempt id is the turn id (`t003-s-001-repair`).
Its structured output is one document. `executor_turn` holds the candidate announcement,
responses to findings and blocking questions. `reviewer_turn` holds the reviewed commit,
the review result, findings, check requests, test assessments and questions. The
coordinator splits the document into envelopes of the [contract](contracts.md) message
kinds and sets every envelope field except the payload.

Nothing a model writes is evidence until the coordinator has resolved it.

- **Executor claims.** A claimed changed path must be in the coordinator's diff of the
  unit. An off-target explanation must name an off-target change. Check proposals may
  name only the unit's criteria and verifications. A response must answer an open
  finding of the unit. Evidence refs must resolve to a coordinator ref shown in the
  packet, a recorded message, or a path of the candidate or its diff (`path`,
  `path:12`, `path:3-9`). Handoff decision refs must be plan decisions or user answers.
  An invented reference makes the turn invalid: the worktree is kept as a rejected
  snapshot and restored, nothing of the turn applies, and the run pauses with
  `invalid_result`.
- **Reviewer claims.** `reviewed_commit` must be the commit the attempt was bound to.
  Coverage paths, finding locations and test assessments must be paths of the candidate
  or its diff. Off-target dispositions must name off-target changes. Evidence refs
  follow the executor rules plus `evidence/<name>`: regular files the reviewer left in
  its evidence directory, collected without following links and size-limited. An
  approval must list every required criterion (the mandatory, coordinator-owned
  criteria of the unit), every changed path, a disposition for every off-target edit and
  an assessment for every flagged test change. Otherwise it is not a review.
- **Stale results.** A result bound to a candidate or evidence digest the unit has left
  is recorded as stale and applied to nothing.

A step is accepted only when all of the following hold:

- the reviewer approved its candidate and evidence digest;
- the unit's checks passed;
- no blocker or major finding of the unit is open;
- no off-target edit was rejected;
- no flagged test change weakens the tests.

An approval that leaves a major finding open requests changes. A `blocked` verdict
needs input.

## Checks

The check set of a unit is the policy's required checks plus the unit's accepted
proposals, accumulated across turns: a check, once accepted, keeps running and cannot be
withdrawn. A proposal is accepted when its argv equals an allowed command exactly, its
cwd is a clean relative directory, its timeout is at most the command's, and its
expectations compile. Any other proposal is refused and stays with the unit as a problem
for the repair. Every coordinator-owned `test` or `command` verification of the unit
must be covered by an accepted check; a missing one keeps the candidate from review.
Equal specs run once per evaluation and share their evidence. The evidence digest binds
the candidate, every check's evidence digest and status, and the problems; a review is
bound to it. Reviewer check requests are validated the same way and join the unit's
check set from the next evaluation on, at the latest in the final checks. A check whose
status is `unknown` (a sandbox failure) pauses the run instead of blaming the candidate.

## Packets

Prompts are packets built by the coordinator.

- **Role rules come first, from the coordinator only.** The reviewer's rules contain the
  architecture's sentence about material under review verbatim.
- **The executor's packet.** It gets the policy, the base commit's instruction files
  (Claude runs with `--safe-mode`), the approved plan body and the assignment. It also
  gets the previous executor's handoff as unverified notes and, on repair, the open
  findings, the checks that did not pass with their output tails, the coordinator's
  problems and the accepted checks.
- **The reviewer's first packet is blind to the executor's account.** It gets no
  description, handoff, claimed paths or questions. Off-target explanations appear as
  labeled untrusted claims. Coverage requirements, flagged test changes, check results
  and the diff are listed.
- **The reviewer's later packets.** They add the unit's findings with the executor's
  responses.
- **Where the files are.** The reviewer's copy, the diff and the check evidence sit in a
  fresh launcher directory per attempt. The executor works in the owned clone. Nothing
  is written into the clone except by the executor.

The diff is built with attributes from the empty tree and without external diff or text
conversion, so a candidate's `.gitattributes` cannot mark a text file binary to hide it
from the packet.

## Findings, repairs and no progress

The ledger is keyed by finding id. A new id opens a finding of the unit. The same
finding repeated is no change. An existing id with different content is invalid. Only
the reviewer changes a finding's state; executor responses are recorded with it.

Each repair cycle consumes the unit's repair budget (`max_repairs_per_step`, also for
the final stage). When it is spent, the run needs input with a summary of the open
findings and their latest responses; only `resume --max-repairs` buys another cycle. A repair that changes no code and disputes nothing
pauses the run with `no_progress`. A dispute without a code change goes back to the
reviewer for the same candidate.

## Limits

- **Invocations** are the `attempt.started` events. A call is refused when none remain.
- **Active time** accrues between the events of a session. Waiting in `needs_input` or
  `paused` between sessions costs nothing. Time after the last event before a crash is
  not counted.
- **Each attempt's deadline** is the invocation deadline or the remaining active time,
  whichever is shorter.
- **A new step** starts only when at least the final reserve plus two invocations, and
  more than the reserve time, remain.
- **`resume --max-invocations`, `--max-time` and `--max-repairs`** raise a limit. The raise is a
  `limits.raised` event with the previous and new values, the contract is never
  rewritten, and the receipt carries the history.

## Gates, questions and attestations

With `gate_per_step`, an accepted step opens a gate `gate-<step>-<sha12>` and the run
needs input. `resume` without a matching answer records nothing and calls nothing. The
gate is released only by a `step_continue` that matches all of these exactly:

- the gate;
- the run;
- the plan and contract digests;
- the step;
- the accepted SHA.

An answer for an old SHA is refused. Repeating an answer for a released gate is
idempotent. The last step's gate holds the run before the final stage.

A question with `needed_decision` stops the run until the user answers it by message id.
The answers are shown to later sessions as user decisions, and handoffs may cite them.

Criteria owned by a human, and criteria traced to a human-owned verification, end the
autonomous phase as `implemented` with `pending_external` (exit 5). An attestation
through `resume --answers` must be for a pending criterion, the implemented candidate,
the plan and contract digests, and a past observation time. With every pending criterion
attested as passed, the whole final gate runs again on the unchanged candidate and a new
receipt version records `done`, without any model call. A
failed attestation keeps the run implemented (exit 1).

The answers file follows the `answers` document schema. It is stored, named by its
content digest, before its entries apply.

## Final gate and receipt

`done` is recorded only when all of the following hold:

- the contract and the stored plan inputs match their digests;
- every step is accepted;
- the final checks passed on the head candidate;
- the final review approved exactly that candidate and its evidence digest;
- no blocker or major finding is open;
- every turn was processed;
- the clone's branch, tree and worktree equal the head candidate;
- nothing holds the run's work area;
- every mandatory criterion is covered (coordinator-owned) or attested (human-owned);
- every artifact the journal references matches its digest.

Then a receipt is written as an immutable version (`receipts/<n>-<status>.json` and
`.md`), `receipt.saved` is recorded, `execution.json` and `execution.md` are replaced
atomically, and only then the run state becomes `done` or `implemented`. A failing gate
pauses the run with `final_gate` and exit 1.

The receipt holds the plan identity and digests, the base and final commits, per-step
acceptance, per-criterion status with evidence, the final checks and review, the ledger,
off-target dispositions, the limit history and usage, the requested and reported models
(Claude's effort is recorded as unknown), every turn and the verified artifacts. It is
evidence of a local execution and of artifact integrity, not a cryptographic attestation
of the environment.

## Work area

`<store>/work/<run-id>/` is private and separate from the run store:

| Path | Content |
|---|---|
| `clone/`, `gitdir/` | The owned clone and its git metadata |
| `executor/<turn>/` | The executor attempt's scratch (TMPDIR, Go caches) |
| `control/<turn>/settings.json` | The rendered Claude settings, outside every model write root |
| `review/<turn>/launcher/` | The reviewer's cwd: `source/` copy, `packet/`, `evidence/`, `scratch/` |
| `review/<turn>/control/` | The Codex output schema and output file |
| `checks/`, `profiles/` | Verifier attempt roots and sandbox profiles |

The executor's settings deny reading `<store>/runs`.

## Acceptance coverage

`go test ./internal/engine ./cmd/niten` (macOS) runs the roadmap's scenarios on the
go-two-step fixture, a two-step plan on a small Go module published by Shogun's own code.

- **The six-step acceptance flow.** A major finding is repaired, and the re-review and
  its checks name the new SHA. `done` follows the receipt. The first review is blind.
  The repair sees the handoff as notes.
- **Negative scenarios.** These cover:
  - a false `done` caught by the real check, and a smuggled status;
  - invented paths, evidence, decisions, verifications and reviewer references, plus
    collected reviewer evidence;
  - an empty approval, an approval of another SHA, a missing check, a refused command
    and a disabled test;
  - a model mismatch and an invalid schema;
  - the final reserve with a recorded raise, and the invocation limit;
  - an open major, no progress, and a dispute on the same SHA;
  - a check that edits the sources.
- **Boundary scenarios.** These cover:
  - an accepted off-target helper with an untrusted explanation;
  - a protected-path edit rejecting the whole result;
  - injection text in a handoff and in the diff;
  - the human step gate;
  - a human criterion with an attestation;
  - a blocking question;
  - a saved result applied after a crash, and an interrupt.

The prompt-injection scenarios check mechanical boundaries: roles, profiles, packets and
provenance. They do not claim that any model is robust against injection.
