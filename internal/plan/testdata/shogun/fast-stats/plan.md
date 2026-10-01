---
title: Add a `--json` flag to `shogun stats`.
plan_id: 20260930-204521-add-a-json-flag-to-shogun-stats-46f6
revision: 1
created: "2026-09-30"
updated: "2026-09-30"
status: planned
run_id: 20260930-204521-add-a-json-flag-to-shogun-stats-46f6
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

- **R-001** `shogun stats --json` prints exactly one JSON document on stdout instead of the table and the totals text.
  - R-001.C1: With --json, all of stdout parses as a single JSON object with the keys `runs` and `totals`. stdout contains no table header ('RUN') and no 'total:' line.
- **R-002** The JSON has one object per run, sorted by id like the table. Every object, including a damaged run's, has id, status, mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd and usage. A damaged run has status "damaged", its reason in `damaged`, and null for the values that are unavailable.
  - R-002.C1: For the test fixture, `runs` has 5 entries in id order. Run 20260927-000001-a has status approved, mode fast, planner claude/opus:high, reviewer codex/gpt-6-astra:high, attempts 2, active_seconds 150, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20, cost_usd 0.5 and usage ok. Run 20260927-000002-b has usage unknown.
  - R-002.C2: The damaged run 20260927-000003-c has status "damaged" and a non-empty `damaged` reason. Runs that are not damaged have no `damaged` key.
  - R-002.C3: Every one of the 5 run objects has all 14 keys id, status, mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd and usage. In the damaged run 20260927-000003-c, mode, planner, reviewer, attempts, active_seconds, the five token keys, cost_usd and usage are present with the value null.
- **R-003** The JSON totals hold the run count, runs by status, the damaged count, the summed attempts, active seconds, token sums and cost, and whether the attempts/time and the tokens/cost are lower bounds. The totals follow the same rules as the text totals.
  - R-003.C1: For the fixture, `totals` has runs 5, by_status {approved:1, failed:1, needs_input:1, paused:1}, damaged 1, attempts 6, active_seconds 750, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20 and cost_usd 0.5. spend_lower_bound and usage_lower_bound are both true.

## Scope and non-goals

- **R-004** (constraint) Without --json the stats output stays exactly as it is now.
  - R-004.C1: The existing assertions in TestStatsCountsEveryRunAndFlagsMissingUsage on the plain `stats` output pass unchanged. The diff leaves the table/totals printing code in cmdStats (stats.go:65-116) untouched.
- **R-005** (constraint) stats stays read-only with --json.
  - R-005.C1: After `stats --json`, the fixture's state.json of 20260927-000001-a is byte-identical to its content before the run.

## Inputs and versions

- repo-1: `demo` at `99e8a297c715`, clean

## Requirements

| ID | Type | Mandatory | Statement | Criteria | Sources |
|---|---|---|---|---|---|
| R-001 | functional | true | `shogun stats --json` prints exactly one JSON document on stdout instead of the table and the totals text. | R-001.C1: With --json, all of stdout parses as a single JSON object with the keys `runs` and `totals`. stdout contains no table header ('RUN') and no 'total:' line. | task |
| R-002 | functional | true | The JSON has one object per run, sorted by id like the table. Every object, including a damaged run's, has id, status, mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd and usage. A damaged run has status "damaged", its reason in `damaged`, and null for the values that are unavailable. | R-002.C1: For the test fixture, `runs` has 5 entries in id order. Run 20260927-000001-a has status approved, mode fast, planner claude/opus:high, reviewer codex/gpt-6-astra:high, attempts 2, active_seconds 150, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20, cost_usd 0.5 and usage ok. Run 20260927-000002-b has usage unknown.; R-002.C2: The damaged run 20260927-000003-c has status "damaged" and a non-empty `damaged` reason. Runs that are not damaged have no `damaged` key.; R-002.C3: Every one of the 5 run objects has all 14 keys id, status, mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd and usage. In the damaged run 20260927-000003-c, mode, planner, reviewer, attempts, active_seconds, the five token keys, cost_usd and usage are present with the value null. | task, repo-1 |
| R-003 | functional | true | The JSON totals hold the run count, runs by status, the damaged count, the summed attempts, active seconds, token sums and cost, and whether the attempts/time and the tokens/cost are lower bounds. The totals follow the same rules as the text totals. | R-003.C1: For the fixture, `totals` has runs 5, by_status {approved:1, failed:1, needs_input:1, paused:1}, damaged 1, attempts 6, active_seconds 750, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20 and cost_usd 0.5. spend_lower_bound and usage_lower_bound are both true. | task, repo-1 |
| R-004 | constraint | true | Without --json the stats output stays exactly as it is now. | R-004.C1: The existing assertions in TestStatsCountsEveryRunAndFlagsMissingUsage on the plain `stats` output pass unchanged. The diff leaves the table/totals printing code in cmdStats (stats.go:65-116) untouched. | task |
| R-005 | constraint | true | stats stays read-only with --json. | R-005.C1: After `stats --json`, the fixture's state.json of 20260927-000001-a is byte-identical to its content before the run. | task |

