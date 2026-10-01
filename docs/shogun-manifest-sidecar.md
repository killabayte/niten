# Shogun Manifest Sidecar Prerequisite

Status: accepted design prerequisite S0, 2026-09-30; implemented on the Shogun branch
`s0-manifest-sidecar` (commits `bb3f7e6` and `29d03b3`), not merged yet. Niten's
`prepare` consumes the triplet ([import](import.md)).
This is a scoped Shogun change in a separate PR, before Niten P1 acceptance.
Implementation and offline tests need no model calls. Fresh live plan generation
has a separate authorization and budget.

## Published contract

For `feature.md`, publish:

```text
feature.md
feature.approval.json
feature.manifest.json
```

The third file is the existing versioned Manifest snapshot used for the approved
generation. Do not rebuild it from today's repositories at publication time.
Its recomputed `Manifest.ComputeFingerprint()` must equal the existing receipt's
`manifest_digest`; no receipt schema change is necessary. The sidecar uses the
existing Manifest `version` field. Niten also hashes its full bytes at intake.

This fingerprint covers ordered repo IDs/fingerprints and input IDs/status/hashes/
roles, not the entire JSON. Check the inner repo fingerprints against their full
HEAD and change hashes as well. Root/workspace/origin/exclude fields are not all
bound by the digest and never confer permissions. The receipt stays path-free;
the sidecar contains manifest metadata, including local paths, with private file
permissions. Do not add raw input contents, authentication material or prompts
to this format; existing path/URL metadata remains private.

## Publication and compatibility

1. Determine all three destinations before intake. Add the exact owned manifest
   destination to the output-exclusion handling alongside plan/receipt. Do not
   accept arbitrary exclusions from an unverified sidecar.
2. Freeze the approved generation's manifest and verify its fingerprint against
   the receipt before installing any new publication.
3. Atomically install manifest, receipt, then plan with no-clobber semantics.
   The plan is last. This is recoverable ordered publication, not a three-file
   atomic transaction. A generation is published only when the triplet verifies.
4. Recover either crash gap without model calls. Identical existing bytes are
   idempotent; a differing file, foreign file or symlink is a conflict.
5. Preserve current `shogun verify` compatibility: an old plan/receipt pair can
   still be `valid`. It verifies the approved body and immutable metadata.
   Niten separately requires the matching manifest for strict execution.

Expected Shogun areas: destination helpers near `internal/library/plan.go`,
intake/output exclusions, publication in `internal/pipeline/integrate.go`, and
publication/recovery tests. The existing `writeOnce` fixes the output mode, so
private sidecar permissions need deliberate handling. No general exporter,
extra model step or shared public Go module is required.

## Offline acceptance

- Fast and thorough publication use the matching approved-generation manifest.
- Moving only the triplet retains importability without a run directory or a
  full archive of planning inputs.
- Changed digest components, inconsistent repo fingerprints, truncation,
  unsupported versions and mismatched sidecars are rejected by Niten.
- Exact output exclusions do not hide unrelated source changes.
- Both publication crash gaps recover the same bytes and spend.
- Foreign files and symlinks are never overwritten; private permissions apply.
- Old pair verification stays compatible, while missing metadata never silently
  authorizes execution against an unpinned base.

## Legacy plans and pilot selection

The inspected local archive has four valid receipts but no manifest sidecars.
Both checked workspace/Shogun run stores are empty. A digest, shortened Markdown
SHA or current working tree cannot reconstruct a lost historical manifest.
Preserve those artifacts unchanged. Recover authentic metadata if available;
otherwise approve a new plan after S0.

The three old `stats --json` plans provide a useful two-step task shape, but
current Shogun already implements the feature. Repeating it there may be a no-op.
Use a dedicated clean fixture that genuinely lacks the feature, or a deliberately
pinned historical fixture baseline, and generate a fresh approved plan against
that exact base. Do not overwrite or repurpose old approvals.

S0 makes new publications independent of retained run directories. It does not
supply missing external specifications, dirty-tree patches or untracked-file
bytes needed by future snapshot support.
