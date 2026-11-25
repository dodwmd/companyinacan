# Tasks: Release engineering and supply-chain security (E20)

`[x]` done and verified offline; `[ ]` open (needs GitHub or a registry).

## Phase 1: Spec artifacts
- [x] spec.md, plan.md (constitution check), research.md, data-model.md, contracts/release.md

## Phase 2: Scripts (self-test written with each)
- [x] scan-secrets.sh + allowlist (never prints values; template references pass; allowlist needs a reason)
- [x] lint-shell.sh (shellcheck clean on all scripts), lint-dockerfiles.sh (placeholders dev notice / release error)
- [x] resolve-base-digests.sh (both ARG shapes; every deploy Dockerfile resolves with a fake resolver; garbage digest fails)
- [x] build-release.sh and check-reproducible.sh (all 9 commands x amd64/arm64 = 18 archives, identical twice; `can version` shows tag, commit, commit date)
- [x] gen-sbom.sh (CycloneDX 1.5, deterministic), release-notes.sh (grouped, subjects never executed), bump-version.sh (never pushes)
- [x] gh-protect-main.sh (dry run default, fake `gh`, check list)

## Phase 3: Workflows and config
- [x] security.yml: govulncheck, go mod verify, dependency review, CodeQL, secret scan, shell/Dockerfile lint, release-check (informational), weekly cron
- [x] release.yml: verify, build (twice, SBOM, checksums, notes), images-check, publish-images (sign, attest), publish-release; dry run default
- [x] dependabot.yml, CODEOWNERS (draft), Makefile targets, `/dist/` ignored
- [x] Actions pinned by SHA (tags resolved through the GitHub API)

## Phase 4: Contract tests and docs
- [x] workflows_test.go: SHA pins and tag comments, top-level permissions, no `pull_request_target`, only `GITHUB_TOKEN`,
      script-injection lint (with positive and negative cases), ci.yml job names, protect script list, release shape
      (tag-only trigger, dry_run default, write permissions and publish verbs confined to gated jobs), command and image
      coverage, Dependabot coverage, SECURITY.md content, script self-tests
- [x] docs/releasing.md (runbook, rollback, advisory flow), docs/maintainers.md (protection, footprint decision), SECURITY.md

## Phase 5: Open (cannot be done from a developer machine)
- [ ] Run `release` via workflow_dispatch with `dry_run=true` on GitHub and read the artifacts
- [ ] Cut `v0.1.0-rc.1` as the first real rehearsal: GHCR push, cosign, attestations, `gh release create`
- [ ] Check CodeQL supports the Go version in `go.mod`; check dependency review and Dependabot accept the config
- [ ] Maintainer actions: create the `release` environment with a required reviewer, run `scripts/gh-protect-main.sh --apply`, enable secret scanning push protection and private vulnerability reporting, make the GHCR packages public when releasing
- [ ] arm64 container images (QEMU or `--platform=$BUILDPLATFORM` in the Dockerfiles)
- [ ] Promote selected `security.yml` jobs to required after they are stable; add them to `REQUIRED_CHECKS`
- [ ] Footprint gate: promote only under the conditions in docs/maintainers.md
