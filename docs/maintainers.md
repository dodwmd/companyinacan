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

## Applying the protections

`scripts/gh-protect-main.sh` prints the exact API calls and payloads (default is a dry run, no network) for three
things: branch protection of `main`, a tag ruleset, and the `release` environment.

```sh
scripts/gh-protect-main.sh                 # prints; changes nothing
scripts/gh-protect-main.sh --apply         # applies all three; needs a token with admin rights on the repository
RELEASE_REVIEWERS=alice,bob scripts/gh-protect-main.sh --apply   # reviewers of a release (default: the repo owner)
```

Branch protection requires the five checks above (up to date with `main`), resolved conversations, no force pushes
and no deletions of `main`. Two things are **opt-in** on purpose, and the dry run says so:

- `LINEAR_HISTORY=1` requires linear history. It is off because this repository merges with merge commits.
- `REVIEWS=n` (default 0) requires n approving reviews **and** code-owner review (`.github/CODEOWNERS`, a draft).
  With one maintainer this locks everyone out of merging, since nobody can approve their own pull request. Raise it
  only when a second maintainer exists.

The script is **not applied** by the repository's automation; a maintainer runs it once.

## The `release` environment and tag ruleset are MANDATORY

`release.yml` checks that a release tag is on `main`, but that check is not a security boundary: a pushed tag can
point at a commit that edits `release.yml` itself. What actually stops an unauthorised publish is:

1. **A tag ruleset** for `refs/tags/v*` that only repository admins can bypass, so nobody else can create, move or
   delete a release tag (the script creates it as `release-tags`).
2. **The `release` environment** with **required reviewers**, deployments limited to a custom policy that allows
   only `v*` **tags** (not branches), and `prevent_self_review` on as soon as there are two reviewers. The publish
   jobs (`publish-images`, `publish-release`) name this environment, so they wait for a reviewer to approve. With one
   reviewer, self-review has to stay allowed or that person could never release; add a second reviewer and the
   script turns the prevention on.

**Naming an environment that does not exist silently creates an unprotected one**, so `environment: release` alone
protects nothing. Create and configure it before the first tag is pushed. As a safety net the publish jobs run
`scripts/check-release-env.sh` before logging in or pushing and abort unless the API shows required reviewers and a
custom deployment policy. That needs the workflow token to read the environment (the jobs request `actions: read`); if
GitHub answers 403/404 the job aborts, unless a maintainer has set the repository variable `RELEASE_ENV_PROTECTED` to
`true`, which they must do only after configuring the environment by hand. The guard protects against a
misconfigured repository, not against an attacker who can already push a workflow change, which is why the tag
ruleset and the reviewers are the primary controls.

Also worth enabling in the repository settings (not scriptable here): the dependency graph (the `dependency-review`
job answers 403 and is informational until it is on), secret scanning with push protection, Dependabot alerts and
security updates, private vulnerability reporting, and "Require approval for all outside contributors" for workflow
runs.

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
