# Company in a Can

A self-hosted business control panel — think Webmin, but for business people.

```
make up        # ~5 minutes later: live HTTPS landing page, forms and a support inbox
make enable crm
make dev       # same thing locally on k3s (k3d)
```

No Kubernetes handy? `docker compose up --build` runs the whole stack on one machine for local testing ([DEV ONLY, insecure defaults](docs/local-compose.md)).

Every capability is a **module** you can switch on or off (support, CRM, wiki, newsletter, analytics, …). Modules initially wrap existing open-source apps and are meant to be replaced by in-house implementations over time, without changing how the rest of the system interacts with them. It runs on a single-node k3s cluster on any provider that gives you a VM with SSH, with first-class support for a few (Hetzner first).

> **Status:** pre-alpha. The foundation (epic E01) is in place: the `can` CLI skeleton (`init`, `validate`, `doctor`, `licences`), the `company.yaml` / `module.yaml` schemas with contract tests, licence checks and CI. `make dev` (local k3d cluster, [docs](docs/dev-cluster.md)) is implemented but unverified on a real cluster; `make up` is not implemented yet; see [docs/epics](docs/epics/README.md) for the plan.

## Goals

- **Five-minute launch:** `make up` → public landing page with lead capture and a support inbox UI (connecting real email is a follow-up step).
- **Small footprint:** explicit RAM budgets, enforced in CI; the starter stack targets a 2 GB server (4 GB recommended until measured).
- **Provider-portable:** any VPS over SSH, plus automated provisioning for a few providers.
- **Local = production:** `make dev` runs the same charts on a local k3s cluster.
- **Whole-company lifecycle (later phases):** from launch through customers, suppliers, hiring (job-board posting built in), people management, 1:1s, a Jira-like tracker, contracts, invoices, timesheets and stock options, to wind-down — as composable modules. A tool, not legal, tax or payroll advice.
- **Modular:** pick only the modules you need.
- **One front door:** one admin UI, one login, one backup, one update flow.
- **Multi-company:** manage several companies from the same tooling (one server per company).
- **Web and Android:** a responsive/PWA web UI and a native Android companion app, both on the same public API (no macOS/iOS for now).
- **Replaceable internals:** stable module contracts, so OSS apps can be swapped for in-house ones.

## Documentation

See [SPEC.md](SPEC.md) for the architecture, module system, catalogue and roadmap.

## Development

Requires Go (see `go.mod`), `git` and `make`.

```sh
make test                      # lint + unit + contract tests + licence check
make build                     # builds bin/can
bin/can init my-company --name "My Co" --domain myco.example --non-interactive
bin/can validate my-company --modules-dir modules
bin/can doctor
```

The project is developed spec-first with [GitHub Spec Kit](https://github.com/github/spec-kit): see [CONTRIBUTING.md](CONTRIBUTING.md) and `specs/`.

## Contributing

Not yet open for outside contributions while the design settles; the rules are in [CONTRIBUTING.md](CONTRIBUTING.md) (DCO sign-off, Spec Kit workflow) and [SECURITY.md](SECURITY.md). Ideas and feedback via issues are welcome once the repo is public.

## Licence

[Apache License 2.0](LICENSE). Bundled or wrapped third-party applications keep their own licences.
