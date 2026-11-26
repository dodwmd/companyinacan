# Releasing

Runbook for cutting, checking, rolling back and (when needed) withdrawing a release. Spec:
[specs/024-release-security](../specs/024-release-security/spec.md). Everything here describes the pipeline as
written; **it has not yet run for real** (see "Status" at the end).

## What a release is

A git tag `vX.Y.Z` (or `vX.Y.Z-rc.N`) on `main`. The tag is the version of the release and of the binaries.
The module images have versions of their own, which the module charts pull:
[core/modimages/images.txt](../core/modimages/images.txt) is the one place they are set. The release pushes each
image under the release tag (`:vX.Y.Z`) and under its own version, which it never moves once published (and the release fails if the sources changed since); a
pre-release does not publish the plain versions. The
tag starts `.github/workflows/release.yml`, which, in order:

1. **verify**: strict semver check, tag is an ancestor of `main`, `make test` (lint, unit, contract, licences),
   `can licences --check` (so `THIRD_PARTY_LICENSES` is current and every wrapped-app licence is accepted), the
   script self-tests.
2. **build**: static binaries (`CGO_ENABLED=0`, `-trimpath`, linux amd64 and arm64; macOS and Windows are not
   built) for `can`, `can-core`, `can-static`, `can-forms`, `can-work`, `can-people`, `can-jobs`, `can-finance`,
   `can-equity`, `can-board`, `can-provision`, `can-files`. Built **twice**; the sha256 of every archive must match or the release stops. Then a CycloneDX
   SBOM per binary, `THIRD_PARTY_LICENSES`, `SHA256SUMS`, and release notes from the commit log.
3. **images-check**: builds the twelve container images (`deploy/*/Dockerfile`) with the base-image digests
   resolved right then by `scripts/resolve-base-digests.sh` (`docker buildx imagetools inspect`), without pushing.
4. **publish-images** and **publish-release** (only for a real release, only these two jobs hold write
   permissions, behind the `release` environment, which must have required reviewers: the publish jobs abort before any login or push if it does not): push to GHCR, sign each image by digest with keyless cosign,
   attach a build-provenance attestation to each image and to the archives/SBOMs, sign `SHA256SUMS`, and create
   the GitHub release with everything attached.

`can version` reports the tag, the short commit and the commit date (the build date is the commit time, which is
what makes the build reproducible).

## Before the first release (checklist)

- [ ] The **`release` environment exists** with required reviewers and deployments limited to `v*` tags
      (`scripts/gh-protect-main.sh --apply`, see [maintainers.md](maintainers.md)). Without it GitHub creates an
      unprotected environment on first use and the publish jobs refuse to run (`scripts/check-release-env.sh`).
- [ ] The **tag ruleset** `release-tags` is active: only admins can create, update or delete `v*` tags.
- [ ] Branch protection on `main` applied; the five required checks are green on `main`.
- [ ] Dependency graph, secret scanning and private vulnerability reporting enabled.
- [ ] A dry run of this workflow succeeded and its artifacts were inspected.
- [ ] After the first push of each image, make its GHCR package **public** (package settings > visibility). New
      packages are private; servers pull anonymously, and `can up` / `can apply` refuse a module whose image
      they cannot pull anonymously.

## Image versions

Before releasing, bump the version of **every image whose sources changed** since its version was published
(shared code under `core/` and linked dependencies count). The release **fails before pushing** otherwise:
`scripts/check-image-versions.sh` compares `scripts/image-source-digest.sh` (the linked files of this
repository, module versions, the go directive, the Dockerfile) with the digest recorded as a label on the
published image. Base images are not compared: a rebuild on a newer base does not need a new version, and does
not reach the module charts without one. In the pull request that changes the service:

```sh
scripts/set-image-version.sh can-work X.Y.Z    # the list, the charts and the workflows
```

and bump `version` in the module's `module.yaml`: can-core upgrades an installed module when that or its
settings change, not when only the image does. `test/contract/module_images_test.go` fails when a chart, a
manifest or a workflow disagrees with the list.

## Upgrade note: module internet access

Modules reach the internet only when their manifest sets `runtime.internet_egress` (`support`, `newsletter`,
`crm`, `status`, `provisioning`). Say so in the release notes of the first release that has this: a company's
**own modules** that connect outside the cluster must add the flag, and `wiki` can no longer download plugins.

