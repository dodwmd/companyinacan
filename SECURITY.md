# Security Policy

## Reporting a vulnerability

Please **do not open a public issue** for a security problem.

Report it privately with GitHub's private vulnerability reporting: on the repository page choose
**Security → Report a vulnerability**. If that is unavailable, contact the repository owner
(dodwmd) directly on GitHub and ask for a private channel before sharing details.

Include what you found, how to reproduce it, the affected version or commit, and the impact you
expect. Please do not include real credentials or personal data in the report.

## What to expect

- We aim to acknowledge a report within **7 days** and to share an assessment or a plan within
  **30 days**. This is a volunteer-run, pre-alpha project, so these are goals, not guarantees.
- We will credit you in the fix notes unless you prefer to stay anonymous.
- Please give us reasonable time to fix the problem before disclosing it publicly.

## How we handle an advisory

1. The maintainer confirms the report and opens a **draft GitHub Security Advisory** (private) on the
   repository, requests a CVE through it when the issue warrants one, and works on the fix in the
   advisory's private fork.
2. The fix is released as a patch version (`vX.Y.Z+1`) from `main` through the normal release pipeline,
   so the fixed artifacts are signed and attested like any other release.
3. The advisory is published with the release, naming the affected and fixed versions, a workaround if
   there is one, and credit to the reporter.
4. If a released version is dangerous to keep running, its GitHub release is edited to say so and the
   advisory points to the fixed version. Published tags and images are never rewritten or deleted; see
   [docs/releasing.md](docs/releasing.md) (rollback and security-advisory flow).

## Scope

In scope: code, charts, scripts and configuration in this repository, including the `can` CLI,
and the module manifests and hooks we ship.

Out of scope: vulnerabilities in third-party applications that modules wrap (report those
upstream; tell us if our configuration makes them worse), and findings that require an
already-compromised host.

## Supported versions

The project has not published a stable release yet. Until a `v1.0.0` exists, only the `main` branch and the
most recent tagged release (when there is one) receive security fixes. Once releases exist, the policy is:

| Version | Supported |
|---|---|
| latest minor release | security and bug fixes |
| previous minor release | security fixes only, for 90 days after the next minor |
| older | not supported; upgrade |

Pre-release tags (`vX.Y.Z-rc.N`) are never supported.

## Verifying a release

Every release (from the first one cut by the release workflow) ships with the following. Replace `vX.Y.Z`,
and use the repository's real owner and name in place of `dodwmd/companyinacan` if it moves.

**Checksums and signature of the archives.** Download `SHA256SUMS`, `SHA256SUMS.sigstore.json` and the
archives from the GitHub release, then:

```sh
cosign verify-blob \
  --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp '^https://github.com/dodwmd/companyinacan/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
```

**Build provenance (SLSA) of an archive or SBOM:**

```sh
gh attestation verify can_vX.Y.Z_linux_amd64.tar.gz --repo dodwmd/companyinacan
```

**Container images** (`ghcr.io/dodwmd/can-core`, `can-static`, `can-forms`, `can-work`, `can-people`,
`can-jobs`, `can-finance`, `can-equity`, `can-hooks`). Pin by the digest printed in the release notes:

```sh
cosign verify \
  --certificate-identity-regexp '^https://github.com/dodwmd/companyinacan/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/dodwmd/can-core@sha256:<digest>
gh attestation verify oci://ghcr.io/dodwmd/can-core@sha256:<digest> --repo dodwmd/companyinacan
```

**SBOM.** Each archive has a CycloneDX 1.5 SBOM (`<name>_vX.Y.Z_linux_<arch>.cdx.json`) listing the Go
modules compiled into the binary. It is covered by the checksums and by the provenance attestation.
`THIRD_PARTY_LICENSES` is attached to every release too.

**Reproducibility.** The release workflow builds every archive twice and refuses to publish if the checksums
differ. To check it yourself, check out the tag, install the **same Go toolchain**, and run
`SOURCE_DATE_EPOCH=$(git log -1 --format=%ct) scripts/build-release.sh vX.Y.Z out`; the archive checksums should equal
those in `SHA256SUMS`. The exact toolchain is recorded at the end of the release notes ("Built with `go version ...`")
and as a component of every SBOM; a different patch release of Go can produce different bytes.

Signing and attestation are keyless (Sigstore): there is no long-lived signing key to steal or rotate. The
trust root is the identity of the release workflow in this repository, which is why the commands above pin it.

## Security design

The security model (default-deny network policies, encrypted secrets, least privilege, sensitive
data handling, audit and retention) is described in [SPEC.md](SPEC.md) §3.1, §3.7 and §5 and in
the project [constitution](.specify/memory/constitution.md) (Principle V). How the repository and its
releases are protected is in [docs/maintainers.md](docs/maintainers.md) and [docs/releasing.md](docs/releasing.md).
