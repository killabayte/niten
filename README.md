# niten

[![ci](https://github.com/killabayte/niten/actions/workflows/ci.yml/badge.svg)](https://github.com/killabayte/niten/actions/workflows/ci.yml)

Niten executes an approved [Shogun](https://github.com/killabayte/shogun) plan as a **pair
session** in Claude Code. Claude implements each step in your normal environment, Codex
reviews every step and the final change independently, and you are asked whenever access,
a decision or missing information is needed.

It is a Claude Code skill (`/niten`) with a small script and two hooks. Shogun plans the
work with two models; Niten carries it out with the same two models in fixed roles.

## How it works

- **Claude is the executor.** It follows the plan step by step: code changes, commands,
  operations outside the repository (registries, cloud CLIs, infrastructure tools). It
  works with your tools and your network, and Claude Code's permission prompts are how you
  approve anything outward-facing.
- **Codex is the reviewer.** After each step, `niten.py review` runs `codex exec` in a
  read-only sandbox with the step, the repositories' diffs against their base commits and
  the step's evidence. Codex returns a structured verdict: each acceptance criterion as
  met, not met or not verifiable, and findings with severity and the exact fix. A step
  passes only on `approve` with no blocker or major finding and no unmet criterion;
  otherwise Claude fixes the findings and asks again. A final review checks the whole
  change against the plan before anything is delivered.
- **You are in the loop.** Claude asks when it needs a login, a permission, a decision the
  plan leaves open or information it cannot find. It does not guess around a gap. After
  three unapproved reviews of one step, or a disagreement on substance, it asks you too.
- **Evidence, not claims.** For every step Claude records the exact commands, exit codes and
  outputs (digests, IDs) in an evidence file. The reviewer cannot use the network, so
  external operations are judged from that record.

## What is enforced

Two hooks make the key rules hold whatever the model does. They act only in the Claude
Code session that started a Niten session and do nothing anywhere else.

| Hook | Rule |
|---|---|
| `Stop` | Claude cannot end its turn while the current step, or the final review, is not approved, unless the session is paused to wait for you. |
| `PreToolUse` (Bash) | No `git push` and no pull request before the final review approved the change. |

Everything else (scope, evidence, asking instead of guessing) is the skill's instruction
to the model, backed by the reviewer, which rejects out-of-scope or unproven work.

## Requirements

- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) and the
  [Codex CLI](https://github.com/openai/codex), both logged in.
- Python 3.9 or later and git.
- A Shogun plan: `<plan>.md` with its `<plan>.approval.json` (and, from newer Shogun,
  `<plan>.manifest.json`, which tells Niten where the repositories are).

## Install

```
git clone https://github.com/killabayte/niten.git && cd niten
./install.sh
```

The skill is linked into `~/.claude/skills/niten`, so `git pull` updates it. The hooks are
added to `~/.claude/settings.json` after a backup; running the installer again changes
nothing. `./install.sh uninstall` removes both. `CLAUDE_CONFIG_DIR` is honoured. Start a
new Claude Code session afterwards.

## Use

In Claude Code, from the directory that holds the plan's repositories:

```
/niten path/to/plan.md
```

or just ask Claude to execute a plan or a ticket that has one. Claude checks the plan's
approval, prepares a work branch per repository with your confirmation, starts the
session and goes step by step. At the end it asks how to deliver (push and pull request,
or local branches only).

## The script

`skills/niten/scripts/niten.py` keeps the session state and runs the reviewer; Claude
calls it, you rarely need to.

| Command | What it does |
|---|---|
| `start --plan P [--repo NAME=PATH]` | Records each repository's base commit and pre-existing changes, registers the session for this Claude Code session |
| `status` | Steps, their review counts, what is next |
| `review S-NNN` | Codex reviews the current step |
| `final` | Codex reviews the whole change, after every step is approved |
| `pause "reason"` / `resume` | Wait for you without the Stop hook blocking |
| `attach --state DIR` | Continue a session in a new Claude Code session |
| `finish [--abort]` | End the session |

Repositories are found by `--repo` (by name or `repo-N` alias), then the plan's manifest,
then `$NITEN_WORKSPACE/<name>`, then `<cwd>/<name>`.

State lives next to the plan in `<plan>.niten/`: `state.json`, `evidence/<step>.md`, and
`reviews/` with every reviewer prompt and verdict.

Settings (environment): `NITEN_REVIEW_MODEL` (default `gpt-6-astra`), `NITEN_REVIEW_EFFORT`
(`high`), `NITEN_REVIEW_TIMEOUT` (seconds, `1800`), `NITEN_CODEX` (default: Shogun's
`codex_command`, then `codex` on PATH), `NITEN_WORKSPACE`.

## Tests

```
python3 -m unittest discover -s tests
```

The tests use temporary repositories and a scripted Codex; no model is called.

## History

Niten started as a Go engine that ran plans inside a sealed sandbox with certified CLI
profiles (tag `go-engine-v0.1`). Sealing the agents off from the network, credentials and
other repositories made it unable to carry real work end to end, so it was replaced by
this skill: the same executor and reviewer, in your environment, with you approving
access instead of the sandbox refusing it.

## License

MIT
