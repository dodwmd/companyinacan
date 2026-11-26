# newsletter

> **Status: experimental.** Installed through can-core on k3d in the advisory `wrapped-modules` CI job
> ([specs/041](../../specs/041-wrapped-modules-live/spec.md)); never on a real server or mail relay. Backups are not run; the `newsletter`
> capability and the `lead.created.v1` subscription are marked `planned` (adapter not built, T031). The
> **SMTP/bulk-relay settings are manual**: enter the relay in Listmonk Settings, SMTP. can-core gives
> this module no mail credentials.

> **Backups: enabling this module stops them for the whole company.** Its hook needs the `can-hooks`
> image, which is not published, and while such a module is enabled every dump run fails and nothing is
> backed up (`can validate` warns; [docs/backup-restore.md](../../docs/backup-restore.md)).

Mailing lists, double opt-in and campaigns. Wraps [Listmonk](https://github.com/knadh/listmonk)
(Go), provides the `newsletter` capability, publishes `subscriber.added.v1`, and optionally
subscribes people who tick a newsletter box on a form (consumes `leads`, subscribes to
`lead.created.v1`).

| | |
|---|---|
| Licence | AGPL-3.0 (`AGPL-3.0-only` in SPDX). Network copyleft: see [modules/README.md](../README.md#licences-and-network-copyleft). We run it unmodified. |
| Image | `listmonk/listmonk:v6.2.0` (upstream), pinned by digest |
| Footprint | 50 MB request, 128 MB limit, 1 GiB disk (uploads). Measured 26 to 27 MiB idle on k3d, 31 to 35 MB locally, plus the shared Postgres. A one-shot `migrate` init container (up to 96 MB) runs before the app |
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

## First sign-in

User `admin`; the password is generated for each installation:

```sh
kubectl -n mod-newsletter get secret can-module -o jsonpath='{.data.CAN_NEWSLETTER_ADMIN_PASSWORD}' | base64 -d
```

Change it in Listmonk afterwards; the Secret is not used again.

## Open items

- A new installation has Listmonk's example SMTP server: set a real one before sending.
- Set the root URL in Listmonk's settings to `https://newsletter.<domain>`, or links in mail are wrong.
- The settings of `settings.schema.json` are accepted but not applied.
- Upgrades to a newer Listmonk are untested.
