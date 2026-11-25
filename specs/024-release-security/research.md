# Research: E20 release engineering and supply-chain security

Decisions with reasons. Everything was decided and tested offline; "unverified" marks what only a real run proves.

**R1 Tool choice: prefer in-repo and GitHub-native.** Secret scanning is a small POSIX scanner
(`scripts/scan-secrets.sh`): no third-party token, no network, an allowlist that demands a reason. The SBOM is
generated from `go version -m` (the module list the toolchain embeds in the binary), so there is no tool to trust
or pin. gitleaks/trufflehog would add a dependency and, for the hosted variants, a token. Trade-off: the scanner
has a short rule list (private keys, AWS, GitHub, Slack, Google, Stripe, 32+ char quoted literals assigned to
password/secret/token names); it is a backstop, and GitHub secret scanning with push protection (maintainers.md)
is the primary control.

**R2 SBOM format: CycloneDX 1.5 JSON.** Small, widely consumed, trivially written from the module list. Components
are Go modules with `pkg:golang` purls and the `go.sum` h1 hash as a property. Not covered: base-image layers (the
images are distroless static, no package database). The document is deterministic (serial number derived from the
input, timestamp from `SOURCE_DATE_EPOCH`).

**R3 Reproducibility.** `CGO_ENABLED=0 -trimpath -buildvcs=false`, `-X` date = commit time, sorted tar with fixed
owner and mtime, `gzip -n`. Verified locally: all 18 archives identical across two builds. Caveat: identical only for
the same Go toolchain version; `go.mod` plus `setup-go` pins it, a user must use the same version to reproduce.

**R4 Keyless signing, no long-lived key.** cosign keyless (OIDC from the workflow) plus
`attest-build-provenance`. The verifier pins the workflow identity (`release.yml@refs/tags/v...`) and the GitHub
OIDC issuer, so a fork cannot produce a matching signature. `SHA256SUMS` is signed as a blob with a bundle, the
archives and SBOMs are attested, the images are signed by digest and attested with `push-to-registry`.
Unverified: all of it, needs a real run (and a public repository for the public Sigstore log).

**R5 Base-image digests at build time.** The Dockerfiles keep `REPLACE_WITH_*` placeholders so development builds
stay simple and `make release-check` documents the guard (specs 004, 007). The release workflow does not rewrite
files: `scripts/resolve-base-digests.sh` resolves each placeholder ARG with `docker buildx imagetools inspect`
and the workflow passes them as `--build-arg`. The script understands both shapes in the repo
(`ARG X=image@sha256:REPLACE` and `ARG X=sha256:REPLACE` used in `FROM image:${V}@${X}`); its self-test uses a fake
resolver and checks every `deploy/*/Dockerfile` resolves completely. Resolved digests are uploaded with the run.
Trade-off: the pin is "current digest at release time", not a reviewed pin in the repository; the Dependabot docker
entries cannot see `FROM ${ARG}` lines.

**R6 Dry run is structural, not conditional steps.** Publishing steps live only in two jobs that are skipped
(`if: needs.verify.outputs.publish == 'true'`), so a dry run cannot reach a push even through a mistake in a step
condition. A contract test asserts the publish verbs appear only in those jobs and that they alone hold write
permissions. A real publish needs a tag ref; a dispatch from a branch is always a dry run.

**R7 Third-party actions kept minimal.** checkout, setup-go, upload/download-artifact, setup-buildx, login,
cosign-installer, attest-build-provenance, dependency-review, codeql. The release is created with the preinstalled
`gh` (no release action), images are built with plain `docker buildx build`. All pinned by SHA with the tag in a
comment (tags resolved with `gh api repos/<o>/<r>/tags`; annotated tags resolve to the commit).

**R8 Dependency licence policy differs from the wrapped-app policy.** `schemas/accepted-licences.yaml` lists
licences for wrapped apps that run as separate containers (it accepts GPL/AGPL there). Go dependencies are linked
into Apache-2.0 binaries, so dependency review uses a deny list of GPL, AGPL, LGPL (static linking) and EUPL. A
deny list rather than an allow list so an unusual permissive licence does not fail a pull request; a contract test
keeps permissive ids out of it.

**R9 Footprint gate stays advisory.** See `docs/maintainers.md`: shared-runner numbers do not describe a 2 GB VPS,
k3d is not deterministic, and a path-filtered required check hangs pending. The deterministic part (declared RAM in
manifests and charts) is already checked by contract tests. Conditions for promotion are written down.

**R10 Branch protection is a script, not an action.** Applying protection needs repository admin; no workflow gets
that token. The script is dry-run by default and a contract test ties its check list to `ci.yml`. `REVIEWS` defaults
to 0 because the project has one maintainer; CODEOWNERS is a draft that routes reviews until a second maintainer exists.

**R14 The tag-on-main check is not a boundary.** A pushed tag can carry a commit that edits `release.yml`. The
boundaries are the tag ruleset (who may create `v*` tags) and the `release` environment (required reviewers, tag-only
deployments); GitHub creates an unprotected environment silently, so the publish jobs first run
`scripts/check-release-env.sh`, which fails closed (needs `actions: read`; falls back to the maintainer-set variable
`RELEASE_ENV_PROTECTED=true` only when the API is unreadable). Unverified: whether the workflow token may read the
environment's protection rules.

**R15 Toolchain.** `go.mod` has no `toolchain` line and CI runs with `GOTOOLCHAIN=local`; a pin would force a
download on every developer machine. Instead the exact `go version` goes into the release notes and the SBOM.

**R11 Arm64 images not built.** The Dockerfiles compile inside the build stage for the build platform; an arm64
image would build under QEMU (slow, another pinned action) or need `--platform=$BUILDPLATFORM` plus `GOARCH` in nine
Dockerfiles. Deferred; arm64 binaries are shipped.

**R12 CodeQL and Go 1.27.** Whether the pinned CodeQL release analyses the Go version in `go.mod` is unknown until
it runs; the job is not required so a failure does not block anything.

**R13 can-core and can-static carry no version variable.** Their `-X` target does not exist, so `build-release.sh`
and their Dockerfiles build with `-s -w` only; the image tag and the archive name carry the version. Adding one is a
separate change to `cmd/can-core`.

**Unverified in this environment (no publishing, no act):** GHCR push, cosign, attestation, release creation,
`imagetools` resolution against the real registry, image builds on a runner, CodeQL, dependency-review behaviour,
Dependabot parsing the config. The workflows were checked as YAML, by contract tests, and by actionlint when it is
available; the shell by shellcheck and self-tests.
