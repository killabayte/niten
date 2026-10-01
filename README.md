# Niten

**Niten — Two Skies.** A Go framework for executing approved
[Shogun](https://github.com/killabayte/shogun) plans with two models:

- **Claude Opus 5.5 / xhigh**, via Claude Code, is the primary executor.
- **GPT-6 Astra / xhigh**, via Codex CLI, is the independent reviewer.

Niten manages the processes, working copies, checks, exchange of findings
and recovery after a stop. Opus writes the code; Astra reviews immutable
snapshots in its own disposable copy. v0.1 runs the pair sequentially; parallel
execution remains a later design goal, informed by the first pilot.

The result is a local branch with the implementation and a report that ties the
plan's requirements, checks and final review to a specific version of the code.
Niten sets `done` once every acceptance condition is met. If the work cannot be
finished, the changes, the evidence and the specific reason for stopping remain.
`implemented` separately marks finished code with external criteria still
pending; the user confirms them through attestations. `export` delivers the
result into a new local ref of the source repo.

## Status

Offline P0a and P1, 30 September 2026. `niten prepare` imports an approved Shogun
plan into a prepared run: it verifies the plan triplet, checks the source repository
against the approved base and freezes the execution contract, without calling a model.
`niten version`, `niten help` and an offline `niten doctor` also work; `run`, `status`,
`resume`, `verify` and `export` are present but refuse to run with exit code 2. The
verifier sandbox backend and the message/record contracts are implemented and tested
offline. No model has been called. `prepare` needs a Shogun build with the S0 manifest
sidecar (`shogun verify --require-manifest`). The settings and
structures below describe the proposed interface. These documents have not gone
through a separate Shogun run and are not a plan with its approval receipt.

```text
go build ./... && go test ./...      # offline; the sandbox tests need macOS
go run ./cmd/niten doctor            # checks the verifier sandbox, never calls a model
go run ./cmd/niten prepare PLAN.md --repo repo-1=/path/to/checkout
```

## Documents

1. [Architecture](docs/architecture.md): roles, parallelism, states, the
   completion criterion, recovery and the layout of the Go application.
2. [Shogun contract](docs/shogun-contract.md): importing the existing format,
   integrity, the source repository version and verifiability of criteria.
3. [Roadmap](docs/roadmap.md): small stages with acceptance conditions.
4. [Source research](docs/research.md): what we take from Shogun, revmux
   and ralphex, with links to the studied commits.
5. [Example configuration](examples/niten.toml): proposed values for v0.1.
6. [P0 profiles](docs/p0-profile.md): concrete argv, the settings template and the early probe.
7. [Design review decisions](docs/reviews/2026-09-30-design-review.md): accepted
   decisions and corrections, verified against the local CLIs.
8. [Manifest sidecar change](docs/shogun-manifest-sidecar.md): the scoped Shogun
   publication prerequisite for portable Niten input.
9. [Practical review decisions](docs/reviews/2026-09-30-practical-review.md): local
   plan inventory, verifier isolation and the revised release sequence.
10. [Contracts](docs/contracts.md): the implemented message and record schemas and
    the shape decisions they fix.
11. [Import](docs/import.md): what `niten prepare` checks, the renderer grammar, reason
    codes and the prepared run layout.

## First version

The framework starts as a Go CLI with internal packages; a public SDK is deferred.
v0.1 executes one clean Git repository with an executor/reviewer repair loop,
persisted progress and an optional `--gate-per-step` for human supervision.
Additional read-only context repositories require the separate P0b profile gate.
Model invocations use the existing Claude Code and Codex CLI logins. The store
and working copies live outside the source repository.

The four archived Shogun plans inspected locally all have linear dependencies;
this sample does not justify a speedup claim for parallel execution. The bounded
pilot comes immediately after the sequential P3 slice. P4 parallel scheduling is
outside v0.1 and is selected only after that pilot. Detailed boundaries and later
capabilities are described in the architecture.

## The boundary of the promise

It cannot be guaranteed that two models will solve any plan or find any defect.
What can be built is a verifiable protocol: never declare success without the
required evidence, bound the spend and preserve the work on a stop. That is the
core contract of Niten.

## License

MIT. No third-party project sources were copied during the design stage.
