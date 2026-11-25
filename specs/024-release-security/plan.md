# Implementation Plan: Release engineering and supply-chain security (E20)

**Spec**: [spec.md](spec.md) | **Research**: [research.md](research.md) | **Contract**: [contracts/release.md](contracts/release.md)

## Summary
Two new workflows (`security.yml`, `release.yml`), ten small POSIX shell scripts with self-tests, Dependabot,
CODEOWNERS (draft), SECURITY.md and two runbooks, and one Go contract test file that pins the hygiene rules.
No product code changes, no new Go dependency, no change to `ci.yml`.

**Language/Version**: POSIX sh, GitHub Actions YAML, Go 1.27 (tests only). **Dependencies**: none added to `go.mod`;
pinned `go run` tools: golangci-lint (existing), govulncheck v1.10.0. **Testing**: shell self-tests (run by
`make contract` through `TestScriptSelfTests`), YAML-level contract tests, shellcheck.
**Constraints**: no real release, no pushes of images or signatures from a developer machine.

## Constitution Check (before research)

| Principle | Assessment |
|---|---|
| I Spec-first | Implements SPEC.md section 12 (release, versioning, OSS plan). No SPEC divergence. |
| II Footprint | No runtime change. The footprint CI gate decision is recorded: it stays advisory, with the conditions to promote it (docs/maintainers.md, research R9). The deterministic RAM declarations remain checked by contract tests. |
| III Test-first | Each script has a self-test with a negative case (secret found, unpinned FROM, dirty tree, bad digest, dry run never calls `gh`); the contract tests were written against the intended rules and fail on a violation (verified by temporarily breaking a pin). The workflows themselves cannot be run offline: stated openly. |
| IV Stable contracts | Required job names `lint unit contract licences dco` unchanged and asserted by a test; release artifact names documented in `contracts/release.md`. |
| V Secure and private | Least-privilege permissions, write scopes only on gated publish jobs, `pull_request` only, no secrets besides `GITHUB_TOKEN`, no event data in shell, keyless signing (no key to leak), SHA-pinned actions, secret scanning, dependency and vulnerability scanning. |
| VI Open-source hygiene | Apache-2.0; no new dependency; licence gate (`can licences --check`) in the release path; SECURITY.md and CODEOWNERS; DCO untouched. |
| VII Simplicity and measured claims | In-repo scripts instead of extra services; reproducibility is measured (18 archives compared), not asserted; unverified items listed. |

## Project Structure

```text
.github/workflows/security.yml   release.yml        (new; ci.yml untouched)
.github/dependabot.yml  .github/CODEOWNERS
scripts/  build-release.sh check-reproducible.sh gen-sbom.sh resolve-base-digests.sh release-notes.sh
          bump-version.sh scan-secrets.sh secret-allowlist.txt lint-shell.sh lint-dockerfiles.sh
          gh-protect-main.sh   and *_test.sh for each
test/contract/workflows_test.go
docs/releasing.md docs/maintainers.md SECURITY.md Makefile (lint-scripts, scan-secrets, release-build)
specs/024-release-security/
```

## Constitution Check (after design)
Unchanged. One tension recorded: Principle III asks for tests before code; the workflow YAML is verified by
structural tests only (no `act`), so the first real dry run is the integration test. Complexity: none added beyond
the scripts; no abstraction layers.
