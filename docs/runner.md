# P2 Runner: Isolated Work and Recovery

Status: implemented offline, 2026-10-01. This records what the P2 packages do, so the
[architecture](architecture.md) and the code stay aligned. Nothing here calls a model:
the adapters are exercised with fake CLIs, and `niten doctor --live` stays refused until
the live probes are separately authorized. The engine that drives these pieces (`run`,
`status`, `resume`) is P3.

## Packages

| Package | Responsibility |
|---|---|
| `internal/store` | Run lock, durable event journal, write-once artifacts, atomic state |
| `internal/workspace` | Owned clone, inspection, candidates, rejected snapshots, verification copies |
| `internal/verify` | Checks in new roots under the sandbox, sealed before reading, evidence records |
| `internal/provider` | Process supervisor, environment filter, Claude and Codex adapters, settings renderer |
| `internal/attempt` | The durable attempt protocol and crash recovery |
| `internal/procinfo` | Kernel process identity on macOS: start time, parent, process group |
| `internal/holders` | Processes holding a directory tree (lsof, failing closed on an incomplete listing) |

## Run store

- **Lock.** `OpenRun` takes an `flock` on `<run>/lock`. A second coordinator gets
  `ErrLocked` with the holder's recorded PID, start time and host. The kernel releases
  the lock when the holder exits for any reason, so a crashed coordinator never leaves a
  lock that would have to be broken by guessing; the owner record is never used to
  signal a process.
- **Journal.** `events.jsonl` holds one JSON event per line with `seq` starting at 1.
  `Append` writes and syncs before it returns. On replay an unterminated last line,
  left by a crash during a write, is moved to `events.tail-<n>` and cut off; any other
  unreadable, unknown-field or out-of-sequence line is `ErrCorrupt` and stops the run.
  After a failed write or sync the journal refuses every further event in that process.
- **Artifacts** are written once (temp file, sync, link, directory sync) before the
  event that references them, with their SHA-256 in the event. `state.json` is a
  projection replaced atomically; the journal is the source of truth.
- **Fault injection.** Write and sync hooks let tests simulate a full disk and failing
  syncs: a failed artifact leaves nothing, a failed state save keeps the old state, a
  failed event breaks the journal.

## Owned clone

- `CreateClone` runs `git clone --no-local --no-checkout --template= --separate-git-dir`:
  objects are copied (no alternates), no hooks are installed, the remote is removed,
  hooks are disabled in the config, and the base is checked out on branch `niten`.
  The worktree's `.git` is a pointer file; the git directory is private and outside the
  worktree. Every later command runs with an explicit git dir and work tree,
  `--no-replace-objects`, `--no-lazy-fetch` and hooks disabled, and with the user's
  global and system configuration, system attributes and the user's attribute and
  exclude files ignored. The only configuration is the clone's own, so attributes a
  candidate writes can name no filter, diff or merge program that git would run.
