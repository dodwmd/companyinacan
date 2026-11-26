# crm

> **Status: not deployable.** The official EspoCRM image serves Apache on port 80 and its README does
> not name a non-root user, so the `restricted` module namespace is expected to refuse it; its release
> tag is unresolved. Do not enable on a real server until a non-root wrapper image exists
> (specs/019-wiki-crm-status tasks T031, T041). `can validate` warns (`semantic.module-status`), and
> the `crm` capability and the lead subscription are `planned` (adapter not built).

Contacts, accounts, deals and a pipeline. Wraps [EspoCRM](https://github.com/espocrm/espocrm) (PHP) on
the shared MariaDB and provides the `crm` capability. Consumes `leads` and, once the adapter exists,
subscribes to `lead.created.v1` so a form submission becomes a contact.

| | |
|---|---|
| Licence | AGPL-3.0 (`AGPL-3.0-only`; GitHub reports AGPL-3.0). Network copyleft: see [modules/README.md](../README.md#licences-and-network-copyleft). We run it unmodified; a patched image obliges you to offer its source to users of the running service. Upstream may attach attribution terms (for example keeping its footer notice): unverified, read its LICENSE before white-labelling. |
| Footprint (estimate) | 350 MB request (web 208 + scheduled-job daemon 96 + adapter 40 MB), 640 MB limit, 5 GiB disk. SPEC.md section 6.1's ~350 MB for EspoCRM; **not measured**. Plus the shared MariaDB. |
| Database | MariaDB (shared instance in `can-data`, own database and user) |
| Public host | `crm.<domain>` |
| SSO | `none`. EspoCRM supports OIDC in its own settings, but the module does not wire it (unverified). |
| Email | needs `email.send`; the relay credentials are entered in EspoCRM's outbound email settings (not read from the environment). [docs/email-policy.md](../../docs/email-policy.md) |
| Capability | `crm` (`schemas/capabilities/crm.v1.json`), served by `crm-adapter` (:8081, off until the adapter image exists) |
| Lead flow | `lead.created.v1` to `crm-adapter` `/hooks/lead`, **planned**: [design](../../specs/019-wiki-crm-status/contracts/lead-to-crm-adapter.md). No public route; can-core delivers it in-cluster |
| Backup | `hooks/backup.sh` (`mariadb-dump --single-transaction`) and `hooks/restore.sh` through the can-core dump endpoint; attachments live on the PVC and are covered by the raw PVC backup |
| Data classes | `crm-contact` 3 years from last activity, `crm-activity` 3 years, `crm-deal` 7 years; all standard sensitivity |

## Personal data and erasure

A CRM holds personal data of people outside the company: names, emails, phone numbers, notes, mail
history. It is declared `standard`; this module must never hold sensitive classes.

- **Erasure on request.** `crm.v1` has an `erase_contact` verb (adapter, planned). Until the adapter
  exists, erase by hand in EspoCRM: delete the contact, then its notes, emails and meetings, then the
  contact's rows in the audit log if audit logging is on. Deals are kept but the contact link is removed.
  Forms publishes `lead.erased.v1` (lead id only) when a submission is erased; the subscription is planned with
  the adapter, so nothing here is erased by it yet.
- **Backups.** An erased contact survives in earlier database dumps. The restore flow re-applies
  erasure tombstones (SPEC.md section 6.5) once a `purge` hook exists; that hook is **not built** (T033).
- **Retention.** `crm-contact` counts from the last activity. Automatic expiry is not built.
- **Lawful basis.** The lead adapter stores the form's free text as a note and records `source: form`;
  it never imports marketing consent it was not given (newsletter opt-in stays in the newsletter module).

## Alternatives considered

Detail and sources in research.md R-crm. Lighter: none that is a business CRM and OSI-licensed with
an active upstream. Heavier or different: **Twenty** (AGPL-3.0 with a commercial `ee` directory; needs
Postgres and Redis, well over 1 GB), **SuiteCRM** (AGPL-3.0, heavy), **Monica** (personal CRM, not a
sales pipeline), **Odoo CRM** (LGPL-3.0 community edition, but features split with a proprietary
Enterprise edition and the footprint is far larger). Each would be a sibling module providing `crm`.

## Open items (not verifiable offline)

- **Non-root image.** See the status note. Options: a small wrapper image that listens on 8080 as
  www-data, or a PHP-FPM variant behind a non-root web server. Not built (T031).
- **Pin the image.** `image.tag` and `image.digest` are `REPLACE_WITH_...` placeholders.
- **Variable names.** `ESPOCRM_DATABASE_*`, `ESPOCRM_ADMIN_*` and `ESPOCRM_SITE_URL` and the daemon
  entry point `docker-daemon.sh` come from the Docker Official Image README, unverified on a cluster.
  The optional websocket container is not shipped.
- **Daemon probes** are file-existence checks, not process checks.
