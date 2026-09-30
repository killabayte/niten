# Niten Development Roadmap

Status: proposed plan for v0.1, 2026-09-30. This is a design document, not an
execution plan approved through Shogun. It builds on the [architecture](architecture.md)
and the [import contract](shogun-contract.md).

Status, evening of 2026-09-30: the offline P0a deliverables exist in the repository,
namely the verifier sandbox backend with its harness (`internal/verify/sandbox`),
the domain [contracts](contracts.md) with schemas and fixtures (`internal/contract`)
and `cmd/niten` with version, help and an offline doctor. S0 is implemented on the
Shogun branch `s0-manifest-sidecar` and awaits review and merge. P1 `niten prepare`
is implemented against that branch ([import](import.md)). No live probes or model
runs have been performed.

Goal of the first version: reproducibly execute a small real Shogun plan
in one repository, fix a defect found by Astra, survive a stop and
finish the work with evidence that refers to the final commit.

Stage IDs are retained for traceability. Delivery order is P0a and S0, P1, P2,
P3, the P3 pilot, then P5 for v0.1. P0b is conditional on extra context roots;
P4 is a post-pilot decision outside v0.1.

## P0a Verify one project and three process profiles

Build the offline disposable harness per [P0 profiles](p0-profile.md). First test
the coordinator-owned verifier sandbox directly, without model calls. It must
permit required Go operations and deny network, protected reads, outside writes
and child/symlink escapes. Failure prevents untrusted verification; no plain-exec
fallback is allowed. A disposable verification copy is not the security boundary.

Then separately authorize the executor/reviewer live probes, sequentially: at
most two CLI invocations, ten minutes each and twenty minutes total, no retries.
There are no context repository copies in P0a. Incomplete controls remain
inconclusive, with all artifacts preserved. This revised budget is only a proposal
for a future run, not existing permission to spend it.

After the profile checks, establish the minimal Go module, cmd/niten, version/help
and contracts for candidates, off-target edits, handoff, findings, reviews,
checks, human step gates, attestations and implemented/done. Fix the external
store and local export CAS. Unimplemented commands remain explicitly documented.

Acceptance:

- all three process profiles have positive and negative controls;
- verifier evidence includes backend/profile/environment/OS identity;
- required model controls use tool events and host assertions, not self-reports;
- exact model IDs/xhigh, unknown telemetry and forbidden delegation are explicit;
- missing sandbox support, malformed policy or unavailable certification fails closed;
- model messages cannot impersonate coordinator/user events;
- basic schemas, offline build/vet and contract fixtures pass;
- Claude's optional dollar cap remains separate from the shared invocation/time limits.

P1 and the main P2 follow P0a. Failed isolation requires an explicit environment
redesign; it does not silently select Linux, a container or weaker permissions.

P0a status: the verifier profile passed its offline positive and negative controls
(details in [P0 profiles](p0-profile.md)); the contracts, schemas, fixtures and the
CLI skeleton exist and their tests pass. An independent review found two defects
in this slice, both fixed with regressions the same day: a `setsid` descendant could
outlive `Run` (now swept with `lsof` and retired by `Seal`), and `check_evidence`
accepted `status: passed` with a failed assertion. Still open in P0a: the separately
authorized executor and reviewer live probes. The export CAS and the external store
are recorded as contract decisions and are implemented in P2/P5.

## P0b Add context repositories only when needed

Extend the verifier and CLI probes with read-only context roots, including
file-tool and child-shell negative controls. This has its own certificate and
separate proposed live budget: one invocation per model, sequentially, at most
ten minutes each/twenty total, no retries. Do not auto-chain it after P0a.
It gates multi-root context support, not the single-project pilot.

## S0 Publish the Shogun manifest sidecar

Implement the [scoped publication change](shogun-manifest-sidecar.md) in Shogun
as a separate prerequisite PR. Publish the exact approved-generation manifest
beside plan/receipt, preserve old pair verification, and cover no-clobber, crash
gaps, output exclusions, mode permissions and digest consistency with offline
tests. No model calls are needed for that PR's implementation or tests.

New triplets are portable without source run retention. Old plans with lost
manifests are not silently repaired. Fresh pilot plan generation is a separately
authorized live operation after S0; source-repo changes are not made by these
design documents. S0 acceptance is required by Niten P1.

S0 status: implemented on the Shogun branch `s0-manifest-sidecar` (commit
`bb3f7e6`), with publication of the triplet, `--require-manifest` for `shogun
verify`, the drift-check tolerance for older runs and offline tests for the
acceptance list; the full Shogun test suite passes. The branch is not pushed or
merged yet.

## P1 Import existing Shogun plans

Implement `niten prepare`, offline `shogun verify`, manifest validation,
the supported Markdown importer, the normalized contract, DAG and coverage checks.
Collect source spans and hashes; reject unknown formats with an exact reason.

Acceptance on fixtures of the real renderer:

- fast and thorough published triplets produce the same execution contract for
  the same approved content without a run directory;
