---
name: niten
description: Execute an approved Shogun plan as a pair session — this Claude session implements each step, Codex independently reviews every step and the final change, and the user is asked whenever access, a decision or missing information is needed. Use when the user asks to execute, implement or do a Shogun plan, or a ticket that has one (e.g. "execute the plan", "do this ticket by its plan", "/niten <plan.md>").
---

# Niten: execute a Shogun plan as a pair session

You are the **executor**. Codex is the **reviewer**: it checks every step and the final
change independently, in a read-only sandbox, through `niten.py`. The user is the
**human in the loop**: ask them whenever you need access, a decision or something the
plan does not give you. You work in the user's normal environment (their git, docker,
cloud CLIs, network); permission prompts are how the user approves anything
outward-facing — never try to get around them.

Script: `python3 ${CLAUDE_SKILL_DIR}/scripts/niten.py <command>`. Session state lives
next to the plan, in `<plan>.niten/`. Arguments given to the skill: `$ARGUMENTS` — a
plan path or a ticket key; if empty, ask which plan.

## Hard rules

1. **No step is done without a reviewer approval.** Steps go in plan order. A step is
   approved only by `niten.py review <step>`; you never declare it yourself. A Stop hook
   blocks ending your turn while the current step is unapproved, unless you are waiting
   for the user (rule 4) or a review is running. Do the steps yourself in this session:
   do not hand step work to sub-agents, whose commands may escape the hooks and the log.
2. **No delivery before the final review.** No `git push` and no pull request until
   `niten.py final` approved the exact commits you deliver; a hook enforces it, and a
   commit after the final review needs a new final review.
3. **Evidence, not claims.** A hook logs every Bash command you run, failed ones too,
   with its exit code and output, to `<plan>.niten/commands.jsonl`, together with the
   user's messages and answers; the reviewer trusts that log over your words. For each
   step also write `<plan>.niten/evidence/<step>.md`: for every verification `V-NNN`,
   which logged command proves it and what it showed (digests, IDs); after a review,
   add your answer to each finding (fixed how, or why you disagree). Run the
   verifications as real commands so they are logged. Never put secrets in commands'
   output or in the evidence.
4. **Ask instead of guessing or working around.** When you need access (credentials, a
   login, a permission), a decision the plan leaves open, or information you cannot
   find: ask with AskUserQuestion. If you must end your turn to wait, first run
   `niten.py pause "<what you need>"`; the pause ends by itself when the user answers.
5. **Stay in the plan's scope.** Do exactly the step's actions: no side refactors, no
   extra improvements. If the plan is wrong or impossible, stop and ask the user — do
   not silently deviate. A changed plan stops all reviews.
6. **Important commands are the user's call, with your reason.** Pushes (git, images),
   registry logins, cloud changes (any `aws` operation other than describe/list/get),
   infrastructure, cluster and release changes, pull requests, writes to web APIs,
   recursive forced deletes, publications, remote shells, `sudo`, and Niten overrides:
   give every such Bash call a `description` that tells the user, in one or two
   sentences, what it does and why the current step needs it. A hook puts the call to
   the user with that text, even where the permission settings would allow it, and
   refuses it without one. Writes to other systems through MCP tools and file writes
   outside the plan's repositories are put to the user too. If the user declines, do
   not retry another way — ask what to do. Follow the plan's preconditions (e.g. "the
   tag must be absent") and its stop conditions exactly.
7. **The record is not yours to edit.** The session state, the command log and the
   reviews in `<plan>.niten/`, the plan and its receipts, and Claude's settings are
   protected; only `niten.py` changes the state. You write only `evidence/*.md`.

## Procedure

### 1. Start

1. Find the plan: the path given, or the Shogun plan library (`plans_dir` in
   `~/.config/shogun/config.toml`, by project and ticket key).
2. Read the whole plan: goal, requirements and criteria, scope and non-goals, decisions,
   context facts, steps, end-to-end verification.
