# P0 Profiles and Verification

Status: concrete hypothesis, 2026-09-30. Help/version output and the official
documentation were checked; no model calls were made. This is the first P0 gate
before P1 and the main part of P2, not a certificate of a working sandbox.
P0a covers one project and three process profiles: executor, reviewer and the
coordinator-owned verifier. Only the first two involve models. P0b adds context
repositories separately; it is not needed for the single-project pilot.

## Verified local facts

In the shell of the current session Claude is an executable, version 2.1.285. Codex
also resolves through PATH to the VS Code extension executable, version 0.155.0-alpha.16.3.
The global Shogun config selects a different executable:
`/Applications/ChatGPT.app/Contents/Resources/codex`, version 0.155.0-alpha.16.4.
Go is 1.26.3 darwin/arm64. A shell function is possible in another shell, but here
the claim "codex exists only as a function" was not confirmed.

The Shogun certificate that was read, `94c4ccfda50c3a1f.json`, created 2026-09-29,
contains Claude 2.1.284 and Codex alpha.16.4. It does not cover the new write profiles;
even matching versions would not replace the Niten probe. Codex help/version under the
current sandbox warned that PATH aliases could not be created, but exited with 0.

## Executor profile

The argv shape; the harness substitutes real paths and the schema. The implementation
uses an argument array, not shell interpolation:

```text
claude -p --output-format stream-json --verbose
  --model claude-opus-5-5 --effort xhigh --json-schema <JSON>
  --restricted --safe-mode --tools=Read,Grep,Glob,Edit,Write,Bash
  --permission-mode acceptEdits --permission-prompts none
  --allowedTools "Bash(go test:*)" "Bash(go build:*)" "Bash(go vet:*)"
                 "Bash(git diff:*)" "Bash(git log:*)" "Bash(git blame:*)"
  --settings <host-owned-settings.json>
  --strict-mcp-config --disable-slash-commands --no-session-persistence
```

The local help describes the file tools/settings restrictions under `--restricted`,
and the disabling of CLAUDE.md/customizations under `--safe-mode`. Agent, REPL and other
delegation facilities are excluded. `--bare` is not used: the help describes it as
disabling the standard OAuth/keychain. Safe-mode keeps authentication.

The [settings template](../examples/claude-settings.template.json) is the input of a
future renderer, not ready-to-run runtime JSON. Placeholders are replaced with canonical
absolute paths; an unresolved placeholder, an unknown field or an ambiguous path
blocks the launch. For P0a the renderer removes the optional CONTEXT_COPY rules
as whole entries before substitution; it never substitutes an empty path or a
wildcard. P0b renders them with an explicit read-only context copy and adds
`--add-dir`. In file permission rules `/{{ABS_PATH}}` becomes
`//absolute/path`; the sandbox filesystem uses the ordinary `/absolute/path`.

The renderer additionally expands the protected/instruction paths from the policy,
including nested and not-yet-existing config paths and symlink destinations,
into deny rules for shell and file tools. It closes off neighbouring role workspaces and
the writable shared temp, leaving only the scratch of the current attempt. If the runtime
does not allow excluding the shared temp, the gate fails. The template without the renderer
does not guarantee this. Settings live outside the model write roots and have their own digest.

