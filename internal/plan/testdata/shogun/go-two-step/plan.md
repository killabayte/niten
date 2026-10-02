---
title: Add Sub and Mul to the calc package
plan_id: 20261001-103859-add-sub-and-mul-to-the-calc-pack-549d
revision: 1
created: "2026-10-01"
updated: "2026-10-01"
status: planned
run_id: 20261001-103859-add-sub-and-mul-to-the-calc-pack-549d
project: demo
repos:
    - calc
tags:
    - shogun
    - plan
planner: claude/opus:high
reviewer: codex/gpt-6-astra:high
---

<!-- shogun:plan:begin -->

# Add Sub and Mul to the calc package

## Goal and success criteria

Task, verbatim:

> Add Sub and Mul to the calc package

- **R-001** calc.Sub returns a minus b.
  - R-001.C1: Sub(5, 3) returns 2 and a test covers it
- **R-002** calc.Mul returns a times b.
  - R-002.C1: Mul(4, 3) returns 12 and a test covers it

## Scope and non-goals

No constraints or non-goals beyond the requirements.

## Inputs and versions

- repo-1: `calc` at `bdffc24a02d5`, clean

## Requirements

| ID | Type | Mandatory | Statement | Criteria | Sources |
|---|---|---|---|---|---|
| R-001 | functional | true | calc.Sub returns a minus b. | R-001.C1: Sub(5, 3) returns 2 and a test covers it | task |
| R-002 | functional | true | calc.Mul returns a times b. | R-002.C1: Mul(4, 3) returns 12 and a test covers it | task |

## Decisions and assumptions

None.

## Context

- FACT-001 `task:task` (observation): The task asks for two functions. — “Add Sub and Mul”

## Approach

Scripted approach.
- Not chosen: Do nothing — The task asks for a change.

## Steps

### S-001 — Add Sub

- Objective: objective of S-001
- Depends on: none
- Requirements: R-001; criteria: R-001.C1

Targets:

- repo-1 `calc.go` (modify)
- repo-1 `calc_test.go` (modify)

Actions:

1. Add func Sub(a, b int) int to calc.go.
2. Add TestSub to calc_test.go.

Verification:

- V-001 (test, repo-1): go test ./... passes, including TestSub

Risks:

- risk of S-001 — mitigation: mitigate S-001

Rollback: git revert

### S-002 — Add Mul

- Objective: objective of S-002
- Depends on: S-001
- Requirements: R-002; criteria: R-002.C1

Targets:

- repo-1 `calc.go` (modify)
- repo-1 `calc_test.go` (modify)

Actions:

1. Add func Mul(a, b int) int to calc.go.
2. Add TestMul to calc_test.go.

Verification:

- V-001 (test, repo-1): go test ./... passes, including TestMul

Risks:

- risk of S-002 — mitigation: mitigate S-002

Rollback: git revert

## End-to-end verification

- R-001.C1: Sub(5, 3) returns 2 and a test covers it
- R-002.C1: Mul(4, 3) returns 12 and a test covers it

## Traceability

| Requirement | Criterion | Steps | Verification |
|---|---|---|---|
| R-001 | R-001.C1 | S-001, end-to-end | S-001/V-001 |
| R-002 | R-002.C1 | S-002, end-to-end | S-002/V-001 |

## Review history


Planner claude/opus:high, reviewer codex/gpt-6-astra:high. The full call and review history stays in the run directory.

<!-- shogun:plan:end -->

## Execution log

This section is outside the approved area and is maintained by whoever executes the plan. `shogun verify` checks only that the body between the markers and the immutable frontmatter still match the approval receipt; it does not check that the work was done, that the repositories are current, or that the solution is correct. Statuses: todo, in_progress, blocked, done (with a link to a commit, PR or test report), skipped (with a reason). A change of scope needs a new plan revision, not a note.

| Step | Status | Date / executor | Evidence / deviation |
|---|---|---|---|
| S-001 | todo | — | — |
| S-002 | todo | — | — |