- an optional legacy-run manifest is checked, and disagreement with a sidecar is rejected;
- missing planning-input archives do not block a self-contained plan, while a
  missing required execution input still yields needs_input;
- a changed approved body, an invalid receipt/manifest, a dirty/drifted repo,
  path traversal, an unknown repo and a cyclic dependency do not allow a run;
- the mutable execution log does not change the identity of requirements;
- a repeated `V-001` in different steps does not collide; escaped table text and IDs
  are not lost; ambiguous Markdown is not guessed;
- a verification without a command is stored as an expected result with an unset
  check_spec, not run as shell;
- `prepare` does not call Claude/Codex and does not change the source plan.
- targets are separated from the hard policy; external ownership is set before the launch, and
  an input with an unclear owner does not automatically turn into a human attestation.

A measure method is retained in schema but rejected as unsupported before a run;
it is not downgraded to another method. If parser fixtures expose ambiguity, fix
the specific contract or propose a typed export separately; grep matches of four
real plans are useful samples, not a production-parser correctness proof.
Dependencies: P0a domain contract and S0 publication acceptance.

P1 status: implemented offline on 2026-09-30, see [import](import.md). The acceptance
items are covered by tests on fourteen triplets that Shogun's own code published at S0:
fast and thorough agree on the execution semantics without a run directory; a legacy
run manifest is checked and a disagreeing one rejected; a reference-only input needs
nothing while a verification that names an input yields `needs_input` until matching
bytes are supplied; a changed body, an invalid receipt or manifest, a drifted or dirty
repository, path traversal, an unknown repository and a cycle are refused; the
execution log does not change requirement identity; repeated `V-001`, escaped table
text and forged headings are handled without guessing; every `check_spec` is null;
prepare starts no model and leaves the plan and the repository unchanged; owners are
`niten` unless the user assigns `human`; `measure` is `needs_input` unless assigned to
a human. The same scenarios pass against a real Shogun build of the S0 branch. All four
archived plans parse with the strict grammar. Formal P1 acceptance still depends on
S0 being reviewed and merged in Shogun.

## P2 Isolated work and a recoverable runner

Implement the owned clone, immutable candidates, the disposable verification copy,
the supervisor, adapters, mandatory verifier sandbox, the run store and offline
`doctor`. `Verifier.Run` never invokes repository code outside its certified
backend; the same supervisor enforces descendant teardown and deadlines. For controlled
checks use a fake CLI, as in Shogun, without network model requests.

Acceptance:

- writes and checkpoints touch only the owned clone; the source repo does not change;
- the separate gitdir, read-only context copies and the external store are not part of the write
  roots; any hard violation rejects the whole candidate, with no partial commit;
- a model's code edit does not change the repo instructions of the next invocation;
- stdout/stderr are drained concurrently; large/truncated events, an exit error
  and a missing terminal event do not produce a false successful result;
- cancel terminates the children; a fresh result file of another attempt is not reused;
- environment filtering covers built-in and custom names without printing values;
- disk-full/fsync errors do not lead to acceptance without evidence;
- a crash before start, during a write, after the result and before the state checkpoint
  recovers the known outcomes or honestly leaves `outcome_unknown`;
- a second coordinator does not get the same run; a stale PID does not kill someone else's process;
- a saved result is recovered without a new model call.

Dependencies: P0a, plus P0b only for extra context roots. P2 automates the profile
harness: verifier checks remain offline, model checks require `doctor --live`.
When the launcher/settings/CLI fingerprint changes, a new separately
authorized certification is required; the early manual probe does not cover a changed adapter.

## P3 First end-to-end executor

First a sequential loop on the same candidate/message contracts that
will be used for the parallel scheduler. Implement `run`, `status`, `resume`,
the sandboxed check runner, finding ledger, budget, gate-per-step and final gate.
Make the first review blind to executor success claims. Every transition is reproducible
without a GUI and without live models.

Acceptance on scripted providers and a small Go fixture repo:

1. Import an approved two-step fixture plan.
2. Fake Opus changes the code, returns a check_spec and a candidate.
3. Niten records the changes and runs a real local check of the fixture.
4. Fake Astra returns a major finding; the step remains unaccepted.
5. Fake Opus fixes the code; the check and the re-review refer to the new SHA.
6. All criteria and final checks pass; `done` appears only after
   the execution receipt is saved.

Mandatory negative scenarios: a false `done`, invented paths/evidence,
an empty review, approve of someone else's SHA, a missing check, a disabled test, model
mismatch, an invalid schema, an exceeded limit, an open major, no-progress and a test
that modified the sources of the verification copy before finishing successfully.
Separately: an admissible off-target helper is accepted by the reviewer, a protected-path
violation rejects the whole result, the handoff does not replace instructions/decisions, injection
text in the diff does not change the role or permissions. The prompt injection check evaluates
mechanical boundaries and scenarios, and does not declare full robustness of any model.
The behaviour of the system is checked, not the textual match of prompts.

Also verify that a human step gate pauses after acceptance, spends no calls while
waiting, and accepts only a user decision for that exact gate/candidate. First
review packets exclude executor success/handoff prose; off-target explanations
are labeled untrusted and cannot replace coordinator evidence.

