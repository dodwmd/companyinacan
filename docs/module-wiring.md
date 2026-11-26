# Module wiring: installing and connecting the in-house modules

How the in-house modules (forms, work, people, jobs, finance) are installed by can-core and how they
talk to it and to each other. Specification: `specs/016-module-wiring/`. Status: built and tested
offline with the real can-core services and the real modules; **on a cluster, only `work` has run, in
the advisory dev-cluster workflow** (see "What ran where").

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
Web UIs, APIs and the forms route are created by can-core, not the chart: see
[`module-routing.md`](module-routing.md).
Per module:

| Module | Service and port | Public route | Needs |
|---|---|---|---|
| work | `work` 8080 | none; `work.<domain>` with `expose.ui` | |
| people | `people-api` 8081 | none; `people.<domain>` with `expose.ui` | keyring |
| jobs | `jobs` 8080 | `careers.<domain>`: only `/careers`, `/feeds`, `/robots.txt`; `jobs.<domain>` with `expose.ui` | keyring; can-core sets `company.name` and `company.base_url` from `company.yaml` |
| finance | `finance` 8080 | none; `finance.<domain>` with `expose.ui` | |
| forms | `forms` 8080 public, `forms-api` 8081 | `/_forms` on the site hosts | at least one form in the chart `config:` |

All run one replica with the `Recreate` strategy (SQLite on a ReadWriteOnce volume), as non-root with a
read-only root filesystem, resources equal to the manifest's `ram_mb` and `ram_limit_mb`, readiness on
`/healthz` and liveness running the binary's own `healthcheck`. The volume is `<id>-data` and survives
uninstall and disable. Configuration is the `config:` block of the chart's `values.yaml`, rendered into a
ConfigMap, over which company.yaml's settings and facts are laid ([Settings](#settings)). Image references are
`repository:tag` plus a digest that is a placeholder until release; `make release-check` fails while
any remains.

## Settings

`modules.<id>.settings` of `company.yaml` reach the module through a mapping declared beside each property of its
`settings.schema.json` (`x-can`; [specs/064-module-settings](../specs/064-module-settings/spec.md)). `config.` below is
the module's configuration file. Secrets are never settings.

```yaml
organisation:                      # optional: facts modules print or compute with
  legal_name: Acme Pty Ltd         # default: name
  address: |
    1 Anvil Way
    Brisbane QLD 4000
  tax_id: ABN 00 000 000 000
  currency: AUD
  timezone: Australia/Brisbane
modules:
  finance:
    settings:
      payment_instructions: "BSB 000-000, account 12345678"
      retention_years: 10
```

A module setting wins over a company fact, a fact over the module's default, and what can-core sets itself
(connections, routes, public addresses) over all of them. An unknown or invalid setting is **not** applied: the module
runs with the fact or the default, and `can validate`, `can apply` and the admin UI warn. A change is planned as an
upgrade. An upgrade that widens what the module does, compared with what it was installed with, waits for approval
(the "approval" column); this includes a setting that was written before settings were mapped and takes effect only
now, and removing a setting whose default is looser than the value it had. Each line to approve shows the new value, or
the entry a list gains or loses, and a digest of the whole value: an approval covers that value only.