## Decisions and assumptions

- Q-001 (assumption): Are these JSON key names and this shape acceptable? The shape is {runs:[{id,status,mode,planner,reviewer,attempts,active_seconds,input_tokens,cache_read_tokens,cache_write_tokens,output_tokens,reasoning_tokens,cost_usd,usage,damaged?}], totals:{runs,by_status,damaged,attempts,active_seconds,input_tokens,cache_read_tokens,cache_write_tokens,output_tokens,reasoning_tokens,cost_usd,spend_lower_bound,usage_lower_bound}}. → Use the proposed names with two lower-bound flags. The text output already distinguishes attempts/time (lower bound only when runs are damaged) from tokens/cost (also when usage is missing).
- Q-002 (assumption): With --json and no runs in the directory, should stats print an empty document (runs [] and zero totals) or keep today's behaviour (a note on stderr and nothing on stdout)? → Print an empty document on stdout and no stderr note. Moving the JSON call below the early return would switch to the other option.

## Context

- FACT-001 `repo-1:cmd/shogun/stats.go:39-44` (observation): cmdStats has only a --dir flag today. It parses flags with fs.Parse. — “fs := a.newFlagSet("stats")
	dir := fs.String("dir", "", "runs directory to read ...")
	if err := fs.Parse(args); err != nil {”
- FACT-002 `repo-1:cmd/shogun/stats.go:60-63` (observation): When there are no runs, stats writes a note to stderr, prints nothing on stdout and exits 0. — “if len(runs) == 0 {
		fmt.Fprintf(a.stderr, "no runs in %s\n", root)
		return ExitOK”
- FACT-003 `repo-1:cmd/shogun/stats.go:65-92` (observation): The table loop prints one row per run and adds up the totals in the same loop. A damaged run shows status 'damaged', '-' in every other column and its reason in the USAGE column. It is left out of the sums and out of byStatus. — “fmt.Fprintf(tw, "%s\tdamaged\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\t%s\n", r.id, r.damaged)”
- FACT-004 `repo-1:cmd/shogun/stats.go:81-82` (observation): Row columns: id, status, mode, planner, reviewer (shown as '-' when empty), attempts, active time in minutes, input, cache read, cache write, output and reasoning tokens, cost, and usage state from r.usage(). — “r.id, r.st.Status, mode, dash(r.planner), dash(r.reviewer),
			c.Attempts, c.ActiveSeconds/60, c.InputTokens, c.CacheReadTokens, c.CacheWriteTokens, c.OutputTokens, c.ReasoningTokens, c.CostUSD, u)”
- FACT-005 `repo-1:cmd/shogun/stats.go:94-116` (observation): The totals print the run count, runs by status and the damaged count. Attempts and active time are lower bounds when any run is damaged. Tokens and cost are lower bounds when any run has 'incomplete' or 'unknown' usage, or any run is damaged. — “spendAtLeast := "" // a damaged run's attempts and time are missing from the totals too
	if damaged > 0 {”
- FACT-006 `repo-1:cmd/shogun/stats.go:24-35` (observation): The usage state is 'ok', 'incomplete' or 'unknown'. It is computed from r.st.Counters, so it cannot be called for a damaged run (st == nil). — “func (r runStats) usage() string {
	c := r.st.Counters”
- FACT-007 `repo-1:internal/run/run.go:43-58` (observation): Counter types: Attempts is int, ActiveSeconds is float64, the token counts are int64 and CostUSD is float64. state.json uses snake_case JSON names (active_seconds, input_tokens, cache_read_tokens, cost_usd, ...). — “ActiveSeconds float64 `json:"active_seconds"` ... InputTokens      int64   `json:"input_tokens,omitempty"`”
- FACT-008 `repo-1:cmd/shogun/other.go:134,154-157` (observation): The existing --json idiom (shogun status) is a Bool flag named json, followed by json.MarshalIndent with a one-space indent and Fprintln to a.stdout. — “asJSON := fs.Bool("json", false, "print state.json") ... data, _ := json.MarshalIndent(st, "", " ")
		fmt.Fprintln(a.stdout, string(data))”
- FACT-009 `repo-1:cmd/shogun/stats_test.go:14-78` (observation): The existing test builds 5 runs: approved, paused with no usage, needs_input, failed, and one damaged run whose state.json is broken. It checks the table and totals text, the modes, and that state.json is unchanged afterwards. — “code, out, errs := runCLI(t, ws, "stats") ... if after, _ := os.ReadFile(...); string(after) != string(before) {
		t.Fatal("stats changed a run")”
- FACT-010 `repo-1:cmd/shogun/main.go:85-87` (observation): The usage text lists each command's flags. The stats line currently lists only [--dir d]. — “shogun status <run-id|dir> [--json] show run state
  shogun stats [--dir d]              time, attempts and tokens of every run in .shogun/runs”
- FACT-011 `repo-1:README.md:237` (observation): The README command table lists the stats flags (currently only `--dir`). — “| `shogun stats` | every run in `.shogun/runs` (stopped and failed too): ... unreadable ones `damaged`; `--dir` |”
- FACT-012 `task:task.md` (observation): Table output must not change, and stats must still write nothing. — “Without the flag the output stays exactly as it is now. The command stays read-only.”
- FACT-013 `task:task.md` (assumption): Every run object, damaged or not, carries all the declared keys (id, status, mode, planner, reviewer, attempts, active_seconds, the five token counts, cost_usd, usage). For a damaged run the values the table shows as '-' (mode, planner, reviewer, the counters, cost and usage) are JSON null, not fabricated zeros, and the reason is in `damaged`. Only runs that are not damaged omit the `damaged` key. Planner and reviewer of readable runs are emitted as the raw strings (empty if unknown), not as '-'. Active time is given in seconds and cost is not rounded. The per-state counts of incomplete/unknown usage from the 'not counted' line are left out because each run's usage field already carries them. — “prints the same data as its table and totals as one JSON document on stdout instead of the table”

## Approach

Add a `--json` Bool flag to cmdStats. After runs are read and sorted, and if the flag is set, call a new self-contained function in stats.go. That function builds typed structs for the runs and totals, using the same rules as the text totals (damaged runs are left out of the sums and by_status; spend is a lower bound if any run is damaged; tokens/cost are a lower bound if any run's usage is not ok or any run is damaged). Every run object has the same keys. The fields a damaged run cannot supply are pointer-typed without omitempty, so they encode as null there. The function prints the result with json.MarshalIndent plus Fprintln, like `shogun status --json`, and returns before the tabwriter. The table code stays byte-for-byte as it is. The existing stats test is extended to run `stats --json` on the same fixture. The usage line and the README row gain `--json`.
- Not chosen: Refactor the table loop into a shared totals helper that both outputs use — This touches the table path the task freezes and is a refactor the task does not ask for. A separate JSON builder keeps the plain output provably unchanged.
- Not chosen: Reuse run.Counters as the per-run JSON object — Its omitempty token tags would drop zero fields. It also carries fields that are not in the table (logical_calls, wait_seconds, ...), so the JSON would not be the same data as the table.
- Not chosen: Emit zero counters or omit the fields for damaged runs — Zeros would fabricate values the run never reported, and omitting the fields gives damaged runs a different shape from the declared one (F-001). null states that the value is unavailable.

## Steps

### S-001 — Add --json output to shogun stats

- Objective: `shogun stats --json` prints the runs and totals as one JSON document, and the plain output is unchanged.
- Depends on: none
- Requirements: R-001, R-002, R-003, R-004; criteria: R-001.C1, R-002.C1, R-002.C2, R-002.C3, R-003.C1, R-004.C1

Targets:

- repo-1 `cmd/shogun/stats.go` (modify)
- repo-1 `cmd/shogun/main.go` (modify)
- repo-1 `README.md` (modify)

Actions:

1. In cmdStats (stats.go:40-41), add `asJSON := fs.Bool("json", false, "print the runs and totals as one JSON document")` next to the --dir flag.
2. Add the JSON types in stats.go. statsRunJSON is one flat struct: ID string `id`, Status string `status`, then Mode, Planner and Reviewer *string (`mode`, `planner`, `reviewer`), Attempts *int `attempts`, ActiveSeconds *float64 `active_seconds`, InputTokens/CacheReadTokens/CacheWriteTokens/OutputTokens/ReasoningTokens *int64 (`input_tokens`, `cache_read_tokens`, `cache_write_tokens`, `output_tokens`, `reasoning_tokens`), CostUSD *float64 `cost_usd`, Usage *string `usage`, all of these without omitempty so nil encodes as null, and finally Damaged string `damaged,omitempty`. statsTotalsJSON has Runs int `runs`, ByStatus map[string]int `by_status`, Damaged int `damaged`, Attempts int, ActiveSeconds float64, the five token sums as int64 and CostUSD float64 (same tag names as the run object, no omitempty), SpendLowerBound bool `spend_lower_bound` and UsageLowerBound bool `usage_lower_bound`. The top-level document is {Runs []statsRunJSON `runs`; Totals statsTotalsJSON `totals`}.
3. Add `func (a *app) printStatsJSON(runs []runStats) int`. It loops over runs in their sorted order. For a damaged run (st == nil) it appends {ID: r.id, Status: "damaged", Damaged: r.damaged} with every pointer field left nil, and counts it as damaged. Otherwise it copies r.mode, r.planner, r.reviewer (raw, no dash), each field of st.Counters and r.usage() into local variables and sets the pointer fields to their addresses. It then increments ByStatus[string(st.Status)] and adds the counters to the totals the same way as stats.go:83-90. It sets SpendLowerBound = damaged > 0 and UsageLowerBound = (any readable run's usage != "ok") || damaged > 0. It initialises Runs as an empty slice and ByStatus as an empty map so they encode as [] and {}. It prints with `data, _ := json.MarshalIndent(doc, "", " ")` and `fmt.Fprintln(a.stdout, string(data))`, then returns ExitOK. Add "encoding/json" to the imports.
4. In cmdStats, if *asJSON, return a.printStatsJSON(runs) right after the sort at stats.go:59 and before the `len(runs) == 0` check. An empty run set then still prints a document (runs [] and zero totals) and nothing goes to stderr (assumption in Q-002). Do not change the lines from stats.go:60 onwards.
5. Update the usage line in main.go:87 to `shogun stats [--dir d] [--json]` and keep the column alignment. Add `--json` next to `--dir` in the README.md:237 stats row (e.g. '`--dir`, `--json` prints the same as one JSON document').

Verification:

- V-001 (command, repo-1): `go build ./... && go vet ./cmd/shogun` succeed.
- V-002 (inspect, repo-1): `git diff cmd/shogun/stats.go` shows no changed or removed lines in the table/totals printing block (the original lines 60-116). Only the flag, the JSON branch, the new types/function and the import are added. statsRunJSON has no omitempty on any tag except `damaged`.

Risks:

- Taking the address of the loop variable's fields could make every run object point at the same values. — mitigation: Copy each value into a fresh local inside the loop body before taking its address. The S-002 test checks distinct values for runs 20260927-000001-a and 20260927-000002-b (usage ok vs unknown), so shared pointers would fail it.

Rollback: Revert the commit that touches stats.go, main.go and README.md. Nothing is persisted or migrated.

### S-002 — Test stats --json on the existing fixture

- Objective: Prove the JSON content, the uniform run shape and read-only behaviour, and keep the plain-output assertions.
- Depends on: S-001
- Requirements: R-001, R-002, R-003, R-004, R-005; criteria: R-001.C1, R-002.C1, R-002.C2, R-002.C3, R-003.C1, R-004.C1, R-005.C1

Targets:

- repo-1 `cmd/shogun/stats_test.go` (modify)

Actions:

1. In TestStatsCountsEveryRunAndFlagsMissingUsage, keep every existing assertion unchanged. After the existing read-only check, run `runCLI(t, ws, "stats", "--json")` and require ExitOK.
2. Assert that stdout does not contain "RUN" or "total:". Unmarshal the whole stdout into a document struct with `runs` as []map[string]any and `totals` as map[string]any, and fail if unmarshalling fails.
3. Assert R-002.C1: 5 runs in id order, the listed field values for 20260927-000001-a, and usage "unknown" for 20260927-000002-b.
4. Assert R-002.C2: 20260927-000003-c has status "damaged" and a non-empty `damaged`, and 20260927-000001-a has no `damaged` key.
5. Assert R-002.C3: for every run object, each of the 14 keys is present (`_, ok := m[k]`). For 20260927-000003-c, mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd and usage are present with value nil.
6. Assert the R-003.C1 totals values, including by_status without damaged and both lower-bound flags true.
7. Re-read 20260927-000001-a/state.json and compare it with `before` again to check that --json did not write to the run.

Verification:

- V-003 (test, repo-1): `go test ./cmd/shogun -run TestStats -v` passes. It covers R-001.C1, R-002.C1, R-002.C2, R-002.C3, R-003.C1 and R-005.C1, and the unchanged plain-output assertions cover R-004.C1.
- V-004 (test, repo-1): `go test ./...` passes.

Rollback: Revert the test change together with S-001.

## End-to-end verification

- R-001.C1: With --json, all of stdout parses as a single JSON object with the keys `runs` and `totals`. stdout contains no table header ('RUN') and no 'total:' line.
- R-002.C1: For the test fixture, `runs` has 5 entries in id order. Run 20260927-000001-a has status approved, mode fast, planner claude/opus:high, reviewer codex/gpt-6-astra:high, attempts 2, active_seconds 150, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20, cost_usd 0.5 and usage ok. Run 20260927-000002-b has usage unknown.
- R-002.C2: The damaged run 20260927-000003-c has status "damaged" and a non-empty `damaged` reason. Runs that are not damaged have no `damaged` key.
- R-002.C3: Every one of the 5 run objects has all 14 keys id, status, mode, planner, reviewer, attempts, active_seconds, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens, cost_usd and usage. In the damaged run 20260927-000003-c, mode, planner, reviewer, attempts, active_seconds, the five token keys, cost_usd and usage are present with the value null.
- R-003.C1: For the fixture, `totals` has runs 5, by_status {approved:1, failed:1, needs_input:1, paused:1}, damaged 1, attempts 6, active_seconds 750, input_tokens 100, cache_read_tokens 1000, cache_write_tokens 0, output_tokens 50, reasoning_tokens 20 and cost_usd 0.5. spend_lower_bound and usage_lower_bound are both true.
- R-004.C1: The existing assertions in TestStatsCountsEveryRunAndFlagsMissingUsage on the plain `stats` output pass unchanged. The diff leaves the table/totals printing code in cmdStats (stats.go:65-116) untouched.
- R-005.C1: After `stats --json`, the fixture's state.json of 20260927-000001-a is byte-identical to its content before the run.

## Traceability

| Requirement | Criterion | Steps | Verification |
|---|---|---|---|
| R-001 | R-001.C1 | S-001, S-002, end-to-end | S-001/V-001, S-001/V-002, S-002/V-003, S-002/V-004 |
| R-002 | R-002.C1 | S-001, S-002, end-to-end | S-001/V-001, S-001/V-002, S-002/V-003, S-002/V-004 |
| R-002 | R-002.C2 | S-001, S-002, end-to-end | S-001/V-001, S-001/V-002, S-002/V-003, S-002/V-004 |
| R-002 | R-002.C3 | S-001, S-002, end-to-end | S-001/V-001, S-001/V-002, S-002/V-003, S-002/V-004 |
| R-003 | R-003.C1 | S-001, S-002, end-to-end | S-001/V-001, S-001/V-002, S-002/V-003, S-002/V-004 |
| R-004 | R-004.C1 | S-001, S-002, end-to-end | S-001/V-001, S-001/V-002, S-002/V-003, S-002/V-004 |
| R-005 | R-005.C1 | S-002, end-to-end | S-002/V-003, S-002/V-004 |

## Review history

- plan: 1 review round(s), 1 finding(s)

Planner claude/opus:high, reviewer codex/gpt-6-astra:high. The full call and review history stays in the run directory.

<!-- shogun:plan:end -->

## Execution log

This section is outside the approved area and is maintained by whoever executes the plan. `shogun verify` checks only that the body between the markers and the immutable frontmatter still match the approval receipt; it does not check that the work was done, that the repositories are current, or that the solution is correct. Statuses: todo, in_progress, blocked, done (with a link to a commit, PR or test report), skipped (with a reason). A change of scope needs a new plan revision, not a note.

| Step | Status | Date / executor | Evidence / deviation |
|---|---|---|---|
| S-001 | todo | — | — |
| S-002 | todo | — | — |