Dependencies: P1 + offline P2. The sequential P3 slice is the basis for the first
live pilot and v0.1; parallelism is not a condition for running that pilot.

## P3 pilot Learn from the sequential pair

After P3 offline checks and current P0/P2 certificates, separately authorize one
small two-step plan on a clean fixture repo. Use a newly approved Shogun triplet
from S0, with a task genuinely absent from that fixture. Historical stats --json
plans are examples, not permission to replay an implemented feature or overwrite
old evidence. Fresh Shogun planning and Niten execution have separate budgets.

Proposed execution envelope: at most 12 CLI invocations, 45 minutes active wall
time, at most two repair cycles per step, with gate-per-step enabled. No automatic
restart, prompt retuning, model substitution or limit increase. This envelope is
not authorization for a live run. Record exact profiles/models/effort, evidence,
phase durations, review findings, false alarms judged against code, handoff gaps
and incomplete usage. Report whether a repair cycle actually occurred; a clean
run does not prove live defect detection or live repair behavior.

Inspect the result before deciding on P4, default budgets or prompt changes.
Preserve failure, timeout and no-progress outcomes. The pilot can end with a
useful negative result; another run requires a new explicit envelope. No speedup
claim follows from a sequential pilot.

## P5 v0.1 acceptance

Finish offline `verify`, local `export`, human attestations,
the JSON/human-readable report, diagnostics and the launch documentation.
Verify recovery and the final gate on the sequential workflow.

Offline gate:

- format, vet, build, meaningful unit/integration tests and the race suite pass;
- the receipt verifies plan/contract/candidate/check/review digests; a missing artifact
  and a changed tree are detected;
- after SIGINT the run continues with the same spend and the saved findings;
- execution does not change the source repo, the plan and the historical attempts;
- export creates only a missing ref with the exact SHA, does not change the checkout,
  does not update an existing branch even fast-forward and is safe under a race/ref conflict;
- manual criteria yield implemented/pending_external; an exact user attestation
  moves the result to done, a forgery from the model/someone else's SHA/a replaced failed test is rejected;
- the exit codes for paused, failed gate and implemented differ; the status JSON keeps the reason.

Use the P3 pilot evidence during acceptance. A newly discovered defect is fixed
and checked offline; another live run is not automatic. Changes to a certified
profile require separately authorized re-certification. P5 does not wait for P4.
Dependencies: P3 and its recorded pilot outcome, passing offline gates, and the
current required P0/P2 certificates. A blocking pilot defect prevents release
until resolved; a failed pilot is not silently treated as acceptance.

## P4 Parallelism after the pilot and outside v0.1

Choose the mechanism using recorded phase time, review usefulness, handoff gaps
and budget pressure. Options are one speculative dependent step, reviewer
preparation while the same step is implemented, or retaining the sequential loop.
The independent-step scheduler alone does not overlap the observed linear plans.
The pilot informs the choice; a later controlled comparison must establish any
claimed speed/cost benefit. Adding a preparation call per step is a budget change.

Before enabling a selected mechanism, test actual overlap using deterministic
barriers, immutable reviewer snapshots, exact-SHA approvals, one-executor authority,
stale evidence invalidation, bounded work ahead and atomic budget reservations.
For dependent speculation, test dependency repair after downstream work already
exists: preserve it as unaccepted, run repair-only next and re-review the cumulative
candidate before continuation. For reviewer preparation, a rubric/probe proposal
is advisory and cannot approve code that did not exist when it was written.
All mechanisms must survive duplicate events, cancellation and recovery without
losing findings or double acceptance. Race tests must cover the selected scheduler.

Dependencies: the P3 pilot, a documented mechanism decision and its own offline
and separately authorized live validation. Do not make P4 a v0.1 release gate.

## Tests that actually matter

The main risks are accepting someone else's version of the code, losing work after a crash, repeating
a write with an unknown outcome, leaving the scope and declaring success without verification.
The tests target these. Tests for every line of documentation, a snapshot
of the whole prompt or a load-testing platform are not needed until a real need appears.

## Decisions after the first pilot

| Question | Current decision | When to revisit |
|---|---|---|
| CLI or API | Claude Code + Codex CLI, confirmed by the user | On an explicit request to change the integration |
| One or several executors | One Opus | If a measurable need and a new role contract appear |
| One or several repos with edits | One clean repo in v0.1 | Multi-repo/dirty snapshot work in v0.2 |
| Fresh or persistent sessions | Fresh sessions with a short handoff | Based on measured cost and context loss |
| Communication within one turn | At invocation boundaries | If feedback latency hinders real plans |
| Import | Published manifest triplet; legacy run optional | If parser fixtures justify typed export |
| Delivery | Local export to a new ref; PR separately | A separate user workflow |

The next concrete work is the P0a offline verifier/CLI harness and the separate
S0 sidecar implementation. Then run only the explicitly authorized model probes.
Prioritize the sequential P3 pilot; keep P4 and a general SDK outside v0.1.