**Restore.** `can restore` asks nobody, so it installs nothing wider than what the backup recorded as installed: a
setting that widens (and equity's break-glass window, or leaving its directory mode) stays as it was, or at the module's
default where nothing was recorded. The apply that starts the modules does the same; the next one lists the difference
and holds it for approval. A module that was disabled or failed at backup time gets the same on its first apply.

**Re-enabling.** Enabling a disabled module is not held, as for a new permission in its manifest: it applies
`company.yaml` as it is, including settings that widen compared with its last installation. The plan (`can apply --plan`,
the admin UI preview) lists them as "newly allowed" first.

### What is applied

| Module | Setting | Goes to | Notes |
|---|---|---|---|
| `analytics` | `admin_email` | read by the chart, when the site is first created |  |
| `board` | `retention.board-minutes` | `config.retention.board-minutes` |  |
| `board` | `retention.board-resolutions` | `config.retention.board-resolutions` |  |
| `board` | `retention.board-audit` | `config.retention.board-audit` |  |
| `board` | `retention.board-packs` | `config.retention.board-packs` | approval: data of this class is deleted sooner |
| `board` | `retention.board-private` | `config.retention.board-private` | approval: data of this class is deleted sooner |
| `entity` | `retention.entity-register` | `config.retention.entity-register` |  |
| `entity` | `retention.entity-filings` | `config.retention.entity-filings` |  |
| `entity` | `retention.entity-audit` | `config.retention.entity-audit` |  |
| `entity` | `retention.entity-person` | `config.retention.entity-person` | approval: data of this class is deleted sooner |
| `equity` | `identity` | can-core connects equity to the people directory (specs/057-module-capability-wiring) |  |
| `equity` | `break_glass_until` | can-core sets the chart value breakGlassUntil (specs/057-module-capability-wiring) |  |
| `equity` | `retention.equity-ledger` | `config.retention.equity-ledger` |  |
| `equity` | `retention.equity-audit` | `config.retention.equity-audit` |  |
| `equity` | `retention.equity-stakeholder` | `config.retention.equity-stakeholder` | approval: data of this class is deleted sooner |
| `equity` | `retention.equity-grants` | `config.retention.equity-grants` | approval: data of this class is deleted sooner |
| `equity` | `retention.equity-valuation` | `config.retention.equity-valuation` | approval: data of this class is deleted sooner |
| `files` | `max_file_mb` | `config.limits.max_file_bytes` |  |
| `files` | `quota_gb_per_owner` | `config.limits.quota_bytes_per_owner` |  |
| `files` | `trash_days` | `config.retention.trash_days` | approval: deleted files are purged sooner |
| `files` | `scanner_required` | `config.scanner.required` | approval: uploads are accepted without a virus scan |
| `files` | `inline_images` | `config.download.inline` | approval: uploaded images are shown in the browser instead of downloaded |
| `files` | `share.max_ttl_hours` | `config.share.max_ttl_hours` | approval: share links can stay open longer |
| `files` | `ui.idle_minutes` | `config.ui.idle_minutes` | approval: idle web sessions stay signed in longer |
| `files` | `ui.absolute_hours` | `config.ui.absolute_hours` | approval: web sessions last longer |
| `finance` | `base_currency` | `config.base_currency` | default: `organisation.currency`; approval: the reporting currency changes, and FX rates already stored are against the old one |
| `finance` | `payment_instructions` | `config.payment_instructions` |  |
| `finance` | `seller.name` | `config.seller.name` | default: `organisation.legal_name`, `name` |
| `finance` | `seller.address` | `config.seller.address` | default: `organisation.address` |
| `finance` | `seller.tax_id` | `config.seller.tax_id` | default: `organisation.tax_id` |
| `finance` | `public_base_url` | `config.public_base_url` | approval: the invoice links sent to customers point there |
| `finance` | `payment_provider` | `config.payment_provider` | approval: a development stand-in answers as if it were a real provider |
| `finance` | `esign_provider` | `config.esign_provider` | approval: a development stand-in answers as if it were a real provider |
| `finance` | `retention_years` | `config.retention_years` | approval: financial records are deleted sooner |
| `finance` | `audit_retention_days` | `config.audit_retention_days` | approval: the audit trail is deleted sooner |
| `finance` | `trust_proxy` | `config.trust_proxy` | approval: client addresses are taken from a request header |
| `finance` | `max_body_bytes` | `config.max_body_bytes` |  |
| `finance` | `rate_limit_per_minute` | `config.rate_limit.per_minute` | approval: the request limit per API token is switched off |
| `finance` | `auth_failures_per_minute` | `config.rate_limit.auth_failures_per_minute` | approval: more password and link guessing is let through |
| `finance` | `signing_record_ip` | `config.signing_record_ip` | approval: more of signers' IP addresses is stored |
| `finance` | `ui.idle_minutes` | `config.ui.idle_minutes` | approval: idle web sessions stay signed in longer |
| `finance` | `ui.absolute_hours` | `config.ui.absolute_hours` | approval: web sessions last longer |
| `forms` | `trust_proxy` | `config.trust_proxy` |  |
| `forms` | `max_body_bytes` | `config.max_body_bytes` |  |
| `forms` | `rate_limit_per_minute` | `config.rate_limit.per_minute` | approval: one address may submit more often |
| `forms` | `retention_days` | `config.retention_days` | approval: submissions are deleted sooner |
| `forms` | `cors_allowed_origins` | `config.cors_allowed_origins` | approval: pages of another origin may submit the forms from a visitor's browser |
| `forms` | `notify_email` | `config.notify_email` | approval: this address is emailed the content of new submissions |
| `jobs` | `approval_required` | `config.approval.required` | approval: jobs open without an approved requisition |
| `jobs` | `collect_eeo` | `config.apply.collect_eeo` | approval: diversity data about applicants is collected |
| `jobs` | `retention.rejected_days` | `config.retention.rejected_days` | approval: rejected candidates are deleted sooner |
| `jobs` | `retention.talent_pool_days` | `config.retention.talent_pool_days` | approval: talent pool candidates are deleted sooner |
| `jobs` | `retention.hired_days` | `config.retention.hired_days` | approval: hired candidates' records are deleted sooner |
| `jobs` | `retention.eeo_days` | `config.retention.eeo_days` | approval: diversity counts are deleted sooner |
| `jobs` | `retention.closed_job_days` | `config.retention.closed_job_days` | approval: closed jobs are deleted sooner |
| `jobs` | `max_resume_mb` | `config.apply.max_resume_bytes` |  |
| `jobs` | `scanner_required` | `config.apply.scanner_required` | approval: resumes are accepted without a virus scan |
| `jobs` | `apply_per_hour` | `config.rate_limit.apply_per_hour` | approval: one address may apply more often |
| `jobs` | `default_valid_days` | `config.apply.default_valid_days` |  |
| `jobs` | `consent_version` | `config.apply.consent_version` | approval: another consent version is recorded for applicants |
| `jobs` | `consent_text` | `config.apply.consent_text` | approval: applicants consent to different wording |
| `jobs` | `eeo_min_cell` | `config.eeo.min_cell` | approval: smaller groups of applicants can be told apart in diversity reports |
| `jobs` | `ui.idle_minutes` | `config.ui.idle_minutes` | approval: idle web sessions stay signed in longer |
| `jobs` | `ui.absolute_hours` | `config.ui.absolute_hours` | approval: web sessions last longer |
| `people` | `retention.directory-basic` | `config.retention.directory-basic` | approval: data of this class is deleted sooner |
| `people` | `retention.people-sensitive` | `config.retention.people-sensitive` | approval: data of this class is deleted sooner |
| `people` | `retention.people-checklists` | `config.retention.people-checklists` | approval: data of this class is deleted sooner |
| `people` | `retention.performance-oneonone` | `config.retention.performance-oneonone` | approval: data of this class is deleted sooner |
| `people` | `retention.performance-goals` | `config.retention.performance-goals` | approval: data of this class is deleted sooner |
| `people` | `retention.performance-reviews` | `config.retention.performance-reviews` | approval: data of this class is deleted sooner |
| `people` | `retention.people-timeoff` | `config.retention.people-timeoff` | approval: data of this class is deleted sooner |
| `people` | `reviews.min_responses` | `config.reviews.min_responses` | approval: fewer answers hide who gave feedback |
| `people` | `ui.idle_minutes` | `config.ui.idle_minutes` | approval: idle web sessions stay signed in longer |
| `people` | `ui.absolute_hours` | `config.ui.absolute_hours` | approval: web sessions last longer |
| `provisioning` | `dry_run` | `config.workflows.dry_run` | approval: connectors are called: accounts in other systems are changed |
| `provisioning` | `privileged_groups` | `config.workflows.privileged_groups` | approval: access of a group no longer needs a second person |
| `provisioning` | `delete_after_days` | `config.workflows.delete_after_days` | approval: leavers' downstream accounts are deleted (sooner) |
| `provisioning` | `timezone` | `config.workflows.timezone` | default: `organisation.timezone`; approval: leavers lose their access at a different time |
| `provisioning` | `leaver_cutoff` | `config.workflows.leaver_cutoff` | approval: a leaver keeps access through their last day |
| `provisioning` | `compensate_with_delete` | `config.workflows.compensate_with_delete` | approval: failed joiner workflows delete accounts |
| `provisioning` | `tasks.joiner_template` | `config.tasks.joiner_template` | approval: different manager and IT tasks are created for joiners |
| `provisioning` | `tasks.leaver_template` | `config.tasks.leaver_template` | approval: different manager and IT tasks are created for leavers |
| `support` | `admin_email` | can-core reads it (specs/059-support-mailbox) |  |
| `support` | `support_address` | can-core reads it (specs/059-support-mailbox) |  |
| `support` | `mailbox_name` | can-core reads it (specs/059-support-mailbox) |  |
| `support` | `inbound.mode` | can-core reads it (specs/059-support-mailbox) |  |
| `support` | `inbound.imap.host` | can-core reads it (specs/059-support-mailbox) |  |
| `support` | `inbound.imap.port` | can-core reads it (specs/059-support-mailbox) |  |
| `support` | `inbound.imap.username` | can-core reads it (specs/059-support-mailbox) |  |
| `support` | `inbound.imap.encryption` | can-core reads it (specs/059-support-mailbox) |  |
| `support` | `inbound.imap.folder` | can-core reads it (specs/059-support-mailbox) |  |
| `wiki` | `php_memory_limit` | `app.phpMemoryLimit` |  |
| `work` | `trust_proxy` | `config.trust_proxy` | approval: client addresses are taken from a request header |
| `work` | `max_body_bytes` | `config.max_body_bytes` |  |
| `work` | `rate_limit_per_minute` | `config.rate_limit.per_minute` | approval: the request limit per API token is switched off |
| `work` | `auth_failures_per_minute` | `config.rate_limit.auth_failures_per_minute` | approval: more password guessing is let through |
| `work` | `retention_days` | `config.retention_days` | approval: resolved issues are deleted sooner |
| `work` | `audit_retention_days` | `config.audit_retention_days` | approval: the audit trail is deleted sooner |

### What is not applied

| Module | Setting | Why |
|---|---|---|
| `analytics` | `site_domain` | the tracked site is the module's own host |
| `analytics` | `retention_days` | it is configured inside the application, and can-core does not write it |
| `crm` | `from_address` | it is configured inside the application, and can-core does not write it |
| `crm` | `lead_intake.enabled` | the module's adapter, which would apply it, is not built |
| `crm` | `lead_intake.form_ids` | the module's adapter, which would apply it, is not built |
| `crm` | `contact_retention_days` | it is configured inside the application, and can-core does not write it |
| `finance` | `webhook_url` | an installed module's events go to can-core, and no webhook secret is provisioned |
| `forms` | `captcha_provider` | the provider needs a secret that can-core does not provision, and no installed form asks for a captcha |
| `forms` | `webhook_url` | an installed module's events go to can-core, and no webhook secret is provisioned |
| `newsletter` | `from_address` | it is configured inside the application, and can-core does not write it |
| `newsletter` | `double_opt_in` | it is configured inside the application, and can-core does not write it |
| `newsletter` | `relay` | it is configured inside the application, and can-core does not write it |
| `newsletter` | `subscribe_from_forms` | the module's adapter, which would apply it, is not built |
| `newsletter` | `default_list` | the module's adapter, which would apply it, is not built |
| `status` | `page_slug` | it is configured inside the application, and can-core does not write it |
| `status` | `heartbeat_retention_days` | it is configured inside the application, and can-core does not write it |
| `status` | `external_probe_hint` | nothing reads it |
| `support` | `inbound.webhook.provider` | the inbound-parse webhook is not built |
| `support` | `retention_days` | it is configured inside the application, and can-core does not write it |
| `wiki` | `wiki_title` | it is configured inside the application, and can-core does not write it |
| `wiki` | `public_help_centre` | it is configured inside the application, and can-core does not write it; the help namespace stays private until its access list is set there |
| `wiki` | `revision_retention_days` | it is configured inside the application, and can-core does not write it |
| `work` | `webhook_url` | an installed module's events go to can-core, and no webhook secret is provisioned |

The forms themselves (fields, which forms exist) are not settings: they are the forms chart's `config.forms`.

## Secrets

can-core creates these in the module's `can-module` Secret the first time the module is enabled and
never replaces them afterwards (enabling again, upgrading, disabling and enabling again all keep them):

| Module | Keys |
|---|---|
| work | `CAN_WORK_ADMIN_TOKEN` |
| forms | `CAN_FORMS_TOKEN_KEY`, `CAN_FORMS_API_TOKEN` (reads, changes and **erases** leads), `CAN_FORMS_API_READ_TOKEN` |
| finance | `CAN_FINANCE_ADMIN_TOKEN`, `CAN_FINANCE_LINK_SECRET`, `CAN_FINANCE_SEAL_KEY` (E15c: seals bank account numbers; never rotated; an install that predates it needs the key added to its Secret once, otherwise adding a bank account answers 503) |
| jobs | `CAN_JOBS_ADMIN_TOKEN`, `CAN_JOBS_HASH_KEY`, `CAN_JOBS_FORM_SECRET`, `keyring` |
| people | `CAN_PEOPLE_API_TOKEN`, `CAN_PEOPLE_DIRECTORY_TOKEN`, `CAN_PEOPLE_LOOKUP_TOKEN`, `keyring` |

Generated values are written with create-if-absent (an HTTP create, not a forced apply): if a second
can-core creates the Secret first, this one adopts its values instead of overwriting them. The
generated database password follows the same rule. The webhook signing secret is only regenerated
when the module has no record; any other store error fails the enable.

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
administrator holds through their own roles) and a lifetime of 1 hour up to the maximum (90 days by default; `CAN_PAT_MAX_TTL_DAYS`, at most 365, changes it). One administrator can hold 10 live tokens per module, and a module 50. The token
(`canpat_...`) is shown once; only its hash is stored. The administrator can revoke it at any time;
anyone with `rbac.manage` can list and revoke all.

