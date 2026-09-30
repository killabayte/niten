# Research Behind Niten

Date: 2026-09-30. The READMEs and the sources listed below were read. revmux and
ralphex were downloaded into temporary directories for reading; their code was not
run and not copied into Niten. Below, observations in the code and proposed
decisions are marked separately.

## Studied versions

| Project | Commit |
|---|---|
| Shogun | `8ca1289283689adaf5aece60eb6d1fa7eef7c708` |
| revmux | `ec262b0a8c13d502bfede0b559708a03353f75ad` |
| ralphex | `a736d5ede42fe0deae16e7b28cdae1ee912be6d0` |

These are research snapshots. The links below are pinned to the commits; further
upstream changes require a new check before porting code or behavior.

## Shogun

Observation: supervised Claude/Codex subprocesses, JSON schemas, structured
plan/review, bounded attempts, atomic artifacts, run locks, requested/reported
telemetry and environment filtering already exist. The profiles are intended for
read-only planning.

Reference sources:

- [Plan and receipt](https://github.com/killabayte/shogun/blob/8ca1289283689adaf5aece60eb6d1fa7eef7c708/internal/library/plan.go): markers, mutable keys, typed metadata canonicalization, `Verify`.
- [Renderer](https://github.com/killabayte/shogun/blob/8ca1289283689adaf5aece60eb6d1fa7eef7c708/internal/pipeline/render.go): Markdown grammar, SHA shortening, scoped verification IDs.
- [Step schema](https://github.com/killabayte/shogun/blob/8ca1289283689adaf5aece60eb6d1fa7eef7c708/internal/planning/schema/step.schema.json): dependencies, targets, expected verification, rollback.
- [Manifest](https://github.com/killabayte/shogun/blob/8ca1289283689adaf5aece60eb6d1fa7eef7c708/internal/inputs/inputs.go) and [repo fingerprints](https://github.com/killabayte/shogun/blob/8ca1289283689adaf5aece60eb6d1fa7eef7c708/internal/inputs/repo.go): source versions and drift.
- [Claude adapter](https://github.com/killabayte/shogun/blob/8ca1289283689adaf5aece60eb6d1fa7eef7c708/internal/provider/claude.go) and [Codex adapter](https://github.com/killabayte/shogun/blob/8ca1289283689adaf5aece60eb6d1fa7eef7c708/internal/provider/codex.go): argv, streams, schema, identity checks.
- [CLI compatibility evidence](https://github.com/killabayte/shogun/blob/8ca1289283689adaf5aece60eb6d1fa7eef7c708/docs/cli-compatibility.md): profile version, unknown Claude effort, delegation ban and the limits of the historical probes.

Decision for Niten: keep the contracts and the offline fixtures approach, reuse
Shogun verify as a CLI. The execution adapters require separate work: enabling
write/Bash fundamentally changes the powers involved. Do not import `internal`
packages of another Go module and do not extract a large shared framework upfront.

## revmux

Observation: this is a supervised review pipeline `find → synthesize → verify`.
`find` runs several sources, stores raw output separately per attempt, retries
suitable errors once and marks degradation. The supervisor drains both streams,
bounds time and terminates process groups. Findings have an explicit severity and
a verification verdict; a missing check may be `unverified`.

- [Supervisor](https://github.com/umputun/revmux/blob/ec262b0a8c13d502bfede0b559708a03353f75ad/app/executor/proc.go).
- [Process group](https://github.com/umputun/revmux/blob/ec262b0a8c13d502bfede0b559708a03353f75ad/app/executor/procgroup_unix.go).
- [Parallel find and attempts](https://github.com/umputun/revmux/blob/ec262b0a8c13d502bfede0b559708a03353f75ad/app/pipeline/find.go).
- [UI events separate from mandatory completion](https://github.com/umputun/revmux/blob/ec262b0a8c13d502bfede0b559708a03353f75ad/app/pipeline/event.go).
- [Finding contract](https://github.com/umputun/revmux/blob/ec262b0a8c13d502bfede0b559708a03353f75ad/app/finding/finding.go) and [verification](https://github.com/umputun/revmux/blob/ec262b0a8c13d502bfede0b559708a03353f75ad/app/pipeline/verify.go).

Decision for Niten: take supervision, the audit trail, the explicit status of an
unverified result and the separation of UI from mandatory events. Do not port the
roster, multi-finder synthesis, confidence voting and the additional model layer:
we have one executor and one mandatory reviewer. Losing Astra suspends acceptance.
A degraded review policy on its own is not suitable for declaring `done`. Automatic
retry of a read-only finder also cannot be blindly applied to the executor.

## ralphex

Observation: the task loop executes tasks through short sessions, checks the
completion signal against actionable checkboxes and bounds the number of
iterations. External review passes findings to Claude for assessment/fixing, has
stalemate logic and separate continuation conditions. The task format differs from
Shogun.

- [Task loop](https://github.com/umputun/ralphex/blob/a736d5ede42fe0deae16e7b28cdae1ee912be6d0/pkg/processor/phase/task.go).
- [External review loop](https://github.com/umputun/ralphex/blob/a736d5ede42fe0deae16e7b28cdae1ee912be6d0/pkg/processor/phase/external_review.go).
- [Session interruption](https://github.com/umputun/ralphex/blob/a736d5ede42fe0deae16e7b28cdae1ee912be6d0/pkg/processor/phase/break_controller.go).
- [Task and checkbox parser](https://github.com/umputun/ralphex/blob/a736d5ede42fe0deae16e7b28cdae1ee912be6d0/pkg/plan/parse.go).
- [Task prompt](https://github.com/umputun/ralphex/blob/a736d5ede42fe0deae16e7b28cdae1ee912be6d0/pkg/config/defaults/prompts/task.txt).

Decision for Niten: take the bounded `implement → check → review → repair` loop,
fresh context and no-progress handling. Do not port the checkbox format, changes to
the approved body, the multi-agent review roster, plan creation, the web dashboard
or notifications. The completion signal is a model message, not the source of
truth. In the studied external review, an empty result and the iteration limit can
end this phase without an error; Niten must require a valid review of the required
candidate. The studied workflow is not a ready-made protocol for Niten's parallel
pair.

## Official model interfaces

- [OpenAI Docs on non-interactive Codex](https://learn.chatgpt.com/docs/non-interactive-mode): `codex exec`, JSONL, a schema for the final answer and use of the saved CLI authentication.
- [GPT-6 Astra](https://developers.openai.com/api/docs/models/gpt-6-astra): the `gpt-6-astra` model and `xhigh` support.
- [Claude Code CLI reference](https://code.claude.com/docs/en/cli-reference): `--effort`, structured output and the headless interface.
- [Claude Code model configuration](https://code.claude.com/docs/en/model-config): Opus 5.5 supports `xhigh`; model defaults and effort restrictions require explicit configuration.

The documentation confirms the interfaces, but not the account's current
entitlements, the reliability of long runs or the possibility of safe writing in a
specific environment. That is the subject of a separate bounded probe, which was
not run here.

After the design review, the local help/version output, the sandbox documentation
and the current Shogun certificate were re-checked. The concrete profiles, the
discrepancies with the review and the limits of verification are in the
[P0 profile](p0-profile.md) and the
[review response](reviews/2026-09-30-design-review.md). The v0.1 decision was
refined: the reviewer checks the immutable candidate from a separate writable
disposable copy.

A subsequent [practical review](reviews/2026-09-30-practical-review.md) checked
the actual archive: four valid plan/receipt pairs, all linear, no manifest sidecars
and no runs in the two checked stores. The resulting decisions are a scoped
Shogun sidecar prerequisite, a separate verifier sandbox, a pilot directly after
P3 and deferral of parallel scheduling beyond v0.1. These are revisions to the
design based on local evidence, not changes attributed to revmux or ralphex.

## Research conclusions

| Decision | Basis |
|---|---|
| CLI subprocesses in Go | Already matches Shogun and the user's choice |
| One executor and review of immutable snapshots | Our design decision against races on a shared working copy |
| Structured findings and evidence | Useful Shogun/revmux contracts, strengthened by binding to the candidate |
| Bounded repair loop | The useful ralphex loop with Niten's own gate |
| Sequential v0.1 and early pilot | The observed linear plans provide no independent-step overlap |
| Published manifest sidecar | Existing retained plans lack their original run directories |
| Dedicated verifier sandbox | A coordinator-launched check does not inherit either CLI sandbox |
| Asynchronous messages at invocation boundaries | Minimal transport without dependence on live steering |
| Fresh sessions first | Explicit context, simple recovery; the cost gain is not yet measured |
| Original codebase without a runtime dependency on revmux/ralphex | Separate mechanisms are needed, not their full pipelines |

Both external projects carry an MIT LICENSE at the studied commits. When copying
code or substantial parts of prompts in the future, the corresponding notices must
be preserved. Today Niten contains its own design documents; the repository license
is kept.
