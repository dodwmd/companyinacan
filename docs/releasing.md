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
   the GitHub release with everything attached. That includes `release-images.json`, the digests of `can-core`,
   `can-static` and `can-forms` for `can up --release-manifest` (listed in `SHA256SUMS`), and the sha256 of the
   `can` archives for linux/amd64 and linux/arm64, from which `can up` installs `can` on the server. A dry run pushes nothing,
   so it writes none.

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

## Upgrade note: equity 0.3.0

Say this in the release notes of the first release that has it (specs/068-equity-board-role):

- The `equity` module **waits for approval**: it gains the permission `equity.board`.
- **The release contains a privacy fix that the held upgrade holds back.** Until it is approved, a holder record
  relinked to another person still shows that person the grant and the previous holder's name, email and tax
  id, and lets them request and cancel exercises, before anybody confirmed the new link. Approve it promptly,
  or disable the module (offline).
- **Approving it activates the existing `equity:board` assignments**: everyone assigned that role becomes an
  equity board member, who can approve grants (directory identity mode), as every owner does. The held upgrade
  names them (`can apply --plan`, `can status`, the admin UI catalogue): read the names and remove whoever
  should not be on the board **before** approving.
- After approval, self service shows only records a second person confirmed (**Holder checks**).
- A personal access token carrying `equity.board` is only issued within 10 minutes of a sign-in or
  confirmation (admin UI and API).

## Upgrade note: work 0.3.0

Say this in the release notes of the first release that has it:

- The `work` module **waits for approval**: it publishes a new event, `work.issue.priority_changed.v1`.
  Until approved it gets no other change either. `can apply --plan` shows the code,
  `can apply --approve work=<code>` approves; the admin UI's catalogue does the same. A first install
  and a restore ask for nothing.
- The tracker's database gains two tables and a column (migration 005). The date of each issue's current
  priority is filled from its history.

## Upgrade note: module internet access

Modules reach the internet only when their manifest sets `runtime.internet_egress` (`support`, `newsletter`,
`crm`, `status`, `provisioning`). Say so in the release notes of the first release that has this: a company's
**own modules** that connect outside the cluster must add the flag, and `wiki` can no longer download plugins.

## Upgrade note: module upgrades and the leads view

Say this in the release notes of the first release that has it:

- Modules are upgraded when their content changes. An upgrade that widens what a module may do waits for
  approval (`can apply --plan`, then `--approve <module>=<code>`, or the admin UI catalogue). The forms module
  0.2.0 is one: until it is approved there is no `leads.erase`.
- **The admin UI's leads page now works without setup, so everyone with `platform.view`, the auditor role
  included, can read the leads** (names, addresses, messages). Check who holds those roles.
- **A module waiting for approval gets no change at all**, exposure removal, secret rotation and mail relay
  opt-out included. To take something away at once, approve the upgrade or disable the module (offline).
- can-core uses the forms module's read and write token for it; users with `leads.erase` can erase from the UI.

## Upgrade note: reminders

Say this in the release notes of the first release that has it (what is announced:
[notifications.md](notifications.md#reminders)):

- The `finance`, `jobs`, `work`, `people` and `entity` modules **wait for approval**: they publish new events
  or gain notification recipients. **Nothing of the new reminders works for a module until it is approved**,
  and until then it gets no other change either. `board` is upgraded without asking.
- To approve: `can apply --plan` shows each waiting module, what it widens and its code; then
  `can apply --approve finance=<code>,jobs=<code>,work=<code>,people=<code>,entity=<code>` (name only the
  modules you run). Or use the admin UI's catalogue page. `can status` lists what still waits.
- A first install and a restore ask for nothing.
- Outside consumers of `contract.expiring.v1` or `entity.filing.due.v1` must accept the `{"count": n}` form.
- Time off requests pending at the upgrade do not tell their requester about the decision.
- After approval the first sweep announces what is already overdue or due, as one summary per kind when
  there are more than five. Contract renewal reminders raised before the upgrade are not raised again.

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