The token acts as `user:<admin username>`. A module that receives it asks can-core
(`POST /v1/tokens/introspect`, with its own projected token) and is told the subject and permissions
only if the token is bound to that module: a token for `work` presented to `jobs` is indistinguishable
from a made-up one. The module then maps the permissions to its own model:

| Module | Mapping |
|---|---|
| work, finance, forms | the module's own permissions, directly |
| jobs | the strongest role the permissions fully cover: every jobs permission including `jobs.admin`: admin (`jobs.admin` alone is refused); `jobs.read`, `jobs.write`, `ats.read`, `ats.write`, `ats.hire` recruiter; `jobs.read`, `ats.read`, `ats.write`, `ats.interview` hiring manager; `ats.interview` interviewer. Otherwise refused. |
| people | `people.read` employee; plus `people.team.manage` (only the manifest's manager role carries it) manager; `people.write` hr; `people.admin` admin. A person is linked to a token by setting their `sso_subject` to `user:<username>`; someone else sets it (nobody changes their own link; the first one is made with the people service token). Roles follow only from permissions that are present, so a token with a subset of someone's permissions never gets a role they lack. |

A token never outlives its issuer's rights: at every introspection can-core checks that the issuing
administrator still exists and intersects the token's permissions with what that administrator holds
now (removing a role takes the permission off their tokens; deleting the account makes them inactive;
a failure of the check drops the permission). Tokens are not revoked in the database by those events,
they simply stop being answered.

A token's permissions also restrict what the module lets its role do: in people a token with
`people.write` but not `people.sensitive-read` / `-write` cannot reach date of birth, national id,
home address, personal phone or salary, `people.admin` alone cannot create or edit people, and
erasure, legal hold and retention need `people.admin`; performance data needs `performance.read` /
`.write`. A role is never granted to a token that holds less than the role (jobs: `jobs.admin` alone
is refused; admin is the whole set; recruiter and admin need `ats.interview` as well, because they
score applications). In people the hr and admin paths of creating a checklist and completing someone
else's task need `people.write`. Self-service writes are deliberately not gated by `people.write`: a
person's own `preferred_name` / `work_phone` and completing a task assigned to them are open to any
token that maps to the employee role (`people.read`), because the manifest's employee role does not
carry `people.write` and gating would lock every employee out of their own entry.

**What an issuer holds.** In finance and work `*.admin` implies every permission of the module (and
`write`/`approve` imply `read` in finance). Issue and introspection expand that the same way: an
issuer whose role has only `work.admin` may issue a token with `work.read`, and the token stays
active exactly as long as the issuer holds `work.admin` or the permission itself. jobs and people
admin permissions imply nothing and are never expanded.

**Lifetime.** Lowering `CAN_PAT_MAX_TTL_DAYS` only limits tokens issued afterwards; tokens already
issued keep their expiry. To end them early, revoke them (or remove the issuer's role or account).

**Subjects.** Personal access tokens can only be issued by a `user:` subject, the local administrator
accounts. An `oidc:` subject (single sign-on, E11b) cannot issue a PAT, and a PAT's subject is always
`user:<username>`. Note for the E11b OIDC change: if OIDC administrators are to issue tokens, the
issuer check in `core/server/pat` (`Issue`, and the `AdminByUsername` existence check at
introspection) must be extended to the new subject form together with a matching `Holds` lookup;
until then OIDC sessions use the admin UI but not the token page.

Behaviour to rely on: revocation, a removed role and a deleted user reach a module within 30 seconds (its cache; the RBAC answer behind it is invalidated at once); an expired token stops
at expiry even inside that window; when can-core cannot be reached a personal token is refused (the
module's static bootstrap token still works); a flood of bad tokens never slows a valid one (bad tokens
are refused locally by shape, cached negatively for 5 seconds, and the modules' failure limiters count
failures only).

## Signing in to a module's web page without a token (E11c)

A module that serves a web UI for people can accept **launch tickets** instead of pasted tokens. It declares
`runtime.web_login` in its manifest (work, people, jobs, finance and files do)
(`specs/036-module-ui-sso`, [`sso.md`](sso.md)). Module-API calls, all authenticated by the module's
projected token:

* The browser that launched also holds a nonce cookie, `__Secure-can_launch_<module id>`
  (`modclient.LaunchBinding` reads it; its result is opaque, give it to `PeekTicket` or `RedeemTicket` as is,
  and they try each candidate when several cookies of that name arrived). Send one nonce as `binding`; without
  it nothing works.
* `POST /v1/tickets/peek {"ticket", "binding"}` names the account (`subject`, `name`) without consuming, for the
  confirmation page. Never replace another account's session without asking.
* `POST /v1/tickets/redeem {"ticket", "binding"}` exchanges the ticket the browser brought (`cant_...`, 60 seconds, once,
  minted for this module only) for `{"active": true, "subject", "name", "permissions", "return_path",
  "session", "session_expires_at"}` or the identical `{"active": false}` for every failure. `permissions` are the
  module's manifest permissions the person holds right now; `name` is a display name (no email, no groups);
  `session` is an opaque handle to keep in memory only. **End your session at `session_expires_at`**, whatever
  your own timeouts say (it carries the identity provider's deadline). `authenticated_at` (also in revalidate;
  absent when unknown) is when the person last authenticated to the admin UI: require it to be recent before
  anything irreversible, and treat absent as not recent.
* `POST /v1/tickets/revalidate {"session"}` returns the same shape (without `session`) while the person is
  still enabled and holds at least one permission of the module, else `{"active": false}`. Ask on the
  interval your PAT sessions already use; any doubt (can-core unreachable, inactive) ends the session.

The in-house modules do this in `GET/POST <ui>/login/ticket`: the GET only shows a form naming the account (a scanner must not
spend the ticket), the POST redeems, creates the module's own session with a fresh id, maps `permissions` with
the same function the module uses for personal access tokens (a restricted actor: never more than a token
with those permissions), and redirects to a clean URL. `ui.token_login: false` removes the paste-a-token form.
`modclient.Client.PeekTicket` / `RedeemTicket` / `RevalidateSession` wrap the calls; `fakecore` fakes them.

## Connections between modules

A module that asks for a capability can-core grants gets network access to the providing module only, its
URL and a scoped token ([specs/057](../specs/057-module-capability-wiring/spec.md)). Today: `provisioning`
and, when asked for, `equity` to `people` (`directory`). Everything else between module namespaces stays closed, and the
connection is removed when either side is disabled. If the directory cannot be reached, equity refuses
what needs a person (503) and provisioning does not act.

**equity connects only when `company.yaml` says `modules.equity.settings.identity: directory`**; until
then it stays in attestation mode, and after that disabling `people` is refused until the setting is
changed back. Read [`equity.md`](equity.md#holder-identity) before opting in: actions can freeze until
records are linked. `break_glass_until` in the same settings opens a break-glass window.

## Events

Installed modules publish to can-core (`POST /v1/events`, with the outbox envelope id as `source_id`, so
a retry is not delivered twice) instead of a `webhook.url` (which stays for standalone use). can-core
delivers to the modules whose manifest `subscribes`, as signed webhooks to `/v1/can/events` on the
module's own listener. A module receives only what its manifest asks for. Apart from the connections above, module-to-module traffic goes
through can-core only.

| Event | From | To | What happens |
|---|---|---|---|
| `lead.created.v1` | forms | notification rule `role:sales` | an inbox item for people holding `forms:sales`. The event carries ids only: no name, e-mail address or message. |
| `candidate.hired.v1` | jobs | people | a person in onboarding ("New hire" and the candidate id: the event has no personal data, HR fills in the details) and the onboarding checklist, once per candidate; a default template is created if none starts on onboarding |
| `approval.decided.v1` | can-core | jobs, finance | the decision of a requisition or invoice request, delivered only to the module that asked |
| `person.*` | people | any subscriber | a person on leave is `active` in every event and no event is published for the change |

Delivery is at least once. can-core retries with backoff (8 attempts, then dead, visible in the admin
UI under Events); receivers verify the signature and a 5-minute replay window and ignore ids they have
seen. The seen-set is per process and bounded (4,096 ids): after a restart a redelivered event reaches
the handler again, so handlers are idempotent by themselves (people: per candidate id; jobs and finance:
only a pending request changes, once). Handlers also check the event's source (`jobs` for a hire,
`can-core` for an approval decision).

## Approvals

Both flows go through can-core's approvals engine; the module code depends on an interface
(`modclient.Approvals`), not on the engine.

* **Jobs requisitions** (manual approval through `POST /v1/jobs/{id}/approval` is closed while
  can-core decides: it answers 409, so nobody can approve behind the engine's back) (`POST /v1/jobs/{id}/approval-requests`): needs a personal access token (the
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

## Revoking the access of someone who left (E13d)

A module that provides `identity.provision` (the `provisioning` module) may call
`POST /v1/identity/revoke {party_id, subject}` on the module API to end the personal access tokens and
the sessions of one account. can-core allows it only for a person the **people module** reported
offboarded (it records `person.offboarded.v1` from that authenticated source, never from a request
field) whose `effective_date` has been reached, and only for the account the directory linked to them:
the event carries `subject_ref`, the first 32 hex characters of the SHA-256 of their `sso_subject`. The
call is idempotent, audited in the same transaction as the change (the party id and counts), and rate
limited to 120 per module and minute, and the caller must be the resolved enabled provider of `identity.provision`. It also disables the account (never the last enabled owner: 409) and drops its push subscriptions. For a termination that cannot wait for the person to be reported offboarded, an administrator with `rbac.manage` uses `POST /api/v1/identity/revoke-now` (an owner is needed to disable an owner; an owner re-enables with `POST /api/v1/identity/enable` or `can-core admin enable`). Refusals: 400 malformed, 403 module or account not allowed, 409 not
offboarded yet or not yet effective, 429 rate limited. See [`provisioning.md`](provisioning.md).

## The files module (E13e)

can-core generates `CAN_FILES_API_TOKEN`, `CAN_FILES_RESTORE_TOKEN`, `CAN_FILES_LINK_SECRET` and the sealed-field `keyring` for the `files`
module (`core/server/modsecrets`). Personal access tokens are introspected like the other in-house modules:
`kind` user, subject `user:<name>`, and the permissions `files.read`, `files.write` and `files.admin`
become the coarse gate; the role in the space decides the rest. The module publishes `file.uploaded.v1`,
`file.shared.v1`, `file.deleted.v1` and `file.infected.v1` through can-core and subscribes to nothing.
Its tombstones (`GET /v1/erasures`) and purge hook (`POST /v1/purge`) follow the erasure-ledger contract of
the other in-house modules: can-core collects the tombstones (`core/server/erasure`). The S3 credentials are not generated: see [`files.md`](files.md).

## What ran where

* Unit and end-to-end tests (`make test`, including `test/wiring`): everything above, with the real
  can-core services (store, registry, events, webhook dispatcher, RBAC, approvals, notifications, tokens,
  module API) and the real modules, with the network and Kubernetes' TokenReview faked, and the
  installer against a fake Kubernetes API.
* The advisory `dev-cluster` workflow builds `can-work`, installs it through can-core on k3d from
  `company.yaml`, issues a personal access token through the admin API, creates a project and an issue
  with it, revokes the token, disables the module and checks that the volume remains. On this epic's pull
  request it passed (`TestWorkModuleThroughCanCore`, 57 s, one run). **It does not run forms, people,
  jobs or finance, event delivery between modules, approvals or the admin UI page.** The pass shows the
  chart, the generated Secrets, the projected token and PAT introspection work on a real cluster for
  `work`; it says nothing about the other four.

## Measured

Idle RSS of the can-core binary, started on a throwaway database with fake token review: 41.9 MiB
before this epic and 41.9 MiB after; after 4,000 requests 60.2 MiB before and 60.9 MiB after
(declared `ram_mb` 48, limit 96, soft Go limit 64). `can-work` idle: 13.5 MiB before, 13.6 MiB after.
Measured on a development machine, not on a cluster node. In the cluster (advisory CI run, after the
whole integration suite including the `work` install): can-core RSS 49.0 MiB, high-water mark 55.1 MiB.

## What is not done

* **Approvals are partly wired**: jobs requisitions and finance invoices only. Finance timesheets,
  expenses, purchase orders and voiding, and every other module's approvals, still use their own rule.
* Invalid token lookups from one module beyond 300 a minute are delayed 200 ms each; valid tokens never.
* equity has its own authenticator: personal access tokens work, with the roles of [`equity.md`](equity.md).

* **forms** is routed at `/_forms` by can-core ([`module-routing.md`](module-routing.md)) but has no
  durable event outbox: if can-core is down for the four attempts the lead is stored but the
  `lead.created.v1` event is dropped.
* **finance** wires invoices only. Its public invoice link is routed only with
  `expose.public: true` ([`module-routing.md`](module-routing.md)).
* **people** does not fetch the new hire's name and contact details from the `ats` capability; HR
  completes the placeholder record.
* **Manager and reporting-line facts** cannot ride on a personal access token (see the people mapping).
* The forms definitions and several settings of the wrapped modules are not applied ([Settings](#settings)).
* Image digests are placeholders until a release; the images other than `work` have never been built in CI
  (`work` is built there with the base image digests of the day).
* A failed rollout after a secret rotation is rolled back to the old pods, which keep the old values
  until the next successful enable; the Secret already holds the new ones.
* Nothing here encrypts Kubernetes Secrets at rest or protects against someone with `kubectl` access to
  `mod-*` namespaces, who can read them.