## Cutting a release

```sh
git checkout main && git pull --ff-only
make test                                  # must be green
scripts/bump-version.sh minor              # dry run: prints current and next version
scripts/bump-version.sh minor --tag        # creates the annotated tag LOCALLY (SIGN=1 for a signed tag)
scripts/release-notes.sh v0.2.0            # preview the notes
git push origin v0.2.0                     # THIS starts the release workflow
```

Before pushing, rehearse: **Actions > release > Run workflow** on `main` with `dry_run` left true. It does all
the building and checking and publishes nothing; download the `release-out` artifact and look. Check
`scripts/lint-dockerfiles.sh --release` is expected to fail on `main` (placeholders): the release workflow
does not need it to pass because it resolves the digests into build arguments instead of editing the files.

Pre-releases (`v0.2.0-rc.1`) go through the same pipeline, are marked as pre-releases and do not move `:latest`.

If the workflow fails before `publish-images`, nothing was published: fix `main`, delete the tag
(`git push --delete origin vX.Y.Z`, `git tag -d vX.Y.Z`) and tag the fixed commit. If it fails **after** some
images were pushed, do not re-tag: see rollback.

## Verifying a release

See [SECURITY.md](../SECURITY.md#verifying-a-release) for `cosign verify`, `cosign verify-blob`,
`gh attestation verify`, the SBOM and the reproducibility check.

## Rollback

Published tags, images and signatures are immutable by policy; a bad release is fixed forward.

- **Operators** roll back by redeploying the previous version (digests of every release are in its notes):
  `can-core` upgrades are atomic Helm releases; restore data from the pre-upgrade snapshot
  ([backup-restore.md](backup-restore.md)). Helm does not undo database migrations, which is why `can upgrade`
  snapshots first.
- **Maintainers**: cut `vX.Y.Z+1` from the last good commit (`git revert` on `main` or a fix) and mark the bad
  release in its GitHub release notes ("do not use, see vX.Y.Z+1"). Mark it as a pre-release or edit the notes;
  do not delete the tag or the images: people may already pin their digests and deleting breaks them silently.
  `:latest` is re-pointed by the next stable release automatically.
- A partial release (some images pushed, then a failure): fix the cause and cut the next patch version. Do not
  push the same tag again.

## Security advisory flow

1. Receive the report through private vulnerability reporting ([SECURITY.md](../SECURITY.md)); acknowledge within 7 days.
2. Open a **draft security advisory**; develop the fix in its private fork; request a CVE if warranted.
3. Merge the fix to `main` through the normal required checks (publicly, once the advisory is ready to publish).
4. Cut the patch release as above. `govulncheck` and CodeQL (security workflow) should be green for the fix.
5. Publish the advisory, naming affected and fixed versions; edit the affected releases' notes to point at the fix.
6. If a dependency was the cause, note the upstream advisory id. If a leaked credential was involved, rotate it
   first and release afterwards.

## Dependencies and pinned actions

Dependabot (`.github/dependabot.yml`) opens weekly, grouped pull requests for Go modules, GitHub Actions and
Dockerfiles. Actions are pinned by commit SHA with the tag in a comment; Dependabot updates both. When adding an
action by hand, resolve the tag to a commit (`gh api repos/<owner>/<repo>/commits/<tag> --jq .sha`), pin the SHA,
and write the tag in the comment: a contract test (`test/contract/workflows_test.go`) fails the build otherwise.
`go run ...@version` tools (golangci-lint, govulncheck) are pinned by version in `Makefile` / `security.yml`.

## Status: what has and has not been verified

Verified offline: the shell scripts (unit-style self-tests, shellcheck), a full local `build-release.sh` of all 18
archives and the reproducibility check, SBOM generation on a real binary, the YAML of the workflows and their
hygiene (contract tests). **Not verified, because no real release has been run:** GHCR push, cosign signing,
provenance attestation, the `gh release create` step, `docker buildx imagetools inspect` digest resolution and the
image builds on a runner, and whether CodeQL supports the Go version in `go.mod`. Treat the first release as a
rehearsal: run the dry run, then cut an `-rc.1` tag before the first stable one. arm64 container images are not built
yet (the images are linux/amd64; the arm64 binaries are in the archives).
