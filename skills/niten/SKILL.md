---
name: niten
description: Execute an approved Shogun plan as a pair session — this Claude session implements each step, Codex independently reviews every step and the final change, and the user is asked whenever access, a decision or missing information is needed. Use when the user asks to execute, implement or do a Shogun plan, or a ticket that has one (e.g. "execute the plan", "do this ticket by its plan", "/niten <plan.md>").
---

# Niten: execute a Shogun plan as a pair session

You are the **executor**. Codex is the **reviewer**: it checks every step and the final
change independently, in a read-only sandbox, through `niten.py`. The user is the
**human in the loop**: ask them whenever you need access, a decision or something the
plan does not give you. You work in the user's normal environment (their git, docker,
cloud CLIs, network); Claude Code's own permission prompts are how the user approves
anything outward-facing — never try to get around them.

Script: `python3 ${CLAUDE_SKILL_DIR}/scripts/niten.py <command>`. State and evidence
live next to the plan, in `<plan>.niten/`.

## Hard rules

1. **No step is done without a reviewer approval.** Steps go in plan order. A step is
   approved only by `niten.py review <step>`; you never declare it yourself. A Stop hook
   blocks ending your turn while the current step is unapproved — unless the session is
   paused (rule 4).
2. **No delivery before the final review.** No `git push` and no pull request until
   `niten.py final` approved the whole change; a PreToolUse hook enforces it.
3. **Evidence, not claims.** For every step write `<plan>.niten/evidence/<step>.md`:
   each command you ran (exact), its exit code and the relevant output, and for every
   verification `V-NNN` what proves it. The reviewer cannot use the network or write, so
   external operations (registries, cloud APIs) are judged from this file — record
   digests, IDs and outputs verbatim. Never put secrets or tokens in it.
4. **Ask instead of guessing or working around.** When you need access (credentials, a
   login, a permission), a decision the plan leaves open, or information you cannot
   find: ask the user with AskUserQuestion. If you must end the turn to wait for them,
   first run `niten.py pause "<what you need>"`; after the answer run `niten.py resume`.
5. **Stay in the plan's scope.** Do exactly the step's actions. No side refactors, no
   extra improvements, no edits to the plan file. If the plan is wrong or impossible,
   stop and ask the user — do not silently deviate.
6. **Important commands are the user's call, with your reason.** Pushes (git, images),
   registry logins, cloud changes (any `aws` operation other than describe/list/get),
   infrastructure, cluster and release changes, pull requests, writes to web APIs,
   recursive forced deletes, publications, remote shells and `sudo`: give every such
   Bash call a `description` that tells the user, in one or two sentences, what it does
   and why the current step needs it. A hook puts the call to the user with that text,
   even where the permission settings would allow it, and refuses it without one. The
   user's answer to the prompt is the decision: if they decline, do not retry another
   way — ask what to do. Follow the plan's preconditions (e.g. "the tag must be absent")
   and its stop conditions exactly.

## Procedure

### 1. Start

1. Find the plan: the path the user gave, or the Shogun plan library (`plans_dir` in
   `~/.config/shogun/config.toml`, by project and ticket key). Verify it with
   `shogun verify <plan>` if `shogun` is on PATH; otherwise check that
   `<plan>.approval.json` exists. An unapproved or changed plan → ask the user.
2. Read the whole plan: goal, requirements and criteria, scope and non-goals, decisions,
   context facts, steps, end-to-end verification.
3. Prepare the repositories the plan names (frontmatter `repos`; aliases `repo-N` in
   "Inputs and versions"):
   - `git fetch`; show the user each repo's branch, how far it is behind its upstream,
     and any uncommitted or untracked files.
   - Propose a work branch per repo you will change (named after the ticket key, from
     the up-to-date default branch) and ask the user to confirm before switching
     branches or touching a dirty tree.
4. Run `niten.py start --plan <plan> [--repo NAME=PATH ...]`. Repositories are found by
   `--repo`, then the plan's `<plan>.manifest.json`, then `$NITEN_WORKSPACE/<name>`,
   then `<cwd>/<name>`. It records each repo's base commit and pre-existing changes
   (those are not part of your change), registers the session for this Claude session
   and lists the steps.
5. Tell the user the plan in a few lines: steps, which are outward-facing, what access
   you expect to need. Ask for anything you already know you will need (e.g. a cloud
   login) before you begin.

### 2. Each step, in order

1. `niten.py status` — confirm the current step.
2. Do the step's actions. Respect its dependencies, preconditions and stop conditions.
3. Run every verification `V-NNN` of the step. Record everything in
   `evidence/<step>.md` (rule 3).
4. `niten.py review <step>`. It prints the verdict, per-criterion status and findings.
   - **APPROVED** → next step.
   - **CHANGES REQUESTED** → fix every blocker and major finding (minor ones: fix if
     cheap and in scope), update the evidence, review again.
   - After **3** unapproved reviews of the same step, or if you disagree with a finding
     on substance, stop and ask the user (show the finding and your reasoning).
   - If the reviewer fails (timeout, no verdict), retry once; then ask the user.
5. Commit the step's repository changes locally on the work branch with a message that
   names the ticket key and the step. Operation-only steps have no commit.

### 3. Final review and delivery

1. Run the plan's end-to-end verification and record it in `evidence/final.md`.
2. `niten.py final`; fix and repeat as for steps.
3. After approval, ask the user how to deliver: push the work branch(es) and open pull
   request(s) (their Git host and conventions), or leave the branches local. Do what
   they choose; the permission prompts approve each push.
4. `niten.py finish`. Report: steps with their review counts, what changed where, what
   external operations were done (with digests/IDs), what is left to the user.

### Interruptions

- Continue an existing session after a restart or `/clear`:
  `niten.py attach --state <plan>.niten`, then `niten.py status`.
- Abandon: `niten.py finish --abort` (ask the user first).
- Settings (environment): `NITEN_REVIEW_MODEL` (default `gpt-6-astra`),
  `NITEN_REVIEW_EFFORT` (default `high`), `NITEN_REVIEW_TIMEOUT` seconds (default 1800),
  `NITEN_CODEX` (path to codex; default: Shogun's `codex_command`, then PATH),
  `NITEN_WORKSPACE` (where repositories live).
