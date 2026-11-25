# Maintainers: repository protection and checks

For people with write or admin rights. Spec: [specs/024-release-security](../specs/024-release-security/spec.md).

## Required checks

The CI contract ([specs/001-project-foundation](../specs/001-project-foundation/)) names five jobs in
`.github/workflows/ci.yml`. **Do not rename them**: branch protection refers to the names, and renaming a job
silently un-requires it (a contract test fails if they change).

| Check | What it runs |
|---|---|
| `lint` | `make lint` (golangci-lint, pinned) |
| `unit` | `make unit` |
| `contract` | `make contract` (includes the workflow hygiene and script self-tests) |
| `licences` | `can licences --check` |
| `dco` | sign-off on every commit of the pull request |

Everything else is advisory for now: `security.yml` (govulncheck, dependency review, CodeQL, secret scan, shell and
Dockerfile lint), `footprint.yml`, `dev-cluster.yml`. Promote a job to required only after it has been stable on
`main` for a few weeks; `security.yml` jobs run on every pull request, so they can be required without the
"pending forever" problem path-filtered workflows have.

## Applying branch protection

`scripts/gh-protect-main.sh` prints the exact API call and payload (default is a dry run, no network):

```sh
scripts/gh-protect-main.sh                 # prints; changes nothing
scripts/gh-protect-main.sh --apply         # applies it; needs a token with admin rights on the repository
REVIEWS=1 scripts/gh-protect-main.sh --apply   # also require one approving review (needs a second maintainer)
```

It requires the five checks above (up to date with `main`), linear history, resolved conversations, no force
pushes and no deletions of `main`. `REVIEWS` defaults to 0 because a one-maintainer project cannot approve its own
pull requests; set it to 1 when there is a second maintainer, and `.github/CODEOWNERS` (a draft) then also blocks
until an owner approves. The script is **not applied** by the repository's automation; a maintainer runs it once.

Also worth enabling in the repository settings (not scriptable here): secret scanning with push protection,
Dependabot alerts and security updates, private vulnerability reporting, "Require approval for all outside
contributors" for workflow runs, and a `release` environment with a required reviewer (the publish jobs of
`release.yml` use it, so a tag alone cannot publish without a maintainer clicking approve).

## Why the footprint check stays advisory

Constitution II makes footprint a CI gate, and `can footprint check` compares measured idle RAM with each
module's declared `ram_mb`. The k3d measurements run in `footprint.yml` on GitHub's shared runners (2-4 vCPU,
about 16 GB). Those numbers describe that machine, not the 2 GB VPS the product targets
([docs/measurements/phase0-ci.md](measurements/phase0-ci.md)). A required check must be deterministic; k3d image
pulls and a shared runner are not (that is why every job there is `continue-on-error`). The workflow is also
path-filtered, and a required check that does not run on a pull request blocks it forever as "pending". So today the
gate is enforced where it is deterministic: the contract tests pin the declared `ram_mb` and limits of the in-house modules to
their charts and manifests, and the measurement is reviewed by a human from the workflow summary. To make it required later: remove
`continue-on-error`, drop the `paths:` filter (or add an always-run job that reports success when no relevant file
changed), run on a self-hosted runner sized like the target, and add the job name to `REQUIRED_CHECKS` in
`scripts/gh-protect-main.sh` and to the table above.

## Secret handling in workflows

Workflows run on `pull_request` (never `pull_request_target`), so a fork's code never sees secrets and gets a
read-only token. The only secret any workflow uses is the automatic `GITHUB_TOKEN`; a contract test fails on any
other. Values from events (titles, branch names, inputs) must reach scripts through `env:` and be quoted, never
through `${{ }}` inside `run:` (also tested). The in-repo secret scanner (`scripts/scan-secrets.sh`) runs in
`security.yml`; fake dev credentials are allowlisted with a reason in `scripts/secret-allowlist.txt`.

## Routine

- Weekly: review the grouped Dependabot pull requests; they run the same required checks. A new action version is
  a SHA change: read the upstream release notes before merging.
- Weekly scheduled `security.yml` run: failures there mean a new advisory against a dependency or a regression;
  treat them like a bug report.
- Releases: [releasing.md](releasing.md).
