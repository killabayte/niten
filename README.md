# niten

[![ci](https://github.com/killabayte/niten/actions/workflows/ci.yml/badge.svg)](https://github.com/killabayte/niten/actions/workflows/ci.yml)

Niten executes an approved [Shogun](https://github.com/killabayte/shogun) plan as a **pair
session** in Claude Code. Claude implements each step in your normal environment, Codex
reviews every step and the final change independently, and you are asked whenever access,
a decision or missing information is needed.

It is a Claude Code skill (`/niten`) with one script and four hooks. Shogun plans the work
with two models; Niten carries it out with the same two models in fixed roles.

## How it works

- **Claude is the executor.** It follows the plan step by step: code changes, commands,
  operations outside the repository (registries, cloud CLIs, infrastructure tools), with
  your tools and your network.
- **Codex is the reviewer.** After each step Claude commits and runs `niten.py review`,
  which calls `codex exec` in a read-only sandbox, without your Codex configuration,
  rules, MCP servers or plugins. Codex sees the plan's step, only this step's changes, the
  commands that actually ran and Claude's evidence, and returns a structured verdict: each
  acceptance criterion as met, not met or not verifiable, every open earlier finding
  settled by its id as addressed, not addressed or withdrawn, new findings with a severity
  and the exact fix, and what it chose not to judge. A step passes only on `approve` that
  judges every criterion in scope, leaves none unmet, raises no blocker or major finding
  and settles every open finding, with no blocker or major one left unaddressed; otherwise
  Claude fixes and asks again. A failed, malformed or stale review never counts as
  approval, and a verdict is discarded if the repositories moved while the reviewer
  worked: their commits, or the content of their working trees, including files that were
  already modified before the session. The working tree is compared byte for byte (the
  index by blob id, every tracked and untracked file read from disk, and the repository's
  own git configuration), so no diff driver, filter or textconv can hide a change, and a
  git call that fails stops the decision instead of looking like "no change". A change to
  a repository's git configuration during the session stops the reviews until you decide. A final review checks the whole change against every criterion of the plan
  before anything is delivered; what nobody could verify, in a step or at the end, goes to
  you to check and confirm.
- **You are in the loop.** Claude asks when it needs a login, a permission, a decision the
  plan leaves open or information it cannot find. After three unapproved reviews of one
  step it stops and asks you how to go on.
- **Evidence is recorded, not claimed.** A hook logs every command Claude runs, with its
  exit code and output (secrets masked). The reviewer trusts that log over Claude's
  account, which matters for operations it cannot repeat without network access.

## What is enforced

The hooks make the key rules hold whatever the model does. They act only in the Claude
Code session that started a Niten session and do nothing anywhere else.

| Hook | Rule |
|---|---|
| `Stop` | Claude cannot end its turn while the current step, or the final review, is not approved, unless it is waiting for your answer or a review is running. After three blocks in a row without any progress the turn ends with a message to you, so a broken reviewer cannot trap the session in a loop. |
| `PreToolUse` | No `git push` or pull request before the final review approved exactly the commits being delivered; `finish` refuses unless the commits and the working trees are what the final review approved. |
| `PreToolUse` | Every important action is put to you, with Claude's explanation of what it does and why the current step needs it, even where your permission settings would allow it. A command without an explanation is refused. |
| `PreToolUse` | The session's state, command log and reviews, the approved plan and its receipts, and Claude's settings cannot be edited by Claude; Claude cannot run the hooks itself, and Claude's own directory `~/.claude` (with the key that signs the log) is out of reach: no shell command may name it, glob over hidden names or sweep the home directory, and the file tools cannot read Niten's files there. |
| `PostToolUse`, `PostToolUseFailure` | Every Bash command, failed ones too, is logged with its exit code and output, and so are your answers to Claude's questions, as the reviewer's evidence. Each entry is signed; a review refuses a log with an entry the hooks did not write. |
| `UserPromptSubmit` | Your messages are logged too, so a deviation from the plan counts as decided only if you decided it; a pause to wait for you ends when you answer. |

Important actions: git pushes and history rewrites; changes to git's user or system
configuration; image pushes, registry logins and
image removal; any `aws` operation that is not describe, list or get; Terraform, `kubectl`
and Helm changes; pull requests, releases and repository changes through `gh`; writes to
web APIs with `curl`/`wget`; recursive forced deletes; package publication;
`ssh`/`scp`/`rsync`; `sudo`; Niten overrides (`--unapproved`, `--restart`, `--abort`,
another review after the limit); MCP tools that change another system; file writes
outside the plan's repositories. Read-only commands pass without an extra question. Add
your own patterns in `~/.claude/niten/config.json`:

```json
{"ask": ["\\bmake\\s+deploy\\b"]}
```

Your own deny rules still win: a command they block cannot be approved through Niten.
The hooks guard only a Niten session started (or attached) inside the Claude Code session
that runs it; outside one they do nothing, except that starting a session without
approval or over an earlier one is put to you.
Everything else (scope, the quality of the evidence, asking instead of guessing) is the
skill's instruction to the model, backed by the reviewer, which rejects out-of-scope or
unproven work. Commands hidden in scripts are not classified; the reviewer sees them in
the log. The checks are pattern-based and the agents run as your own OS user: a
deliberately obfuscated shell command (a name assembled at run time, an encoded script)
can slip past them. Plain commands, quoting tricks, globs over hidden names and sweeps of
the home directory are caught; the signed log and the final review are there for the rest.

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

or ask Claude to execute a plan or a ticket that has one. Claude checks the plan's
approval, prepares a work branch per repository with your confirmation, starts the
session and goes step by step. At the end it asks how to deliver (push and pull request,
or local branches only).

## The script

`skills/niten/scripts/niten.py` keeps the session state and runs the reviewer; Claude
calls it, you rarely need to.

| Command | What it does |
|---|---|
| `start --plan P [--repo NAME=PATH] [--restart] [--unapproved]` | Checks the approval, records each repository's base commit and pre-existing changes, registers the session for this Claude Code session; `--restart` archives an earlier session |
| `status` | Steps, their review counts, what is next |
| `review S-NNN [--user-approved DECISION]` | Codex reviews the current step's committed changes |
| `final [--user-approved DECISION]` | Codex reviews the whole change, after every step is approved |
| `confirm "WHAT"` | You checked what the final reviewer could not verify |
| `pause "reason"` / `resume` | Wait for you without the Stop hook blocking |
| `attach --state DIR` | Continue a session in a new Claude Code session |
| `finish [--abort]` | End the session |

Repositories are found by `--repo` (by name or `repo-N` alias), then the plan's manifest,
then `$NITEN_WORKSPACE/<name>`, then `<cwd>/<name>`.

State lives next to the plan in `<plan>.niten/`: `state.json`, `commands.jsonl` (the
command log), `evidence/<step>.md`, and `reviews/` with every reviewer prompt, verdict,
duration and token usage.

Settings (environment): `NITEN_REVIEW_MODEL` (default `gpt-6-astra`), `NITEN_REVIEW_EFFORT`
(`high`), `NITEN_REVIEW_TIMEOUT` (seconds, `1800`), `NITEN_CODEX` (default: Shogun's
`codex_command`, then `codex` on PATH), `NITEN_WORKSPACE`.

## Tests

```
python3 -m unittest discover -s tests
```

The tests use temporary repositories and a scripted Codex; no model is called.

## Prior art

Ideas were taken from projects that pair Claude Code with a second reviewer:
[openai/codex-plugin-cc](https://github.com/openai/codex-plugin-cc) (a Stop-hook review
gate, and the loop failures it ran into), [obra/superpowers](https://github.com/obra/superpowers)
(plan execution with fresh verification evidence and scoped re-reviews),
[claudex-loop](https://github.com/chaseai-yt/claudex-loop) (approvals bound to the plan's
hash; the builder never grades its own work) and
[codex-review](https://github.com/JustinTervala/codex-review) (a ledger of findings that
re-reviews settle). Niten combines them with per-criterion verdicts, a harness-recorded
command log and hook-enforced approvals for outward-facing actions.

## History

Niten started as a Go engine that ran plans inside a sealed sandbox with certified CLI
profiles (tag `go-engine-v0.1`). Sealing the agents off from the network, credentials and
other repositories made it unable to carry real work end to end, so it was replaced by
this skill: the same executor and reviewer, in your environment, with you approving
access instead of the sandbox refusing it.

## License

MIT
