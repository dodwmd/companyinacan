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
```

No network is needed after dependencies are downloaded (the first `make lint` fetches the
pinned linter and falls back to `go vet` if it cannot).

To add a validation case, drop `testdata/invalid/<name>.yaml` and a sidecar
`<name>.expect.yaml` listing the expected `rule` and `path`; the contract suite picks it up.

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
