# newsletter

> **Status: experimental.** Never run on a cluster; the `newsletter` capability and the
> `lead.created.v1` subscription are marked `planned` (adapter not built, T031). Until the adapter
> exists the **SMTP/bulk-relay settings are manual**: paste the relay credentials into Listmonk
> Settings, SMTP; Listmonk does not read them from the environment.

Mailing lists, double opt-in and campaigns. Wraps [Listmonk](https://github.com/knadh/listmonk)
(Go), provides the `newsletter` capability, publishes `subscriber.added.v1`, and optionally
subscribes people who tick a newsletter box on a form (consumes `leads`, subscribes to
`lead.created.v1`).

| | |
|---|---|
| Licence | AGPL-3.0 (`AGPL-3.0-only` in SPDX). Network copyleft: see [modules/README.md](../README.md#licences-and-network-copyleft). We run it unmodified. |
| Footprint (estimate) | 50 MB idle request, 128 MB limit, 1 GiB disk (uploads). Figure from SPEC.md section 6.1; consistent with upstream's single-Go-binary design but **not measured**. A one-shot `migrate` init container (up to 96 MB) runs before the app and does not add to steady state. |
| Database | Postgres (shared instance in `can-data`, own database and user) |
| Public host | `newsletter.<domain>` (admin UI, subscription and unsubscribe pages) |
| Email | needs `email.send`. A separate bulk relay is recommended: [docs/email-policy.md](../../docs/email-policy.md) |
| Capability | `newsletter` (`schemas/capabilities/newsletter.v1.json`), served by `newsletter-adapter` (:8081, off until the adapter image exists) |
| Backup | `hooks/backup.sh` (`pg_dump`) and `hooks/restore.sh` through the can-core dump endpoint; uploads on the PVC are covered by the raw PVC backup |
| Data classes | `newsletter-subscriber` 3 years after last activity, `campaign-engagement` 2 years, standard sensitivity |

## Consent

The capability refuses a subscribe request without a recorded consent (`given_at`, `source`).
Double opt-in is on by default (setting `double_opt_in`); a subscriber stays `unconfirmed`
and receives no campaign until they confirm. Subscribing from a form (`lead.created.v1`) is
limited to forms listed in `subscribe_from_forms` and requires the opt-in field.

## Open items (not verifiable offline)

- **Pin the image.** `image.digest` is a `REPLACE_WITH_DIGEST` placeholder and `image.tag` is the
  5.x tag known when this was written; verify the newest 5.x release when pinning.
- **Non-root run.** The chart bypasses the image entrypoint (`command: /listmonk/listmonk`) so
  that no privilege drop is needed; confirm with a real run that `/listmonk/uploads` on the PVC is
  writable for uid 1000.
- **Environment names.** `LISTMONK_app__address`, `LISTMONK_db__*` and `LISTMONK_ADMIN_USER/PASSWORD`
  are from upstream's documentation as remembered; `--install --idempotent --yes` and `--upgrade --yes`
  likewise.
- **SMTP settings** live in Listmonk's database; applying the relay credentials needs the API
  (adapter, deferred).
