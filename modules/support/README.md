# support

> **Status: experimental.** Installed through can-core on k3d in the advisory `wrapped-modules` CI job
> ([specs/041](../../specs/041-wrapped-modules-live/spec.md)); never on a real server, domain or mail relay. Backups are not run, and the
> `tickets` capability is marked `planned` (no adapter yet).

Shared support inbox. Wraps [FreeScout](https://github.com/freescout-help-desk/freescout) (PHP),
provides the `tickets` capability and publishes `ticket.created.v1`.

| | |
|---|---|
| Licence | AGPL-3.0 (`AGPL-3.0-only` in SPDX). Network copyleft: see [modules/README.md](../README.md#licences-and-network-copyleft). We run it unmodified. |
| Image | `pikapods/docker-freescout:1.8.252-r2`, pinned by digest. FreeScout publishes no image: this is a **third party** build, non-root, **amd64 only** |
| Footprint | 192 MB request, 672 MB limit, 2 GiB disk. Measured 90 to 115 MB idle locally, 140 MB while migrating, plus the shared Postgres (36 to 50 MB) |
| Database | Postgres (can-core's shared server in `can-data`, own database and role) |
| Public host | `support.<domain>` (the whole app, including its sign-in page) |
| Sign in | `admin@<domain>` (setting `admin_email`, used on the first start only); password below |
| Email | the company relay is written into FreeScout's **system** mail settings; mailboxes are configured by hand: [docs/email-policy.md](../../docs/email-policy.md) |
| Capability | `tickets` (`schemas/capabilities/tickets.v1.json`), served by `support-adapter` (:8081, off until the adapter image exists) |
| Backup | `hooks/backup.sh` (`pg_dump`) and `hooks/restore.sh`; **not run yet** (the hooks image is not published). Attachments live on the PVC |
| Data classes | `support-conversation`, `support-attachment`: 3 years, standard sensitivity |

The password is generated for each installation:

```sh
kubectl -n mod-support get secret can-module -o jsonpath='{.data.CAN_SUPPORT_ADMIN_PASSWORD}' | base64 -d
```

Change it in FreeScout after the first sign-in; the Secret is not updated and not used again.

FreeScout's knowledge base is a paid add-on; the public help centre comes from the wiki module.
SSO is `none`: FreeScout has no OIDC in its free core.

## Email

- With a relay configured ([docs/email.md](../../docs/email.md)) and `email.modules.support.enabled`
  not `false`, invitations, password resets and agent notifications use it. The settings are written
  again at every pod start. This mail does not pass `email.send`: no allow-list, quota or delivery log.
- Replies to customers and fetching mail are per mailbox: open the mailbox's connection settings and
  enter the SMTP and IMAP details. The relay is not copied there.
- A relay configured after the module was enabled arrives at the next install or upgrade of the module.
- Delivery through a real relay has not been tried.

## Open items

- Inbound-parse webhook (`inbound.mode: webhook`) needs the adapter, which does not exist.
- Upgrades to a newer FreeScout are untested; the image is not updated automatically.
- `settings.schema.json` accepts `support_address`, `inbound` and `retention_days`; they are not applied.
