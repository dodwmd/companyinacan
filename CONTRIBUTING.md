# Contributing

Thanks for your interest in Company in a Can. The project is pre-alpha and developed
spec-first, so the best way to contribute is to start from the spec.

## How the project is organised

- [SPEC.md](SPEC.md) is the source of truth for behaviour and architecture.
- [.specify/memory/constitution.md](.specify/memory/constitution.md) holds the non-negotiable
  engineering principles; every pull request is reviewed against them.
- Work is split into **epics** ([docs/epics/README.md](docs/epics/README.md)). Each epic is one
  [GitHub Spec Kit](https://github.com/github/spec-kit) feature under `specs/NNN-name/`
  (`spec.md`, `plan.md`, `tasks.md`, ...), developed on its own branch.

## Workflow

1. Pick or propose an epic (open an issue using the *epic* template). Discuss before large work.
2. Follow the Spec Kit flow in order: `/speckit-specify` -> `/speckit-clarify` (if needed) ->
   `/speckit-plan` -> `/speckit-tasks` -> `/speckit-analyze` -> `/speckit-implement`.
3. Write tests first (Constitution III) and keep them failing until the code exists.
4. If behaviour changes, update `SPEC.md` in the same pull request (Constitution I).
5. Open a pull request using the template and tick the constitution checklist.

## Running the checks locally

You need Go (see `go.mod`), `git` and `make`; `can doctor` verifies this.

```sh
make test       # lint + unit + contract tests + licence check
make unit       # unit tests only
make contract   # schema fixtures and examples
make licences   # regenerate THIRD_PARTY_LICENSES after changing a module manifest
make build      # build bin/can
make docs-check # every relative Markdown link must resolve (also run by `make contract`)
make e2e-up e2e # end-to-end suite on the Docker Compose stack (advisory in CI; needs Docker Compose v2 or podman-compose)
make e2e-down   # remove that stack and its volumes
```

End-to-end scenarios live in `test/e2e` (build tag `e2e`, helpers in `test/e2e/harness`); see
[docs/getting-started.md](docs/getting-started.md). New user-facing behaviour needs a scenario there only when it
crosses the compose edge; module logic is still tested in the module's own package.

No network is needed after dependencies are downloaded (the first `make lint` fetches the
pinned linter and falls back to `go vet` if it cannot).

To add a validation case, drop `testdata/invalid/<name>.yaml` and a sidecar
`<name>.expect.yaml` listing the expected `rule` and `path`; the contract suite picks it up.

## Flaky tests

A test that fails sometimes is a bug, in the test or in the code. The `unit` job is required, so a
flake blocks everyone.

- Never hide a flake with `t.Skip`, a retry loop around the whole test, or `-run` filters. A skip that
  hides a failing assertion is worse than a red test.
- Fix the cause. The usual ones, and the usual cure:
  - a wall-clock assertion (`elapsed < 100ms`): inject a clock or sleep function, or assert an
    observable property (a count, an ordering, a channel receive) instead of a duration;
  - random test data that can collide with an assertion (a random id that happens to contain `555`):
    use fixture literals with an unambiguous pattern;
  - a tight shutdown or wait deadline: wait on a condition (a channel, `Eventually`-style polling) with
    a generous guard that only exists to turn a hang into a failure, not to measure speed;
  - shared package-level state between parallel or shuffled tests: give each test its own instance.
- Reproduce before and after with `sh scripts/flaky-hunt.sh` (for example
  `PKGS=./core/jobs/... ITER=50 sh scripts/flaky-hunt.sh`). It repeats the tests with shuffled order
  under CPU load. The non-required `flaky-hunt` workflow runs it nightly.
- Quarantine is the last resort and needs an open issue. A quarantined test carries the issue link in
  the skip message (`t.Skip("flaky, see https://github.com/dodwmd/companyinacan/issues/NNN")`), the
  issue says how to reproduce it, and the same pull request adds the test to the list in
  `docs/measurements/ci-stability.md`. A quarantine without an issue link is rejected in review.

## Sign-off (DCO)

Every commit must carry a `Signed-off-by` line certifying the
[Developer Certificate of Origin](https://developercertificate.org):

```sh
git commit -s -m "your message"        # sign a new commit
git commit --amend -s                  # sign the last commit
git rebase --signoff <base>            # sign several commits
```

CI's `dco` job fails if a commit in your pull request is not signed off by its author.
We do not require a CLA.

## Licences and third-party code

Project code is Apache-2.0. A module that wraps a third-party app must declare the upstream
licence in its manifest; only OSI-approved licences from `schemas/accepted-licences.yaml` are
accepted, and anything source-available must be marked `licence_class: source-available`
(opt-in, flagged in the UI). CI fails otherwise.

## Data hygiene

Never commit secrets, credentials, real personal data or real company data. Examples use fake
values and reserved domains such as `example.com`, `acme.example`.

## Security issues

Do not file public issues for vulnerabilities; see [SECURITY.md](SECURITY.md).

## Conduct

Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).
