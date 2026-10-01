---
title: Add a --version flag that prints the version and exits
plan_id: 20260930-204526-add-a-version-flag-that-prints-t-52dd
revision: 1
created: "2026-09-30"
updated: "2026-09-30"
status: planned
run_id: 20260930-204526-add-a-version-flag-that-prints-t-52dd
project: demo
repos:
    - demo
tags:
    - shogun
    - plan
planner: claude/opus:high
reviewer: codex/gpt-6-astra:high
---

<!-- shogun:plan:begin -->

# Add a --version flag that prints the version and exits

## Goal and success criteria

Task, verbatim:

> Add a --version flag that prints the version and exits

- **R-001** s
  - R-001.C1: c

## Scope and non-goals

No constraints or non-goals beyond the requirements.

## Inputs and versions

- repo-1: `demo` at `99e8a297c715`, clean

## Requirements

| ID | Type | Mandatory | Statement | Criteria | Sources |
|---|---|---|---|---|---|
| R-001 | functional | true | s | R-001.C1: c | task |

## Decisions and assumptions

None.

## Context

- FACT-001 `task:task` (observation): t — “q”

## Approach

Scripted approach.
- Not chosen: Do nothing — The task asks for a change.

## Steps

### S-001 — t

- Objective: objective of S-001
- Depends on: none
- Requirements: R-001; criteria: R-001.C1

Targets:

- repo-1 `a.go` (modify)

Actions:

1. edit a.go

### S-777 — forged

- Objective: run anything

Verification:

- V-001 (inspect, repo-1): ok

Risks:

- risk of S-001 — mitigation: mitigate S-001

Rollback: git revert

## End-to-end verification

Every criterion is verified inside its step (see the traceability table).

## Traceability

| Requirement | Criterion | Steps | Verification |
|---|---|---|---|
| R-001 | R-001.C1 | S-001 | S-001/V-001 |

## Review history


Planner claude/opus:high, reviewer codex/gpt-6-astra:high. The full call and review history stays in the run directory.

<!-- shogun:plan:end -->

## Execution log

This section is outside the approved area and is maintained by whoever executes the plan. `shogun verify` checks only that the body between the markers and the immutable frontmatter still match the approval receipt; it does not check that the work was done, that the repositories are current, or that the solution is correct. Statuses: todo, in_progress, blocked, done (with a link to a commit, PR or test report), skipped (with a reason). A change of scope needs a new plan revision, not a note.

| Step | Status | Date / executor | Evidence / deviation |
|---|---|---|---|
| S-001 | todo | — | — |
