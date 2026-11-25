# Module wiring: installing and connecting the in-house modules

How the in-house modules (forms, work, people, jobs, finance) are installed by can-core and how they
talk to it and to each other. Specification: `specs/016-module-wiring/`. Status: built and tested
offline with the real can-core services and the real modules; **nothing here has been seen running on
a cluster except what the advisory dev-cluster workflow does for `work`, and that workflow has not been
run yet** (see "What ran where").

## Install

```
# company.yaml
modules:
  work: {enabled: true}
```

`can apply` hands the module to can-core, which renders `modules/<id>/chart` with its own sandboxed
renderer, creates the namespace, the ServiceAccount and the `can-module` Secret, and applies the
workloads. A chart may contain only Deployments, Services, PVCs, ConfigMaps, NetworkPolicies and (for a
module with a `runtime.subdomain`) an IngressRoute for `<subdomain>.<domain>`; anything else is refused.
Per module:

| Module | Service and port | Public route | Needs |
|---|---|---|---|
| work | `work` 8080 | none | |
| people | `people-api` 8081 | none | keyring |
| jobs | `jobs` 8080 | `careers.<domain>`: only `/careers`, `/feeds`, `/robots.txt` | keyring, `company.name`, `company.base_url` in the chart `config:` |
| finance | `finance` 8080 | none (see below) | |
| forms | `forms` 8080 public, `forms-api` 8081 | none (see below) | at least one form in the chart `config:` |

All run one replica with the `Recreate` strategy (SQLite on a ReadWriteOnce volume), as non-root with a
read-only root filesystem, resources equal to the manifest's `ram_mb` and `ram_limit_mb`, readiness on
`/healthz` and liveness running the binary's own `healthcheck`. The volume is `<id>-data` and survives
uninstall and disable. Configuration is the `config:` block of the chart's `values.yaml`, rendered into a
ConfigMap (company.yaml `settings` are **not** mapped onto it yet). Image references are
`repository:tag` plus a digest that is a placeholder until release; `make release-check` fails while
any remains.

## Secrets

can-core creates these in the module's `can-module` Secret the first time the module is enabled and
never replaces them afterwards (enabling again, upgrading, disabling and enabling again all keep them):

| Module | Keys |
|---|---|
| work | `CAN_WORK_ADMIN_TOKEN` |
| forms | `CAN_FORMS_TOKEN_KEY`, `CAN_FORMS_API_TOKEN`, `CAN_FORMS_API_READ_TOKEN` |
| finance | `CAN_FINANCE_ADMIN_TOKEN`, `CAN_FINANCE_LINK_SECRET` |
| jobs | `CAN_JOBS_ADMIN_TOKEN`, `CAN_JOBS_HASH_KEY`, `CAN_JOBS_FORM_SECRET`, `keyring` |
| people | `CAN_PEOPLE_API_TOKEN`, `keyring` |

Alongside them, for every module: `CAN_API_URL`, `CAN_MODULE_ID`, `CAN_WEBHOOK_SECRET` (the HMAC secret
that signs can-core's webhooks to the module), `CAN_TOKEN_FILE`. The values are 256-bit random hex (a
keyring is `k1 <64 hex>` lines). They are not written to logs, command output or events, and the audit
log records only that a rotation was asked for.

**Rotate:** `kubectl -n can-system exec deploy/can-core -- /can-core admin secrets rotate <module> <key>`
where key is `CAN_WEBHOOK_SECRET`, a key above, or `keyring`. The request is recorded; the reconciler
(within about 30 seconds) regenerates the value, and because the pod template carries a digest of the
Secret, the module restarts with it. A keyring rotation adds a key and keeps the old ones, so sealed
data stays readable: run the module's own `rotate-keys` afterwards to re-seal it. `CAN_JOBS_HASH_KEY`
cannot be rotated (a new key silently splits every candidate from their duplicates). Anything that was
issued with an old value (a bootstrap token in a script) stops working at the restart.

The values live in a Kubernetes Secret, which is base64 in etcd: encrypting Secrets at rest is a
cluster setting that can-core does not assert.

## Personal access tokens

A signed-in administrator opens **API tokens** in the admin UI (or `POST /api/v1/tokens`), picks an
enabled module, a name, the permissions (only ones the module declares in its manifest and that the
administrator holds through their own roles) and a lifetime of 1 hour to 365 days. The token
(`canpat_...`) is shown once; only its hash is stored. The administrator can revoke it at any time;
anyone with `rbac.manage` can list and revoke all.

The token acts as `user:<admin username>`. A module that receives it asks can-core
(`POST /v1/tokens/introspect`, with its own projected token) and is told the subject and permissions
only if the token is bound to that module: a token for `work` presented to `jobs` is indistinguishable
from a made-up one. The module then maps the permissions to its own model:

| Module | Mapping |
|---|---|
| work, finance, forms | the module's own permissions, directly |
| jobs | the strongest role the permissions fully cover: `jobs.admin` admin; `jobs.read`, `jobs.write`, `ats.read`, `ats.write`, `ats.hire` recruiter; `jobs.read`, `ats.read`, `ats.write`, `ats.interview` hiring manager; `ats.interview` interviewer. Otherwise refused. |
| people | `people.read` employee; plus `performance.write` without `people.sensitive-write` manager; `people.write` hr; `people.admin` admin. A person is linked to a token by setting their `sso_subject` to `user:<username>`. Manager access cannot be told apart from a plain employee set by permissions alone beyond that rule: SSO sessions are the proper answer. |

