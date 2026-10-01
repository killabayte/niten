# Niten Design Review Decisions

Date: 2026-09-30. Basis: the review the user passed on. Verified against the local
documents, help/version of the installed CLIs, the saved Shogun certificate and
the official documentation. No model invocations and no execution of a test plan
took place. This is a breakdown of the findings and an update of the design, not
an independent model approval.

This is the first review record. Later release timing, portable input and verifier
profile decisions are superseded by the
[practical review](2026-09-30-practical-review.md); its historical observations
remain unchanged.

## Accepted decisions

| Finding | Decision | Where it is recorded |
|---|---|---|
| Executor profile comes too late | Concrete profile and a separately authorized probe at the start of P0, before P1/P2 | [P0](../p0-profile.md), [roadmap](../roadmap.md) |
| Two roles without subagents | Claude Agent excluded; Codex agents.enabled=false; env overrides removed | [Architecture](../architecture.md), P0 |
| Injection through files | Base-commit instructions, safe-mode, clean Codex launcher, protected config paths, separate instruction diff | Architecture, [contract](../shogun-contract.md) |
| Targets are incomplete | Repo minus protected paths is the hard boundary; Astra evaluates every off-target change; a hard policy violation rejects the whole candidate | Architecture, contract |
| Git metadata | Separate gitdir outside the model write roots, protected pointer, coordinator-owned Git | Architecture, P0 |
| Manual criteria | Human ownership before the run, exact-SHA attestations; implemented separated from done | Contract, roadmap |
| Retrieving the result | niten export creates a missing local ref via CAS; no checkout, merge, remote push or force | Architecture, roadmap |
| Store location | XDG state or store_dir outside the source repos/model write roots | Architecture, [TOML](../../examples/niten.toml) |
| Reviewer may run checks itself | Writable disposable copy; its edits are not imported; the required checks are run by Niten | Architecture, P0 |
| Executable and context repo | Absolute binary paths + doctor resolution; --add-dir is not presented as read-only | Architecture, P0 |
| Terms and checks | executor/reviewer; policy.commands separate from checks.required | TOML, architecture |
| Fresh-session handoff | implementation_notes, remaining_work, risks, decision_refs, evidence refs | Architecture |
| Revise during the next step | The current invocation is preserved; the next one is repair only; continuation after re-review | Architecture, P4 |
| Exit codes | paused=4, failed gate=1, implemented=5, needs_input=3 | Architecture |
| Caches | GOCACHE/temp per role; shared GOMODCACHE only as an immutable snapshot | Architecture |

## Corrections to technical claims

1. In the current shell `codex` resolves to an executable from VS Code, alpha.16.3.
   Shogun is configured with the app executable alpha.16.4. Another shell may have
   a function; the decision is to check the specific executable rather than assume
   a single PATH.
2. The verified certificate `94c4ccfda50c3a1f.json` contains Claude 2.1.284,
   not 2.1.283. The installed version is 2.1.285. None of these read-only
   certificates certifies the new Niten profiles.
3. `--safe-mode` was added to the Claude profile: the local help for `--restricted`
   does not promise that all CLAUDE.md/customizations are disabled. The sandbox
   settings close off fallback and a missing sandbox, not only the denied paths.
4. `--max-budget-usd` exists and is included as an additional Claude setting.
   The behavior of the cap under subscription auth and the size of a possible
   overshoot are not measured yet. It is not a strict shared dollar cap for the pair.
5. `--max-turns` is absent from the local help, but the official
   [CLI reference](https://code.claude.com/docs/en/cli-reference) describes a print-mode
   option. The claim "SDK only" is not confirmed; we do not rely on the flag yet.
6. `--ignore-user-config` and project_doc_max_bytes=0 do not prove that project
   config/hooks are disabled. A clean launcher, explicit feature disabling and a
   canary probe are needed. `-s workspace-write` does not combine automatically with
   custom permissions: [precedence rules](https://learn.chatgpt.com/docs/permissions).
7. A read-only gitdir does not prove that Git commands cannot change the working
   tree. Therefore the full diff is checked regardless of the command name.
8. A single non-force fetch allows a fast-forward of an existing ref. The
   "new branch only" requirement is enforced by a CAS on absence, with idempotence
   only for the same SHA.

## Remaining verification

P0 must prove that settings, safe-mode, subscription auth, file tools, the Bash
sandbox, temp isolation and the review copy work together. The JSON template is
syntactically valid but needs a path renderer and effective-policy checks; it is
not considered ready to run without them. Having the settings in the documentation
is not a passed probe.

The next development step is the offline harness and its verifiable artifacts,
then separate authorization of a specific bounded live probe. This leaves no open
decisions about targets, manual evidence, store and delivery: they are already
accepted in the design.
