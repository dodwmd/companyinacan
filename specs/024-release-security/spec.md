# Feature Specification: Release engineering and supply-chain security (Epic E20)

**Feature Branch**: `epic/E20-release-security` (spec dir `024-release-security`)

**Created**: 2026-11-25

**Status**: Draft (implemented and verified offline; no real release has been run)

**Input**: Epic E20 in `docs/epics/README.md`; SPEC.md section 12 (open-source plan, release and versioning,
footprint); Constitution II (footprint is a CI gate) and VI (open-source hygiene); the CI contract of
`specs/001-project-foundation` (required job names `lint`, `unit`, `contract`, `licences`, `dco`, which this epic
must not rename).

## User Scenarios and Testing

### US1: A maintainer cuts a trustworthy release (P1)
A maintainer tags `vX.Y.Z` on `main`. The pipeline tests, builds static `linux/amd64` and `linux/arm64` binaries of
the nine commands twice (identical checksums), writes SBOMs, builds and pushes the container images, signs and
attests them, and creates a GitHub release with notes, checksums and `THIRD_PARTY_LICENSES`.

**Independent test**: run the `release` workflow with `dry_run=true`: everything builds, nothing is published.

**Acceptance**
1. Only a tag matching `v*.*.*` (strict semver enforced in the first job) or a manual dispatch can start it; a
   dispatch defaults to `dry_run=true`.
2. In a dry run no registry login, push, signature, attestation or release happens and the publish jobs do not start.
3. A tag not on `main` is refused. The `can licences --check` gate and `make test` must pass first.
4. Two builds of one commit give identical archive checksums, else the release stops.
5. Write permissions (`packages`, `id-token`, `attestations`, `contents`) exist only on the publish jobs.

### US2: A user verifies what they run (P1)
Given a release, a user can verify the archive checksums, the keyless signature, the provenance attestation and
the image signature with documented commands, and read an SBOM.

**Acceptance**: SECURITY.md carries `cosign verify`, `cosign verify-blob`, `gh attestation verify` and the
reproduction recipe; every archive has a CycloneDX SBOM covered by the checksums and attestation.

### US3: Pull requests and the weekly schedule are scanned (P2)
Every pull request, `main` and a weekly schedule run govulncheck, `go mod verify`, dependency review (licence
policy), CodeQL for Go, an in-repo secret scan, shellcheck and Dockerfile checks. Not required initially.

### US4: The CI and release surface cannot rot silently (P1)
A contract test fails when an action is not pinned by a 40-hex SHA, a workflow lacks top-level `permissions`, uses
`pull_request_target` or a non-`GITHUB_TOKEN` secret, interpolates event data into a `run:` script, when the
required job names in `ci.yml` change, when a release image or command is not covered, or when the publish steps
leak out of the gated jobs.

### US5: Maintainers can protect `main` (P2)
`scripts/gh-protect-main.sh` prints (default) or applies the branch protection with exactly the five required
checks; `docs/maintainers.md`, a draft CODEOWNERS and SECURITY.md describe the process. It is not applied by
anything in the repository.

## Requirements

- **FR-001** `.github/workflows/security.yml`, never required, `pull_request` only (no `pull_request_target`), weekly cron.
- **FR-002** `.github/workflows/release.yml` as in US1; keyless cosign; `actions/attest-build-provenance`; GHCR push.
- **FR-003** `scripts/`: `build-release.sh`, `check-reproducible.sh`, `gen-sbom.sh`, `resolve-base-digests.sh`,
  `release-notes.sh`, `bump-version.sh`, `scan-secrets.sh`, `lint-shell.sh`, `lint-dockerfiles.sh`,
  `gh-protect-main.sh`; each with a self-test where it has logic.
- **FR-004** `.github/dependabot.yml`: gomod, github-actions, docker (every `deploy/*/Dockerfile`), weekly, grouped.
- **FR-005** Footprint gate: documented decision (advisory, with the conditions for making it required).
- **FR-006** `docs/releasing.md` (runbook, rollback, advisory flow), `docs/maintainers.md`, `.github/CODEOWNERS`
  (draft), SECURITY.md (supported versions, advisory process, verification commands).
- **FR-007** Contract tests as in US4.
- **FR-008** No secret other than `GITHUB_TOKEN` in any workflow; no third-party token or service.

## Out of scope / not done

macOS and Windows binaries; arm64 container images (the Dockerfiles build natively; cross-building needs QEMU or a
Dockerfile change); Homebrew/apt packaging; a Helm chart repository; signing with a long-lived key; actually
running a release, applying branch protection, enabling repository settings.

## Success criteria

- `make test` green with the new contract tests; `shellcheck` clean on every script.
- A complete local run of `scripts/build-release.sh` plus `check-reproducible.sh` on all 18 archives passes.
- The first dry run of the workflow on GitHub (dispatch only; a pull request cannot start it) is the remaining
  verification, listed in `tasks.md`.