Behaviour to rely on: revocation reaches a module within 30 seconds (its cache); an expired token stops
at expiry even inside that window; when can-core cannot be reached a personal token is refused (the
module's static bootstrap token still works); a flood of bad tokens never slows a valid one (bad tokens
are refused locally by shape, cached negatively for 5 seconds, and the modules' failure limiters count
failures only).

## Events

Installed modules publish to can-core (`POST /v1/events`, with the outbox envelope id as `source_id`, so
a retry is not delivered twice) instead of a `webhook.url` (which stays for standalone use). can-core
delivers to the modules whose manifest `subscribes`, as signed webhooks to `/v1/can/events` on the
module's own listener. A module receives only what its manifest asks for. Module-to-module traffic goes
through can-core only.

| Event | From | To | What happens |
|---|---|---|---|
| `lead.created.v1` | forms | notification rule `role:sales` | an inbox item for people holding `forms:sales`. The event carries ids only: no name, e-mail address or message. |
| `candidate.hired.v1` | jobs | people | a person in onboarding ("New hire" and the candidate id: the event has no personal data, HR fills in the details) and the onboarding checklist, once per candidate; a default template is created if none starts on onboarding |
| `approval.decided.v1` | can-core | jobs, finance | the decision of a requisition or invoice request, delivered only to the module that asked |
| `person.*` | people | any subscriber | a person on leave is `active` in every event and no event is published for the change |

Delivery is at least once. can-core retries with backoff (8 attempts, then dead, visible in the admin
UI under Events); receivers verify the signature and a 5-minute replay window and ignore ids they have
seen.

## Approvals

Both flows go through can-core's approvals engine; the module code depends on an interface
(`modclient.Approvals`), not on the engine.

* **Jobs requisitions** (`POST /v1/jobs/{id}/approval-requests`): needs a personal access token (the
  requester must be a person; the bootstrap and service tokens are refused). The engine routes to the
  chain `[admin]` (people holding `jobs:admin`); when it decides, jobs sets the requisition approved or,
  for rejected, cancelled or expired, back to rejected so it can be asked again. If can-core lists the
  requester among the approvers, jobs records it as rejected.
* **Finance invoices** (`POST /v1/invoices/{id}/request-approval`, then `POST .../approve`): requesting
  needs `finance.write` and a personal access token; the chain is `[finance]`. `approve` then needs an
  approved request for the invoice's current version, from a caller other than the requester, the
  invoice's creator and whoever last edited it. Editing the invoice invalidates the approval. The invoice
  is still approved by an explicit call because that call numbers the invoice and records who approved
  it. Timesheets, expenses, purchase orders and voiding keep the `finance.approve` rule.

When can-core is unreachable a request fails (503 for finance, an error for jobs) and nothing is
approved.

## What ran where

* Unit and end-to-end tests (`make test`, including `test/wiring`): everything above, with the real
  can-core services (store, registry, events, webhook dispatcher, RBAC, approvals, notifications, tokens,
  module API) and the real modules, with the network and Kubernetes' TokenReview faked, and the
  installer against a fake Kubernetes API.
* The advisory `dev-cluster` workflow (not yet run when this was written) builds `can-work`, installs it
  through can-core on k3d from `company.yaml`, issues a personal access token through the admin API,
  creates a project and an issue with it, revokes the token, disables the module and checks that the
  volume remains. **It does not run forms, people, jobs or finance, event delivery, approvals or the
  admin UI page.** A pass shows the chart, the Secrets, the projected token and the introspection work on
  a real cluster for `work`; it says nothing about the other four.

## Measured

Idle RSS of the can-core binary, started on a throwaway database with fake token review: 41.9 MiB
before this epic and 41.9 MiB after; after 4,000 requests 60.2 MiB before and 60.9 MiB after
(declared `ram_mb` 48, limit 96, soft Go limit 64). `can-work` idle: 13.5 MiB before, 13.6 MiB after.
Measured on a development machine, not on a cluster node.

## What is not done

* **forms has no route to the internet through can-core.** The installer only routes
  `<subdomain>.<domain>`; forms' public form post is meant to be a same-origin path of the company site.
  forms also has no durable event outbox: if can-core is down for the four attempts the lead is stored
  but the `lead.created.v1` event is dropped.
* **finance** wires invoices only. Its public invoice link is not routed for the same reason.
* **people** does not fetch the new hire's name and contact details from the `ats` capability; HR
  completes the placeholder record.
* **Manager and reporting-line facts** cannot ride on a personal access token (see the people mapping).
* company.yaml `settings` are not mapped onto the charts' `config:` blocks.
* Image digests are placeholders until a release; the images other than `work` have never been built in CI.
* A failed rollout after a secret rotation is rolled back to the old pods, which keep the old values
  until the next successful enable; the Secret already holds the new ones.
* Nothing here encrypts Kubernetes Secrets at rest or protects against someone with `kubectl` access to
  `mod-*` namespaces, who can read them.
