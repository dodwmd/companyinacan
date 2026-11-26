# Documentation index

Start here. The project is **pre-release: nothing has run on a real server** (see the [README](../README.md#what-is-verified)).
Every document says what was verified and what was not; where two documents disagree, [SPEC.md](../SPEC.md) is the design
and the code is the truth.

## Start

| Read | For |
|---|---|
| [README](../README.md) | What it is, status, module table, footprint measured against claimed, limitations |
| [getting-started.md](getting-started.md) | Run it: Docker Compose, k3d, a real server (with the caveats) |
| [architecture.md](architecture.md) | How can-core, modules, events, tokens, approvals and backup fit together; sequence diagrams for lead capture, hire handoff and approvals |
| [security-model.md](security-model.md) | Trust boundaries, tokens, sealed data, audit, and the known residual risks in one list |
| [SPEC.md](../SPEC.md) | Vision, principles, module system, catalogue, roadmap, risks |
| [epics/README.md](epics/README.md) | The epic list with honest statuses |

## Running and operating

| Read | For |
|---|---|
| [local-compose.md](local-compose.md) | The Docker Compose stack: URLs, dev credentials, what is not covered, the end-to-end suite |
| [dev-cluster.md](dev-cluster.md) | `make dev`: the local k3d cluster |
| [node-isolation.md](node-isolation.md) | Keeping pods away from the server: the host firewall rules, checking them, what is unverified |
| [make-up.md](make-up.md), [providers.md](providers.md) | `make up` on a server; the ssh, Hetzner and DigitalOcean providers; Cloudflare DNS |
| [backup-restore.md](backup-restore.md) | Backups, the recovery kit, `can restore` (not functional yet) |
| [observability.md](observability.md) | Metrics, `/readyz`, request ids, log redaction, alert rules, `can diagnose` support bundles, measured cost |
| [releasing.md](releasing.md), [maintainers.md](maintainers.md) | Cutting a release, required checks, branch protection, advisories |
| [module-wiring.md](module-wiring.md) | Installing modules through can-core: secrets, personal access tokens, events, approvals, what ran where |

## Platform (can-core)

| Read | For |
|---|---|
| [admin-ui.md](admin-ui.md) | The admin web UI, module catalogue and wizard, security model, installing on Android |
| [sso.md](sso.md) | OpenID Connect sign-in for the admin UI, and one-click sign-in into module web pages (Apps, launch tickets) |
| [approvals.md](approvals.md), [notifications.md](notifications.md) | The approval engine, RBAC names, inbox, email and Web Push |
| [pwa.md](pwa.md) | Installing the admin UI on a phone, Web Push, limits |
| [email.md](email.md) | Outbound email: the relay, who may be written to, quotas, delivery status |
| [email-policy.md](email-policy.md) | How outbound and inbound email are meant to work |

## Modules

| Read | For |
|---|---|
| [modules-catalogue.md](modules-catalogue.md) | Every module: status, memory, licence, what was verified |
| [work-tracker.md](work-tracker.md), [work-ui.md](work-ui.md) | The `work` tracker: API, CLI, web UI |
| [people.md](people.md), [people-ui.md](people-ui.md) | The `people` directory, 1:1s, goals, reviews, time off; its web UI |
| [recruiting.md](recruiting.md), [recruiting-ui.md](recruiting-ui.md) | The `jobs` module: requisitions, careers pages, ATS; its web UI |
| [provisioning.md](provisioning.md) | Account provisioning (SCIM) for joiners, movers and leavers |
| [finance.md](finance.md), [finance-ui.md](finance-ui.md) | Invoices, payments, contracts, timesheets, expenses; the finance web UI |
| [files.md](files.md) | Document and file storage: spaces, versions, share links, scan hook, retention, erasure |
| [equity.md](equity.md) | Cap table and option grants (a tool, not legal or tax advice) |
| [board.md](board.md) | Board meetings, quorum, resolutions, minutes (a tool, not legal advice; experimental, not in the compose stack) |
| [entity.md](entity.md) | Legal entities, statutory registers, filings calendar (a tool, not legal advice; experimental, not in the compose stack) |
| [../modules/README.md](../modules/README.md) | One directory per module, licences and network copyleft |

## Project

| Read | For |
|---|---|
| [../CONTRIBUTING.md](../CONTRIBUTING.md) | DCO sign-off, the Spec Kit workflow, how to run the checks |
| [../SECURITY.md](../SECURITY.md) | Reporting a vulnerability |
| [../specs/](../specs/) | One directory per feature: spec, plan, research, data model, contracts, tasks |
| [measurements/phase0-ci.md](measurements/phase0-ci.md) | The measured idle memory of k3s and the first two services, and the network-policy proofs |

## Checking the docs

`make docs-check` (or `sh scripts/check-doc-links.sh`) fails if a relative Markdown link points at nothing; a contract
test runs it in CI. External links are not checked.
