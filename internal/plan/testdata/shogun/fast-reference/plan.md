---
title: Add a `--json` flag to `shogun stats`.
plan_id: 20260930-204521-add-a-json-flag-to-shogun-stats-934b
revision: 1
created: "2026-09-30"
updated: "2026-09-30"
status: planned
run_id: 20260930-204521-add-a-json-flag-to-shogun-stats-934b
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

# Add a `--json` flag to `shogun stats`.

## Goal and success criteria

Task, verbatim:

> Add a `--json` flag to `shogun stats`.
> 
> With `--json`, `shogun stats` prints the same data as its table and totals as one JSON document on stdout instead of the table: one object per run (id, status, mode, planner, reviewer, attempts, active seconds, input / cache read / cache write / output / reasoning tokens, Claude list-price equivalent, usage state, and the damage reason for a damaged run) plus the totals (runs by status, damaged count, attempts, active seconds, token sums, cost, and whether the totals are lower bounds). Without the flag the output stays exactly as it is now. The command stays read-only.
> 
> Every object in `runs` has all the listed fields, damaged runs included: values that are not available for a damaged run are `null`, not zero, and `damaged` holds the damage reason. The test checks that these keys are present and `null` for the damaged run.
> 
> The attached plan (in-1) is the basis for this correction: keep what it gets right and fix the damaged-run shape. Where it differs from this task, this task wins.

- **R-001** `shogun stats --json` prints exactly one JSON document on stdout instead of the table and totals lines, and exits 0.
  - R-001.C1: On the five-run test fixture, stdout of `stats --json` decodes as one JSON object with keys `runs` and `totals`, with only whitespace after it. It contains no `RUN` header and no `total:` line; the exit code is 0 and stderr is empty.
- **R-002** `runs` holds one object per run, sorted by id as in the table. Every object, damaged runs included, has the keys id, status, mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd, usage and damaged. For a damaged run, status is "damaged", `damaged` holds the damage reason and every other value that is not available is null (not zero). For a readable run, `damaged` is null.
  - R-002.C1: The fixture's approved run (20260927-000001-a) has status "approved", mode "fast", planner "claude/opus:high", reviewer "codex/gpt-6-astra:high", attempts 2, active_seconds 150, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20, cost_usd 0.5, usage "ok" and damaged null. The paused run has usage "unknown".
  - R-002.C2: The fixture's damaged run 20260927-000003-c has status "damaged" and a non-empty string in `damaged`. The keys mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd and usage are all present and null (a key that is missing or holds 0 fails).
  - R-002.C3: All five run objects have the same set of 15 keys, and `damaged` is present and null on the four readable runs.
