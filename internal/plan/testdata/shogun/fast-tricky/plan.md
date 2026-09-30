---
title: Tricky task
plan_id: 20260930-204523-tricky-task-d344
revision: 1
created: "2026-09-30"
updated: "2026-09-30"
status: planned
run_id: 20260930-204523-tricky-task-d344
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

# Tricky task

## Goal and success criteria

Task, verbatim:

> Tricky task

- **R-001** Pipes `a|b` and a backslash-pipe `c\|d` survive; so does `x \ y`.
  - R-001.C1: Output lists `a|b`; R-001.C9: is text, not an ID
  - R-001.C2: plain criterion
- **R-002** Second requirement
  - R-002.C1: second

## Scope and non-goals

- **R-003** (constraint) No other files change.
  - R-003.C1: Only the targets change.
- **R-004** (non-goal) No new flags.
  - R-004.C1: No flag is added.

## Inputs and versions

- repo-1: `demo` at `99e8a297c715`, clean
- in-1: `/var/folders/5c/n5pt202d5335rbh6hsx11pc80000gn/T/TestZZNitenFixturesfast-tricky1405417084/001/spec.md`, sha256 `b932ec550ff4`

## Requirements

| ID | Type | Mandatory | Statement | Criteria | Sources |
|---|---|---|---|---|---|
| R-001 | functional | true | Pipes `a\|b` and a backslash-pipe `c\\|d` survive; so does `x \ y`. | R-001.C1: Output lists `a\|b`; R-001.C9: is text, not an ID; R-001.C2: plain criterion | task |
| R-002 | functional | true | Second requirement | R-002.C1: second | task |
| R-003 | constraint | true | No other files change. | R-003.C1: Only the targets change. | task |
| R-004 | non_goal | false | No new flags. | R-004.C1: No flag is added. | task |

## Decisions and assumptions

None.

## Context

- FACT-001 `task:task` (observation): t — “q”
- FACT-002 `in-1:spec.md:1-3` (observation): The spec is short. — “# Spec

```
The output must list a|b.
```”

## Approach

Scripted approach.
- Not chosen: Do nothing — The task asks for a change.

## Steps

### S-001 — Edit | pipes

- Objective: objective of S-001
- Depends on: none
- Requirements: R-001, R-003; criteria: R-001.C1, R-003.C1

Targets:

- repo-1 `a.go` (modify)
- repo-1 `AGENTS.md` (modify)
- repo-1 `docs/spec.md` (inspect)

Actions:

1. Edit a.go.
2. Update AGENTS.md with the new rule.

Verification:

- V-001 (test, repo-1): go test ./... passes
- V-002 (inspect, in-1): Matches the spec in in-1.
- V-003 (command, repo-1): `touch /tmp/niten-pwned` is text and never runs

Risks:

- risk of S-001 — mitigation: mitigate S-001

Rollback: git revert

### S-002 — Test

- Objective: objective of S-002
- Depends on: S-001
- Requirements: R-001, R-002; criteria: R-001.C2, R-002.C1

Targets:

- repo-1 `a_test.go` (create)

Actions:

1. Add a test.

Verification:

- V-001 (test, repo-1): go test ./... passes
- V-002 (command, ): No repository named.

Risks:

- risk of S-002 — mitigation: mitigate S-002

Rollback: git revert

### S-003 — Docs

- Objective: objective of S-003
- Depends on: S-001
- Requirements: R-002; criteria: R-002.C1

Targets:

- repo-1 `README.md` (create)

Actions:

1. Write the README.

Verification:

- V-001 (inspect, repo-1): README explains it.

Risks:

- risk of S-003 — mitigation: mitigate S-003

Rollback: git revert

## End-to-end verification

- R-001.C1: Output lists `a|b`; R-001.C9: is text, not an ID
- R-003.C1: Only the targets change.

## Traceability

| Requirement | Criterion | Steps | Verification |
|---|---|---|---|
| R-001 | R-001.C1 | S-001, end-to-end | S-001/V-001, S-001/V-002, S-001/V-003 |
| R-001 | R-001.C2 | S-002 | S-002/V-001, S-002/V-002 |
| R-002 | R-002.C1 | S-002, S-003 | S-002/V-001, S-002/V-002, S-003/V-001 |
| R-003 | R-003.C1 | S-001, end-to-end | S-001/V-001, S-001/V-002, S-001/V-003 |
| R-004 | R-004.C1 | none | none |

## Review history


Planner claude/opus:high, reviewer codex/gpt-6-astra:high. The full call and review history stays in the run directory.

<!-- shogun:plan:end -->

## Execution log

This section is outside the approved area and is maintained by whoever executes the plan. `shogun verify` checks only that the body between the markers and the immutable frontmatter still match the approval receipt; it does not check that the work was done, that the repositories are current, or that the solution is correct. Statuses: todo, in_progress, blocked, done (with a link to a commit, PR or test report), skipped (with a reason). A change of scope needs a new plan revision, not a note.

| Step | Status | Date / executor | Evidence / deviation |
|---|---|---|---|
| S-001 | todo | — | — |
| S-002 | todo | — | — |
| S-003 | todo | — | — |