3. Prepare the repositories the plan names (frontmatter `repos`; aliases `repo-N` in
   "Inputs and versions"):
   - `git fetch`; show the user each repo's branch, how far it is behind its upstream,
     and any uncommitted or untracked files.
   - Propose a work branch per repo you will change (named after the ticket key, from
     the up-to-date default branch) and ask the user to confirm before switching
     branches or touching a dirty tree.
4. `niten.py start --plan <plan> [--repo NAME=PATH ...]`. It refuses a plan without its
   approval receipt (and runs `shogun verify` when Shogun is installed); only the user
   can choose `--unapproved`. Repositories are found by `--repo`, the plan's
   `<plan>.manifest.json`, `$NITEN_WORKSPACE/<name>`, then `<cwd>/<name>`. It records
   each repo's base commit and pre-existing changes (not part of your change) and
   registers the session for this Claude session.
5. Tell the user the plan in a few lines: steps, which are outward-facing, what access
   you expect to need. Ask for anything you already know you will need (e.g. a cloud
   login) before you begin.

### 2. Each step, in order

1. `niten.py status` — confirm the current step.
2. Do the step's actions. Respect its dependencies, preconditions and stop conditions.
3. Run every verification `V-NNN` of the step as a command, and write
   `evidence/<step>.md` (rule 3).
4. Commit the step's repository changes on the work branch, with a message that names
   the ticket key and the step. A review refuses uncommitted new changes. Operation-only
   steps have nothing to commit.
5. `niten.py review <step>`, as a **background** Bash command (`run_in_background`):
   a review takes minutes, longer than a foreground command may run. Wait for it to
   finish, then read its output. The reviewer sees only this step's changes (since the
   previous step's approval), the step's log entries and your evidence, and settles
   every earlier finding one by one.
   - **APPROVED** → next step.
   - **CHANGES REQUESTED** → fix every blocker and major finding (minor ones: fix if
     cheap and in scope), commit the fix, update the evidence, review again.
   - After **3** unapproved reviews, `review` refuses: show the user the findings and
     your view and ask how to proceed. Another review then runs as
     `niten.py review <step> --user-approved "<their decision>"`, which the user
     confirms in a permission prompt.
   - If the reviewer fails (timeout, no verdict), retry once; then ask the user.

### 3. Final review and delivery

1. Run the plan's end-to-end verification and write `evidence/final.md`.
2. `niten.py final` (in the background, like a step review); fix and repeat as for
   steps. If the reviewer approves but could not verify some criteria, the result is
   **NEEDS THE USER'S CHECK**: show the user exactly what to check; when they confirm,
   run `niten.py confirm "<what they checked>"`, which they approve in a permission
   prompt.
3. After approval, ask the user how to deliver: push the work branch(es) and open pull
   request(s) (their Git host and conventions), or leave the branches local. Do what
   they choose; each push goes through a permission prompt.
4. `niten.py finish`. Report: steps with their review counts, what changed where, what
   external operations were done (with digests/IDs), what is left to the user.

### Interruptions

- Continue after a restart or `/clear`: `niten.py attach --state <plan>.niten`, then
  `niten.py status`.
- Begin again: `niten.py start --plan <plan> --restart` archives the old session (the
  user decides; it goes through a permission prompt). Abandon: `niten.py finish --abort`
  (likewise).
- Settings (environment): `NITEN_REVIEW_MODEL` (default `gpt-6-astra`),
  `NITEN_REVIEW_EFFORT` (default `high`), `NITEN_REVIEW_TIMEOUT` seconds (default 1800),
  `NITEN_CODEX` (path to codex; default: Shogun's `codex_command`, then PATH),
  `NITEN_WORKSPACE` (where repositories live). Extra commands to put to the user:
  `~/.claude/niten/config.json`, `{"ask": ["<regex>", ...]}`.
