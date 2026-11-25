# Contract: release pipeline and scripts

## Triggers (`release.yml`)
- `push` of tags `v*.*.*` only (no branches). Strict semver `^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$` is enforced by the first job.
- `workflow_dispatch` inputs: `dry_run` (boolean, default **true**), `version` (default `v0.0.0-dryrun`, used only on a branch).
- Publishing happens iff the ref is a tag and (the event is a push, or a dispatch with `dry_run=false`).

## Permissions
Top level: `contents: read`. `publish-images`: contents read, packages write, id-token write, attestations write.
`publish-release`: contents write, id-token write, attestations write. No other job has a write permission.
Both publish jobs use the `release` environment.

## Scripts (POSIX sh; exit 0 ok, 1 failure, 2 usage)
| Script | Contract |
|---|---|
| `build-release.sh <version> <outdir>` | archives + `bin/` + `SHA256SUMS`; env `BINARIES`, `ARCHES`, `SOURCE_DATE_EPOCH`, `COMMIT` |
| `check-reproducible.sh <version> <workdir>` | builds into `a/` and `b/`, diffs `SHA256SUMS` |
| `gen-sbom.sh <binary> [version]` | CycloneDX JSON on stdout |
| `resolve-base-digests.sh <Dockerfile>` | `NAME=VALUE` lines for `--build-arg`; env `DIGEST_RESOLVER` |
| `release-notes.sh <tag> [prev]` | markdown on stdout; subjects never interpreted |
| `bump-version.sh <major\|minor\|patch\|vX.Y.Z> [--tag]` | prints versions; `--tag` creates a local annotated tag on a clean `main`; never pushes |
| `scan-secrets.sh [dir]` | prints `path:line: [rule]` only, never the value; allowlist `scripts/secret-allowlist.txt` |
| `lint-shell.sh [--require]` | shellcheck on tracked `*.sh` |
| `lint-dockerfiles.sh [--release] [root]` | pinned FROM, USER, no ADD of a URL, no pipe into a shell; placeholders flagged with `--release` |
| `gh-protect-main.sh [--apply] [owner/repo]` | dry run by default; required checks `lint unit contract licences dco` |

## Verification commands
In SECURITY.md: `cosign verify`, `cosign verify-blob --bundle`, `gh attestation verify`, `sha256sum -c`.