- `Inspect` snapshots the whole worktree through a private index in the git directory
  (the real index is not touched) and classifies every change against HEAD. Hard
  violations: a protected path, an instruction path without an explicit plan target, a
  symlink leaving the repository, a nested repository or submodule, a rewritten `.git`
  pointer, and changed git metadata (HEAD, config, refs, info, hooks, alternates,
  fingerprinted after the coordinator's last commit). Changed metadata or a rewritten
  pointer stops the inspection before any git command reads the worktree. Inspect also
  walks the physical worktree, independent of ignore rules: a protected or instruction
  path that git ignores, or a nested `.git` directory, is a hard violation; other ignored
  files are listed and are not part of the candidate. Symlinks, the committed ones of the
  snapshot and the ignored ones of the worktree, are resolved on the worktree's own file
  system, component by component with `Lstat`, so its name semantics apply (APFS treats
  `ALIAS` as `alias` and NFC as NFD), and every symlink is followed before a later `..`
  is applied: `alias -> .` with `escape -> ALIAS/../x` leaves the repository even though
  an exact or lexical comparison would say otherwise. An absolute target, a resolution above the root or a
  loop is a violation, also for an unchanged symlink that starts to escape because another
  symlink changed. Where a symlink resolves depends only on the set of symlinks and the
  file system's name lookup, so a base commit's own outside symlink is tolerated only while
  that set (snapshot plus ignored symlinks) is exactly the base's; once any symlink is
  added, removed or retargeted, every escaping symlink is a violation, the base's included. Paths outside the plan targets are
  off-target; they are allowed for review, not rejected here.
- `Commit` refuses any violation (there is no partial commit of the permitted part), an
  empty snapshot and a HEAD that moved since the inspection; candidates are committed by
  `Niten <niten@localhost>`. `SaveRejected` keeps a rejected snapshot under
  `refs/niten/rejected/<name>` without moving HEAD; `Restore` returns the worktree to
  HEAD exactly. `Materialize` writes a candidate into new roots byte for byte from its
  blobs (`git cat-file --batch`), with no smudge-side conversion (`ident`, end-of-line,
  working-tree encoding) and no filter, and `SourcesChanged` compares raw bytes
  (`hash-object --no-filters`), so an attribute cannot hide a change to the code under
  test.
- Repository instructions for the next invocation come from the base commit copies that
  `prepare` stored, never from the clone, and the CLIs are told not to load them
  (`--safe-mode` for Claude, `project_doc_max_bytes=0` for Codex), so a candidate's edit
  to an instruction file never instructs the next call.

## Verifier

`Verifier.Run` gives every check new roots under its attempt directory and refuses a
root that already exists. It materializes the candidate commit, runs the check under the
Seatbelt backend with an argv resolved only inside the declared toolchains and an
explicit environment, and always calls `Seal`. Outputs are read only from the sealed
paths:

| Status | When |
|---|---|
| `unknown` | the sandboxed run failed (a holder outside the attempt's group, a scan error, an unavailable backend), or `Seal` failed |
| `invalidated` | the check edited or removed the code under test, added a file to the source tree that is not a declared output, or group members outlived it |
| `failed` | timeout, wrong exit code or signal, truncated output, an unmet stdout expectation |
| `passed` | none of the above |

Build caches and temporary files live in the scratch root, so a new file in the source
tree is code the check added; a check declares the outputs it may create there (a
coverage profile, for example) as patterns. The evidence is schema-validated, stored with
both streams as artifacts and announced by
a `check.recorded` event. If the store refuses any write, `Run` returns an error and
records nothing. The environment digest leaves out the per-attempt paths, so equal
environments give equal check keys.

## Supervisor

`Supervise` starts a CLI as the leader of a new process group, delivers the prompt on
stdin with EOF only after the start was recorded (so a CLI started in the window before
`attempt.started` has no task to work on), and drains stdout and stderr concurrently into
files it creates
exclusively, so a stream of another attempt can never be read as this one. Limits: one
JSONL event (16 MiB by default) and each stream (64 MiB). Cancellation and the deadline
send TERM to the group and KILL after a grace period; after exit the group is killed and
waited out. The process identity (PID, kernel start time, group) is handed to `OnStart`
before the supervisor waits; if it cannot be recorded, the attempt is killed. A stream
write failure is an error, never a clean outcome.

`TerminateRecorded` signals a recorded group after a crash only while its leader is
alive with the recorded start time. A reused PID, a dead leader or an unreadable process
table is never grounds for a signal; members left in a dead leader's group are reported.

`FilterEnv` removes API keys and model overrides (`ANTHROPIC_*`, `OPENAI_*`,
`CLAUDE_CODE_*`, `CODEX_API_KEY`, `RUST_LOG` and the other built-in names) plus the
configured `strip_env` names, and returns only the removed names for the record.

## Adapters

- **Claude executor.** The argv is the executor profile of [P0 profiles](p0-profile.md):
  `--restricted --safe-mode`, the six executor tools, `acceptEdits`, the rendered
  settings, no MCP, no slash commands, no session persistence, optional
  `--max-budget-usd`. A result needs the init event, a result event, a clean exit and
  schema-valid structured output. The reported model must equal the requested id
  exactly (aliases are not accepted); the permission mode must be `acceptEdits`; any tool
  outside the profile or any delegation is a protocol violation. Claude does not report
  effort, so it is recorded as `unknown`.
- **Codex reviewer.** The argv is the reviewer profile (`-s workspace-write` with the
  network off, user config and rules ignored, agents and the listed features disabled,
  project docs off, a clean launcher directory). A result needs `turn.completed`, a clean
  exit, a session record with the requested model, effort and approval policy and a
  restricted network, and an output file that did not exist before the attempt and was
  written during it.
- **Failure classes:** transport, payload, rate_limit, config, refusal, protocol,
  timeout, canceled. A failed attempt is never a result.
- **Settings.** `RenderSettings` fills `examples/claude-settings.template.json` with
  clean absolute paths, removes every context-copy entry in single-repository runs
  instead of substituting an empty path, refuses unknown or unresolved placeholders, and
  adds Edit/Write deny rules for the policy patterns. `contract.Bundle` makes a payload
  schema self-contained for `--json-schema` and `--output-schema`.

## Attempt protocol and recovery

One attempt writes, in order: the prompt artifact; `attempt.intent` (role, argv and its
digest, prompt digest, environment names, removed names, stream paths); the stream files
and the process; `attempt.started` with the identity, before waiting; the outcome and
result artifacts; `attempt.finished` with both digests. A store failure before the start
means the model is never started.

`Recover` resolves every attempt of the replayed journal and records its answer, so it
is not repeated; an interrupted recovery reuses its own identical artifact:

| Journal state after a crash | Resolution |
|---|---|
| `attempt.finished` present | `finished`; both artifacts must match their digests |
| started, outcome artifact saved | `finished`, completed from the saved outcome (and result, or a parse against the saved outcome); a known failure stays a failure |
| started, outcome or result artifact present but unreadable | recovery stops with `ErrCorrupt`; an unreadable record is never treated as absent |
| started, no outcome, recorded process alive with its recorded start time | the group is stopped, then as below |
| started, members of a dead leader's group remain | `outcome_unknown`, processes reported, never signalled; the run is blocked |
| started or intent, the attempt's directory is held by any process, or the holders cannot be listed | `outcome_unknown`, blocked until nothing holds it |
| started, no outcome, nothing holds the directory, complete result in the saved stream | `recovered` without a new model call; the result notes that the exit status is unknown |
| started, no outcome, incomplete or empty stream | `outcome_unknown` |
| intent only, stream files exist | `outcome_unknown`: the prompt is delivered only after `attempt.started`, so the stream is never a result |
| intent only, stream files never created | `not_started` |

A torn `attempt.finished` line falls into the "outcome artifact saved" row: the tail is
kept aside by the store and the saved outcome decides.

## Doctor

The offline doctor also reports the configuration source, the git version, the store
location and whether it is private (without creating it), and for `claude`, `codex` and
`shogun` the resolved path, real path, binary digest and version, plus whether Shogun
supports `verify --require-manifest`. It runs only `--version`, `version` and `help`.