- **R-003** `totals` holds the runs by status, the damaged count, attempts, active seconds, the five token sums, cost, and whether the totals are lower bounds. These are computed by the same rules as the text totals.
  - R-003.C1: For the fixture, totals has by_status {approved:1, failed:1, needs_input:1, paused:1}, damaged 1, attempts 6, active_seconds 750, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20 and cost_usd 0.5.
  - R-003.C2: For the fixture, spend_lower_bound (attempts and active time; true when damaged > 0) and usage_lower_bound (tokens and cost; true when any run's usage is not ok or damaged > 0) are both true.

## Scope and non-goals

- **R-004** (constraint) Without `--json` the output of `shogun stats` stays exactly as it is now.
  - R-004.C1: The existing text assertions of TestStatsCountsEveryRunAndFlagsMissingUsage are unmodified and pass.
  - R-004.C2: The output of `shogun stats --dir <static runs dir>` from the old and the new binary is byte-identical (the diff is empty).
- **R-005** (constraint) `shogun stats` stays read-only, with or without `--json`.
  - R-005.C1: After `stats --json` on the fixture, the approved run's state.json is byte-identical to before.

## Inputs and versions

- repo-1: `demo` at `99e8a297c715`, clean
- in-1: `/var/folders/5c/n5pt202d5335rbh6hsx11pc80000gn/T/TestZZNitenFixturesfast-reference2886867301/001/prior-plan.md`, sha256 `3083f2b3d70c`

## Requirements

| ID | Type | Mandatory | Statement | Criteria | Sources |
|---|---|---|---|---|---|
| R-001 | functional | true | `shogun stats --json` prints exactly one JSON document on stdout instead of the table and totals lines, and exits 0. | R-001.C1: On the five-run test fixture, stdout of `stats --json` decodes as one JSON object with keys `runs` and `totals`, with only whitespace after it. It contains no `RUN` header and no `total:` line; the exit code is 0 and stderr is empty. | task |
| R-002 | functional | true | `runs` holds one object per run, sorted by id as in the table. Every object, damaged runs included, has the keys id, status, mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd, usage and damaged. For a damaged run, status is "damaged", `damaged` holds the damage reason and every other value that is not available is null (not zero). For a readable run, `damaged` is null. | R-002.C1: The fixture's approved run (20260927-000001-a) has status "approved", mode "fast", planner "claude/opus:high", reviewer "codex/gpt-6-astra:high", attempts 2, active_seconds 150, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20, cost_usd 0.5, usage "ok" and damaged null. The paused run has usage "unknown".; R-002.C2: The fixture's damaged run 20260927-000003-c has status "damaged" and a non-empty string in `damaged`. The keys mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd and usage are all present and null (a key that is missing or holds 0 fails).; R-002.C3: All five run objects have the same set of 15 keys, and `damaged` is present and null on the four readable runs. | task, in-1, repo-1 |
| R-003 | functional | true | `totals` holds the runs by status, the damaged count, attempts, active seconds, the five token sums, cost, and whether the totals are lower bounds. These are computed by the same rules as the text totals. | R-003.C1: For the fixture, totals has by_status {approved:1, failed:1, needs_input:1, paused:1}, damaged 1, attempts 6, active_seconds 750, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20 and cost_usd 0.5.; R-003.C2: For the fixture, spend_lower_bound (attempts and active time; true when damaged > 0) and usage_lower_bound (tokens and cost; true when any run's usage is not ok or damaged > 0) are both true. | task, in-1, repo-1 |
| R-004 | constraint | true | Without `--json` the output of `shogun stats` stays exactly as it is now. | R-004.C1: The existing text assertions of TestStatsCountsEveryRunAndFlagsMissingUsage are unmodified and pass.; R-004.C2: The output of `shogun stats --dir <static runs dir>` from the old and the new binary is byte-identical (the diff is empty). | task |
| R-005 | constraint | true | `shogun stats` stays read-only, with or without `--json`. | R-005.C1: After `stats --json` on the fixture, the approved run's state.json is byte-identical to before. | task |

## Decisions and assumptions

- Q-001 (assumption): With --json and an empty runs directory, should stats print a JSON document with an empty runs list and zero totals, or keep today's behaviour (a 'no runs in …' note on stderr and nothing on stdout)? → Print the JSON document with an empty runs array and zero totals (no stderr note), as in-1 decided; this is easy to switch.

## Context

- FACT-001 `repo-1:cmd/shogun/stats.go:40-44` (observation): cmdStats defines its flags on a FlagSet (only --dir today) and parses them with fs.Parse; there are no positional arguments. — “fs := a.newFlagSet("stats")
	dir := fs.String("dir", "", ...)
	if err := fs.Parse(args); err != nil {”
- FACT-002 `repo-1:cmd/shogun/stats.go:53-63` (observation): Runs are collected via readRunStats and sorted by id; an empty runs directory prints a note to stderr and exits 0. — “sort.Slice(runs, func(i, j int) bool { return runs[i].id < runs[j].id })
	if len(runs) == 0 {
		fmt.Fprintf(a.stderr, "no runs in %s\n", root)
		return ExitOK”
- FACT-003 `repo-1:cmd/shogun/stats.go:65-91` (observation): The table prints per run id, status, mode, planner, reviewer, attempts, active time, five token counts, cost and usage; a damaged run shows status 'damaged', dashes for every other column and its damage reason, and is excluded from the sums and from byStatus. — “fmt.Fprintf(tw, "%s\tdamaged\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\t%s\n", r.id, r.damaged)”
- FACT-004 `repo-1:cmd/shogun/stats.go:103-115` (observation): The totals have two lower-bound conditions: attempts/active time when damaged > 0; tokens and cost when any run's usage is not ok or damaged > 0. — “spendAtLeast := "" // a damaged run's attempts and time are missing from the totals too
	if damaged > 0 {
...
	if len(partial) > 0 || damaged > 0 {
		atLeast = "at least "”
- FACT-005 `repo-1:cmd/shogun/stats.go:17-35` (observation): runStats holds st == nil and a damage reason for a damaged run; usage() dereferences r.st and so may only be called for non-damaged runs. For a damaged run mode, planner and reviewer are never read (readRunStats returns before setting them). — “st                          *run.State
	damaged                     string // why the run could not be read; empty when st is set
...
func (r runStats) usage() string {
	c := r.st.Counters”
- FACT-006 `repo-1:cmd/shogun/other.go:134,154-157` (observation): Precedent `status --json`: a bool flag named json, json.MarshalIndent with one-space indent, printed with Fprintln to a.stdout. — “asJSON := fs.Bool("json", false, "print state.json")
...
		data, _ := json.MarshalIndent(st, "", " ")
		fmt.Fprintln(a.stdout, string(data))”
- FACT-007 `repo-1:internal/run/run.go:43-57` (observation): Counters: Attempts int, ActiveSeconds float64, the five token counts int64, CostUSD float64; state.json uses snake_case keys (attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd). — “Attempts      int     `json:"attempts"`
...
	ActiveSeconds float64 `json:"active_seconds"`
...
	InputTokens      int64   `json:"input_tokens,omitempty"`”
- FACT-008 `repo-1:cmd/shogun/stats_test.go:14-78` (observation): TestStatsCountsEveryRunAndFlagsMissingUsage builds five runs (approved with usage, paused without usage, needs_input, failed, and 20260927-000003-c with invalid state.json), checks the text output and that the approved run's state.json is unchanged. — “code, out, errs := runCLI(t, ws, "stats")”
- FACT-009 `repo-1:cmd/shogun/main.go:97; README.md:237` (observation): The usage text (main.go:97, not :87 as in-1 says) and the README command row list the stats flags (`--dir`). — “shogun stats [--dir d]              time, attempts and tokens of every run in .shogun/runs”
- FACT-010 `task:task.md:5` (observation): Every run object needs the same keys; for a damaged run the unavailable values are null. This replaces in-1's damaged shape, which had only id, status and damaged (in-1 R-002.C2, S-001 action 4). — “Every object in `runs` has all the listed fields, damaged runs included: values that are not available for a damaged run are `null`, not zero, and `damaged` holds the damage reason.”
- FACT-011 `in-1:Approach; S-001 actions 2-5` (observation): in-1 is right about the flag, the snake_case keys, the separate JSON path that leaves the text path alone, the two lower-bound booleans, the MarshalIndent precedent, the docs and the test. Its damaged-run shape is what must be fixed. — “Damaged runs are emitted with a separate small struct (id, status, damaged) so missing counters are absent rather than shown as zeros.”
- FACT-012 `task:task.md:3` (observation): The text output must stay the same and the command must not write anything. — “Without the flag the output stays exactly as it is now. The command stays read-only.”
- FACT-013 `task:task.md:3` (assumption): The key names follow in-1 (snake_case, as in state.json). A damaged run keeps status "damaged", as the table shows it. The `damaged` key is on every object and is null for a readable run, so that every object has the same keys. Planner and reviewer of a readable run stay the raw strings (empty when unknown), as in-1 has them. — “one object per run (id, status, mode, planner, reviewer, attempts, active seconds, ... usage state, and the damage reason for a damaged run)”
- FACT-014 `in-1:Decisions and assumptions, Q-001` (assumption): With --json and no runs, stdout still gets one JSON document with empty runs and zero totals. This is carried over from in-1 as a reversible assumption. — “Print the JSON document with an empty runs array and zero totals (no stderr note), so stdout always holds one JSON document; easy to switch.”

## Approach

Add a `json` bool flag to cmdStats. After the runs are sorted, and only when the flag is set, a new helper printStatsJSON builds {runs, totals}, marshals it with json.MarshalIndent("", " ") like `status --json`, prints it to stdout and returns ExitOK. The text path is left untouched. Each run uses one struct whose fields are pointers without omitempty (id stays a plain string), so a damaged run keeps every key with null values and its reason in `damaged`. A readable run fills every pointer and leaves `damaged` nil. Totals follow the text loop's rules, with two lower-bound booleans. The usage line and the README row gain `--json`. The existing stats test gets a JSON pass on the same fixture that checks the null keys of the damaged run.
- Not chosen: Separate damaged-run struct with only id/status/damaged (in-1) — The task requires every key on every run object, with null for unavailable values.
- Not chosen: Plain value fields that are zero for a damaged run — The task says null, not zero; a zero would look like real data.
- Not chosen: omitempty on counter fields — It drops keys: legitimate zeros disappear from readable runs, and damaged runs lose keys.
- Not chosen: Refactor the text loop into shared aggregation for both outputs — It touches the text path, which must stay byte-identical. A separate small summation carries no risk to it.

## Steps

### S-001 — Add --json to shogun stats

- Objective: When --json is given, print the stats runs and totals as one JSON document, with the damaged-run shape the task asks for; the text output stays unchanged.
- Depends on: none
- Requirements: R-001, R-002, R-003, R-004, R-005; criteria: R-004.C2

Targets:

- repo-1 `cmd/shogun/stats.go` (modify)
- repo-1 `cmd/shogun/main.go` (modify)
- repo-1 `README.md` (modify)

Actions:

1. Before editing, build the current HEAD binary to a path outside the repo (e.g. /tmp/shogun-before). Prepare a static runs directory outside the repo with a few runs, including one damaged run: a copy of an existing .shogun/runs, or a few hand-made run dirs, one of them with an invalid state.json. Capture `/tmp/shogun-before stats --dir <that dir>` to /tmp/stats-before.txt.
2. In cmd/shogun/stats.go add `encoding/json` to the imports. In cmdStats add `asJSON := fs.Bool("json", false, "print runs and totals as JSON")` next to the --dir flag.
3. In cmdStats, right after the sort.Slice line (line 59) and before the `len(runs) == 0` check, add `if *asJSON { return a.printStatsJSON(runs) }`. Change no other line of the text path.
4. Add the JSON types in stats.go. statsRunJSON has ID string `json:"id"` and Status string `json:"status"`. Its other fields are pointers without omitempty: Mode, Planner, Reviewer *string (`mode`, `planner`, `reviewer`); Attempts *int (`attempts`); ActiveSeconds *float64 (`active_seconds`); InputTokens, CacheReadTokens, CacheWriteTokens, OutputTokens, ReasoningTokens *int64 (`input_tokens`, `cache_read_tokens`, `cache_write_tokens`, `output_tokens`, `reasoning_tokens`); CostUSD *float64 (`cost_usd`); Usage *string (`usage`); Damaged *string (`damaged`). statsTotalsJSON has ByStatus map[string]int `by_status`, Damaged int `damaged`, Attempts int, ActiveSeconds float64, the five int64 token sums and CostUSD float64 under the same keys as the run fields, SpendLowerBound bool `spend_lower_bound` and UsageLowerBound bool `usage_lower_bound`. The document struct has Runs []statsRunJSON `runs` and Totals statsTotalsJSON `totals`.
5. Add `func (a *app) printStatsJSON(runs []runStats) int` with a one-line comment in the file's style. Start with a non-nil empty Runs slice and an empty ByStatus map. For a damaged run (r.st == nil): increment Damaged, reason := r.damaged, and append {ID: r.id, Status: "damaged", Damaged: &reason}, leaving every other pointer nil. For a readable run: c := r.st.Counters and status := string(r.st.Status); increment ByStatus[status]; u := r.usage(); if u != "ok" set anyPartial. Take local copies of mode, planner, reviewer, u and the counters, and append a statsRunJSON with every pointer set to them and Damaged nil. Add the counters to the totals exactly as lines 83-90 do. Then set SpendLowerBound = Damaged > 0 and UsageLowerBound = anyPartial || Damaged > 0. Marshal with json.MarshalIndent(doc, "", " "), Fprintln the result to a.stdout, and return ExitOK.
6. In cmd/shogun/main.go:97 change `shogun stats [--dir d]` to `shogun stats [--dir d] [--json]`, keeping the description column aligned. In README.md:237 change the trailing ``; `--dir` `` to ``; `--dir`; `--json` prints the same as JSON ``.
7. Build the changed tree to /tmp/shogun-after, run `stats --dir` without --json on the same static directory into /tmp/stats-after.txt, and diff it against the before capture.

Verification:

- V-001 (command, repo-1): `diff /tmp/stats-before.txt /tmp/stats-after.txt` prints nothing (R-004.C2); `go build ./...` succeeds and `gofmt -l cmd/shogun` prints nothing.

Risks:

- Pointers to a loop variable or a shared local end up aliased, so every run object shows the last run's values. — mitigation: Declare the per-run copies inside the loop body; Go 1.22+ loop semantics also scope them per iteration. The S-002 test checks distinct per-run values (approved vs paused usage).
- A run in the captured directory changes between the before and after captures, which would give a false diff. — mitigation: Capture from a static copy outside any live workspace.

Rollback: Revert cmd/shogun/stats.go, cmd/shogun/main.go and README.md with git checkout. Nothing else is touched and no data is written.

### S-002 — Test stats --json on the existing fixture

- Objective: Prove the document's shape and contents, including the null keys of the damaged run, and prove that the text output is unchanged and the command wrote nothing.
- Depends on: S-001
- Requirements: R-001, R-002, R-003, R-004, R-005; criteria: R-001.C1, R-002.C1, R-002.C2, R-002.C3, R-003.C1, R-003.C2, R-004.C1, R-005.C1

Targets:

- repo-1 `cmd/shogun/stats_test.go` (modify)

Actions:

1. In TestStatsCountsEveryRunAndFlagsMissingUsage, add after the existing state.json comparison (line 77) and leave every existing line unchanged. Run `code, out, errs = runCLI(t, ws, "stats", "--json")` and require ExitOK and empty errs; require that out contains neither "RUN" nor "total:".
2. Decode out with a json.Decoder into struct{ Runs []map[string]any `json:"runs"`; Totals map[string]any `json:"totals"` }. Require that a second Decode returns io.EOF. Add `encoding/json` and `io` to the imports.
3. Require 5 runs in id order. Check the approved run's values per R-002.C1 (numbers decode as float64; damaged present and nil) and the paused run's usage "unknown".
4. Find 20260927-000003-c and require status "damaged" and a non-empty string in `damaged`. For each of mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd and usage, require that the key is present (`v, ok := m[k]; ok`) and that v == nil.
5. Require that every run object has exactly the same 15 keys, and that `damaged` is present and nil on the four readable runs (R-002.C3).
6. Check the totals per R-003.C1, and that spend_lower_bound and usage_lower_bound are both true (R-003.C2).
7. Read the approved run's state.json again and compare it with `before` (R-005.C1).

Verification:

- V-002 (test, repo-1): `go test ./cmd/shogun -run TestStats` passes, covering R-001.C1, R-002.C1, R-002.C2, R-002.C3, R-003.C1, R-003.C2 and R-005.C1.
- V-003 (inspect, repo-1): `git diff cmd/shogun/stats_test.go` shows only added lines (imports and the new block), so the existing text assertions are unmodified and pass in V-002 (R-004.C1).
- V-004 (test, repo-1): `go test ./...` passes.

Rollback: Revert cmd/shogun/stats_test.go; the change is test-only.

## End-to-end verification

- R-004.C1: The existing text assertions of TestStatsCountsEveryRunAndFlagsMissingUsage are unmodified and pass.
- R-004.C2: The output of `shogun stats --dir <static runs dir>` from the old and the new binary is byte-identical (the diff is empty).
- R-005.C1: After `stats --json` on the fixture, the approved run's state.json is byte-identical to before.

## Traceability

| Requirement | Criterion | Steps | Verification |
|---|---|---|---|
| R-001 | R-001.C1 | S-002 | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-002 | R-002.C1 | S-002 | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-002 | R-002.C2 | S-002 | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-002 | R-002.C3 | S-002 | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-003 | R-003.C1 | S-002 | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-003 | R-003.C2 | S-002 | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-004 | R-004.C1 | S-002, end-to-end | S-002/V-002, S-002/V-003, S-002/V-004 |
| R-004 | R-004.C2 | S-001, end-to-end | S-001/V-001 |
| R-005 | R-005.C1 | S-002, end-to-end | S-002/V-002, S-002/V-003, S-002/V-004 |

## Review history


Planner claude/opus:high, reviewer codex/gpt-6-astra:high. The full call and review history stays in the run directory.

<!-- shogun:plan:end -->

## Execution log

This section is outside the approved area and is maintained by whoever executes the plan. `shogun verify` checks only that the body between the markers and the immutable frontmatter still match the approval receipt; it does not check that the work was done, that the repositories are current, or that the solution is correct. Statuses: todo, in_progress, blocked, done (with a link to a commit, PR or test report), skipped (with a reason). A change of scope needs a new plan revision, not a note.

| Step | Status | Date / executor | Evidence / deviation |
|---|---|---|---|
| S-001 | todo | — | — |
| S-002 | todo | — | — |
