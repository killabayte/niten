# Practical Review Decisions and Local Evidence

Date: 2026-09-30. This records the second user-supplied review and a read-only
check of the actual plan archive, Shogun source and local sandbox tooling.
Repository documents stay in English; conversation stays in Russian.
No model calls, plan regeneration or Shogun source edits were performed here.

## Verified archive

Configured plan library: `~/workspace/shogun-plans`. All four Markdown/receipt
pairs returned exit 0 and `valid` from the local `shogun verify`. This confirms
body/metadata integrity, not implementation correctness or freshness of the base.

| Archived plan | Steps and dependencies | Methods | Planning base |
|---|---|---|---|
| stats JSON, ID ending `6be3` | Two, S-002 depends on S-001 | test, command, inspect | One clean repo |
| stats JSON, ID ending `1b4e` | Two, S-002 depends on S-001 | test, command, inspect | One clean repo |
| stats JSON, ID ending `5d31` | Two, S-002 depends on S-001 | test, command, inspect | One clean repo; prior-plan input listed |
| Three-repository plan | Three, S-001 then S-002 then S-003 | command, inspect | Three repos, each with local changes |

These are three distinct stats plan IDs, each with receipt revision 1, rather
than three revisions of one plan ID. No adjacent manifest sidecars were found.
The checked `~/workspace/.shogun/runs` and Shogun repo `.shogun/runs` both exist
and are empty; Niten has no such run store. This is a scoped filesystem check,
not proof that no recoverable copy exists anywhere on the machine.

Shogun source is still `8ca1289283689adaf5aece60eb6d1fa7eef7c708` with a clean
working tree. `publish` currently installs only receipt and plan. Manifest
fingerprints contain digests, not archived dirty file bytes. Current
`cmd/shogun/stats.go` already implements JSON output, so replaying the old task
against current Shogun would not make a meaningful implementation pilot.

## Accepted decisions before P0

| Finding | Decision | Contract |
|---|---|---|
| Required run directory is absent | S0 publishes the approved-generation manifest beside plan/receipt; the run becomes optional | [Sidecar prerequisite](../shogun-manifest-sidecar.md), [import](../shogun-contract.md) |
| Planning-input archive blocks portable execution | No blanket archive requirement; required execution references still need matching content | Import contract |
| Coordinator tests have no sandbox | Mandatory third process profile, direct macOS Seatbelt backend, first tested offline | [P0 profiles](../p0-profile.md), [architecture](../architecture.md) |
| Pilot is too late | Bounded sequential pilot immediately after P3 | [Roadmap](../roadmap.md) |
| Existing DAGs give no independent-step overlap | P4 moves outside v0.1; mechanism chosen after pilot evidence | Architecture, roadmap, README |
| Pilot needs user supervision | gate-per-step with durable exact-candidate user continuation | Architecture, P3 |
| First review can be anchored by success claims | Exclude executor success/handoff narrative; retain labeled off-target justifications | Architecture, P3 |
| Probe is too broad and short | P0a single-project; P0b extra contexts separately; each model gets up to ten minutes sequentially | P0 profiles |
| measure has no current case | Keep the schema value; reject unsupported execution before model calls in v0.1 | Import contract |
| Framework scope is implicit | README explicitly says Go CLI with internal packages first; public SDK deferred | README |

## Important limits to the recommendations

A new manifest sidecar helps new publications, not the four legacy plans whose
matching manifests are currently unavailable. Preserve those approvals; recover
authentic metadata or generate a new approved triplet after S0. Never infer lost
snapshot data from shortened hashes or silently use today's working tree.

Likewise, input bytes are unnecessary for a self-contained approved body, but a
step requiring an external schema/specification cannot be executed from its hash.
Missing execution-critical content remains a question, not permission to guess.

The pilot should use a clean fixture genuinely missing the feature, with a newly
approved two-step plan. Planning generation, P0 probes and pilot execution have
separate authorizations/budgets. The revised twenty-minute probe envelope is a
proposal, not permission to spend more than a previously approved run budget.

`/usr/bin/sandbox-exec` exists locally and its installed man page marks it
deprecated. No verifier sandbox/helper was executed in this design pass. The
backend is a specific, testable macOS choice, not proof of isolation or a promise
of future OS compatibility. P0a must prove the generated policy and refuse any
unsandboxed fallback. The verifier is not a third model.

The three-repository plan is the first concrete v0.2 workflow candidate. Dirty multi-repo support
requires actual patch/untracked contents, file metadata and cross-repo acceptance,
not just manifest fingerprints. Its current rejection in v0.1 stays intentional.

## Result and next implementation work

The design now has a concrete S0 publication prerequisite and three process
profiles, with the sequential pilot before any concurrency implementation.
Next: implement the offline P0a harness and S0 separately, then run only explicitly
authorized probes. No runnable Niten, completed S0 or passed P0 is claimed here.