Sandbox enabled, failIfUnavailable, the disabled unsandboxed fallback and an empty
excludedCommands are mandatory. Their combined effect under safe-mode and managed policy
is verified by the probe. Bash prefix rules are not an isolation boundary. `--add-dir`
also does not mean read-only: context copies receive Edit/Write/Bash write denials.
Basis: [official sandbox documentation](https://code.claude.com/docs/en/sandboxing).

The gitdir is placed outside the source root and outside the denyRead store via `git clone --separate-git-dir`, with no
shared alternates and no writable hardlinks to the original metadata. The `.git` pointer
is protected. The coordinator commits with explicit GIT_DIR/GIT_WORK_TREE and hooks disabled.
A read-only gitdir does not forbid every change to working files by git commands;
the admissibility of the full tree is checked separately.

## Reviewer profile

Codex is launched from a clean `<review>/launcher` without candidate-controlled ancestors.
The sources are materialized in `<review>/launcher/source`, without a writable gitdir.
The candidate's nested `.codex` is not in the config search chain from the launcher cwd.
Inputs are passed explicitly; the launcher is recreated before each new attempt.

```text
codex exec --json --output-schema <schema.json> -o <host-result.json>
  -m gpt-6-astra -c model_reasoning_effort=xhigh -c approval_policy="never"
  -s workspace-write -C <clean-launcher> --skip-git-repo-check
  --ephemeral --ignore-user-config --ignore-rules
  --disable apps --disable browser_use --disable browser_use_external
  --disable computer_use --disable image_generation --disable multi_agent
  --disable goals --disable hooks
  -c agents.enabled=false -c project_doc_max_bytes=0
  -c mcp_servers={} -c plugins={} -c web_search="disabled"
  -c sandbox_workspace_write.network_access=false
  -c sandbox_workspace_write.exclude_slash_tmp=true
  -c sandbox_workspace_write.exclude_tmpdir_env_var=true
  -
```

TMPDIR/GOCACHE/GOTMPDIR live in the launcher scratch. The host result lies outside the
tools' write roots and is created by the CLI as an output artifact. The reviewer may modify
its own copy for investigation; it never becomes a new candidate.
The result of checking a modified copy does not certify the original SHA. Context copies,
the original repo and the store are not part of the writable roots.

The adapter enables the diagnostic `RUST_LOG=codex_exec=info,codex_core=info`, as in
the studied Shogun, and verifies the actual model/effort/policy from the session
record. Missing required diagnostics are not replaced by values from argv.

`--ignore-user-config` does not mean project config is disabled; the trap check
with `.codex/config.toml` and hooks is mandatory. We do not mix `-s workspace-write`
with `default_permissions` as if the restrictions were additive: the precedence is described
in the [OpenAI Docs](https://learn.chatgpt.com/docs/permissions). If narrower
filesystem rules are needed, we pick one custom profile and re-verify the whole launch.

## Coordinator verifier profile

Mandatory check_spec commands are run by Niten itself. They do not inherit either
CLI sandbox, and a disposable directory alone gives no isolation. The v0.1 macOS
backend directly launches the fixed `/usr/bin/sandbox-exec` executable:

```text
/usr/bin/sandbox-exec -f <host-owned-verifier-profile.sb> <absolute-check-command> <argv...>
```

The coordinator remains outside this sandbox to own logs/state. The child receives
only the required stdio pipes, cwd and an explicit environment allowlist. No
repository hook or model can select a different launcher/profile or bypass it.
Every descendant inherits the sandbox; timeout tears down the process group.

The generated SBPL profile starts from default deny. It permits only the process,
loader and system runtime operations needed by the pinned toolchain, reads of
the verifier source tree and declared toolchain/immutable dependency paths, and
writes to verifier source/scratch. It denies networking including loopback and
DNS, reads of home/credentials/store beyond explicitly required public toolchain
paths, and writes to original/executor/reviewer trees, store, gitdir and shared
caches. Toolchain paths must be narrow resolved paths, never the whole home.
Symlinks, aliases and child processes receive the same restrictions. Denied
reads/writes remain denied if a test invokes a shell or helper executable.

The check environment is explicitly constructed (PATH/locale and role-local
GOCACHE/GOTMPDIR/TMPDIR/GOMODCACHE as needed), not inherited and then partially
redacted. For Go fixtures use GOENV=off, GOTOOLCHAIN=local and GOPROXY=off; dependencies
must already be supplied. No production/provider credentials or ambient shell
startup files are available. Tests requiring broader access fail with a clear
unsupported-environment reason; no unsandboxed retry or automatic human waiver.

P0a must build and validate the actual generated profile, not just this policy
specification. The backend must refuse missing binaries, unsupported OS/profile,
invalid policy or a failed certificate before executing untrusted checks.
A plain exec fallback is forbidden. The profile digest, OS build, resolved
launcher/toolchain paths and environment policy join each check's evidence key.
The verifier certificate is independent of the two model certificates.

Local evidence: `/usr/bin/sandbox-exec` is present; its installed man page marks
it deprecated and documents `-f`. Availability is not a successful isolation
probe. Apple's [explanation of the unsupported SBPL interface](https://developer.apple.com/forums/thread/661939)
means this is a consciously macOS-specific backend tied to a tested OS/profile,
not a portable or stable Apple API promise. A runtime failure blocks verification
until an explicitly chosen alternative is implemented and tested.

### Offline verification results, 2026-09-30

The verifier backend and its harness are implemented in `internal/verify/sandbox`
and exercised by `go test ./internal/verify/...` on macOS 26.6.2 (25G83), arm64,
Go 1.26.3. No model was called. What the run established:

- A default-deny profile must allow reading the root directory itself, `(literal "/")`.
  Without it dyld aborts every process with SIGABRT and no message. Read access to
  `/usr`, `/bin`, `/sbin`, `/System`, `/Library`, `/private/etc`, `/private/var/db`,
  `/private/var/select` and `/dev`, plus `file-map-executable` on the same paths,
  is sufficient for the Go toolchain with `CGO_ENABLED=0`.
- `mach-lookup` is restricted to `com.apple.system.opendirectoryd.libinfo`. A blanket
  allow would let a test reach the network through XPC daemons such as nsurlsessiond.
  `go build`, `go test` and `go vet` work under the restriction; the only logged
  lookups are `com.apple.logd` and `com.apple.diagnosticd`, and both are harmless.
- Positive controls: `go build`, `go test` and `go vet` of a dependency-free fixture
  with GOCACHE, GOTMPDIR and GOMODCACHE under scratch. Negative controls, attempted
  from inside a test binary and its `/bin/sh` child: writes outside the roots, into a
  fake store, a gitdir and shared `/tmp`; reads of a store canary and of a fake
  `~/.claude/credentials.json`; a symlink escape; a loopback TCP connect to a host
  listener; a DNS lookup. All were denied and the host snapshot of the neighbouring
  directories was unchanged. The kernel log confirms `network-outbound` denials for
  the loopback port and for `/private/var/run/mDNSResponder`.
- Harmless denials that appear in every run and need no allowance: writes to
  `/dev/dtracehelper`, `ipc-posix-shm-read-data apple.shm.notification_center` and
  reads of the real home's `.CFUserTextEncoding`.
- The go command's telemetry child cannot start in the sandbox. In a fresh scratch home
  the first `go` command (Go 1.26.3) prints `can't start telemetry child process:
  fork/exec .../bin/go: operation not permitted` and goes on; its exit status is
  unchanged, and later commands in the same scratch print nothing. No child escapes the
  process group. The line is a notice, not the reason for a failed check.
- Denials are observable without extra tooling: `/usr/bin/log show --style compact
  --predicate 'eventMessage CONTAINS "deny"'` lists `Sandbox: <process> deny(1) <op> <path>`.
- `sandbox-exec` worked while the coordinator itself ran inside Claude Code's Bash
  sandbox; nesting was not an obstacle on this machine.
- The timeout kills the whole process group and a `sleep` grandchild does not
  survive. The launcher must be the root-owned `/usr/bin/sandbox-exec`; a relative
  argv[0], a cwd outside the roots, an inherited environment or a profile directory
  inside a writable root are refused before anything starts.
- Review finding, fixed the same day: a descendant that calls `setsid` leaves the
  child's process group and survived the group kill while keeping the profile, so
  it could keep writing inside the roots after `Run` returned with exit 0. macOS has
  no process namespaces. The final design after three review rounds:
  - The child is the leader of a new process group, and the profile denies `setsid`
    and `setpgid` (`(deny syscall-unix (syscall-number SYS_setsid SYS_setpgid))`), so
    a descendant created with fork stays in the group. After the child exits the
    whole group is killed and `Run` waits until the group is gone; a group that
    survives `SIGKILL` refuses the run, and `Stragglers` marks a run whose group
    still had members. A run with stragglers is not evidence.
  - Membership in that group is the only proof that a process belongs to the
    attempt. A start time, a parent chain or orphaning to launchd do not prove it:
    the coordinator may start an unrelated process during the attempt (third
    review). `posix_spawn` with `POSIX_SPAWN_SETSID` or `SETPGROUP` still leaves the
    group, because the syscall filter does not see attributes applied inside
    `posix_spawn`; such a descendant is likewise not provably the attempt's.
  - After every run `/usr/sbin/lsof` lists the holders of the roots (open file,
    cwd, mapped binary). Members of the group are killed with the group. Every
    other holder is never killed: it is reported in `ForeignPIDs` and the run is
    refused. An incomplete listing (any non-zero `lsof` exit) also refuses the run,
    because an open descriptor keeps writing into a renamed tree (second review).
  - `Seal` renames both roots to sibling paths that no profile permits, so nothing
    can open anything in the trees afterwards, and lists holders again. It kills
    nothing: the attempt's group is already gone, so every holder is foreign and
    refuses the seal. Outputs are read from the sealed paths only, and a refused
    seal means the trees are not evidence.
  - Residual risk: a descendant that escaped through `posix_spawn` attributes
    survives, still under the profile (no network, no protected reads, writes only
    to the attempt's original root paths). If it holds the roots it refuses the run;
    if it holds nothing it is invisible. P2 must therefore give every attempt new
    root paths and seal them before reading outputs, so such a process never sees a
    later attempt's trees.
  - Regressions cover the denied `setsid`/`setpgid`, an orphan that stays in the
    group and dies with it, a `posix_spawn` escape that is refused but not killed,
    a holder that existed before the run, a sibling the coordinator starts during
    the run, an incomplete listing, a sealed path written under the attempt's
    profile, and the `kinfo_proc` layout the process group is read from. A test
    command that creates sessions or process groups fails inside the verifier;
    that is a v0.1 limitation.

This closes the offline verifier gate of P0a. The executor and reviewer profiles
remain unprobed, and the seatbelt result is bound to the OS build above.

## Budget and CLI capabilities

The local Claude help has `--max-budget-usd`. This is an additional cap on a single
invocation by the CLI's accounting, not a shared budget of the model pair. Its behaviour on
subscription auth and the overshoot by the last response are not yet verified; a list-price
equivalent does not mean a charge against the subscription:
[CLI reference](https://code.claude.com/docs/en/cli-reference),
[cost accounting](https://code.claude.com/docs/en/costs).

`--max-turns` is not listed in the local help, but the official CLI reference
describes it for print mode. We do not accept the claim "it is only an SDK option".
In v0.1 we do not rely on it; absence from the help does not prove absence of support.

Builtin child-env filtering covers ANTHROPIC_*, OPENAI_*, CLAUDE_CODE_*, GIT_* and
CODEX_API_KEY, model overrides and the user's strip_env. The adapter then sets
its own verified values. In particular, CLAUDE_CODE_SUBAGENT_MODEL,
CLAUDE_CODE_EFFORT_LEVEL and ANTHROPIC_MODEL are removed. The profile does not let the model
launch additional agentic CLIs through the shell; their tool-network/auth access must
also be closed. Process tools and shell children are checked separately.

## P0a Single-project probes

First prepare a small Go fixture without external dependencies, separate gitdir,
fake original repo/store/credential canaries and isolated scratch per role.
There are no context repository copies in P0a. Real credentials are never copied
into fixtures or logs, and real user projects are not write targets.

Start with the verifier helper entirely offline. Positive controls exercise
allowed reads/writes, compilation and child processes. Negative controls attempt
filesystem escapes, symlink access, secret-canary reads, writes to neighbouring
role roots, shared temp/cache and network connections (including a controlled
local listener). The observer outside the sandbox records syscall failures and
checks that canaries remain intact. Deliberately allow one forbidden canary path
in a separate broken-profile control to prove the detector catches the violation;
removing a rule alone may leave default-deny protection intact. Do not launch models
until the strict verifier profile passes. Each helper has a 30-second deadline;
the offline probe suite has a five-minute deadline after the helper is built.

Then run the executor and reviewer controls sequentially. The harness records
original hashes/permissions and exact binaries, argv, schemas, settings and env
policy. A controlled go-test helper attempts forbidden operations from a child
process even when the CLI permission layer permits go test. Native Edit/Write
are checked separately. No attempted violation means inconclusive, not pass.

| Control | Mandatory observation |
|---|---|
| Verifier positive | Required Go commands and children work within source/scratch |
| Verifier negative | Network, protected reads, outside writes and symlink escapes fail under direct Niten launch |
| Executor positive | Edit/Write inside source and go test/build work |
| File tools negative | Writes outside source and into protected config paths fail |
| Shell negative | Helper cannot write original/store/gitdir/neighbouring workspace/common temp |
| Escape hatch | Sandbox required, no unsandboxed retry or excluded command |
| Git | Diff/log work; metadata/pointer unchanged; coordinator commits after the call |
| Startup injection | Candidate instructions/hooks/MCP/config canaries are not applied |
| Delegation | Agent/collaboration unavailable; no child model sessions |
| Reviewer | Checks/caches work in disposable copy; candidate and other roots remain unchanged |
| Network | Model tool network closed; trusted CLI inference/auth connectivity works |
| Identity | Exact models, subscription auth; Codex effort/policy observed; Claude effort unknown if unreported |
| Supervision | Valid terminal result, complete streams, no remaining descendants |

Proposed live P0a budget: at most two CLI invocations, run sequentially, up to ten
minutes each and twenty minutes total active wall time, with no retries. This is
a revised proposal for a future authorized run, not an increase to a running or
historical budget. Separate user authorization is required. A missing mandatory
event or timeout remains inconclusive and is preserved. A chosen Claude dollar
cap is recorded separately; no shared Codex dollar cap is claimed.

## P0b Read-only context repositories

P0b adds context copies and extra-root controls only when that capability is
needed. Run the same verifier helper offline with context-read positive controls
and context-write/escape negative controls, then test both CLIs with those copies.
Claude adds --add-dir plus explicit file-tool and Bash write denials; Codex does
not add the context copies as writable roots. An inherited P0a pass is insufficient.

Proposed live P0b budget is separate: at most one invocation per model, sequential,
ten minutes each/twenty minutes total, no retry. P0b requires its own authorization;
it is not automatically chained after P0a. Prepare refuses context-repo plans
without this certificate, while the single-project pilot can proceed on P0a.

The harness for the executor and reviewer controls is `niten doctor --live`, described in
[live probe](live-probe.md). The settings template now also denies writes to `/tmp` and
`/private/tmp`, the shared temp this section requires closed.

Certificates cover binaries/versions, policy templates, effective managed policy,
env policy, models/effort, OS and role topology. Concrete generated paths are
validated offline and recorded per invocation; a new temporary directory alone
does not require another paid probe. A changed policy or wider topology does.
P2 certifies changes to its actual launcher rather than inheriting a manual
probe's pass blindly. Crash recovery and semantic review have separate tests.
