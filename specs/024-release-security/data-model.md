# Data model: E20

No persistent state. The "entities" are release artifacts and their relations.

| Artifact | Name | Produced by | Covered by |
|---|---|---|---|
| Archive | `<cmd>_<version>_linux_<arch>.tar.gz` (binary + LICENSE) | `scripts/build-release.sh` | SHA256SUMS, provenance attestation |
| SBOM | `<cmd>_<version>_linux_<arch>.cdx.json` (CycloneDX 1.5) | `scripts/gen-sbom.sh` | SHA256SUMS, provenance attestation |
| Licences | `THIRD_PARTY_LICENSES` | `can licences --write` | SHA256SUMS, attestation |
| Checksums | `SHA256SUMS`, `SHA256SUMS.sigstore.json` | release workflow, cosign sign-blob | keyless signature |
| Image | `ghcr.io/<owner>/<image>:<version>` and `@sha256:<digest>` | `docker buildx build --push` | cosign signature, provenance attestation |
| Notes | release body | `scripts/release-notes.sh` + image digest list | n/a |

`<cmd>` is one of `can can-core can-static can-forms can-work can-people can-jobs can-finance can-equity`.
Images: `can-core can-static can-forms can-work can-people can-jobs can-finance can-equity can-hooks`.

Version: the tag `vMAJOR.MINOR.PATCH[-pre]`. Embedded in `can version` (`core/cli.version`, `.commit`, `.date`)
and, for the services, `core/<name>.Version`. The commit date is the build date.

Outputs of the `verify` job: `version`, `prerelease` (a `-` in the version), `publish` (tag ref and either a tag
push or a dispatch with `dry_run=false`).
