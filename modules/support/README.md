# support

Shared support inbox. Wraps [FreeScout](https://github.com/freescout-help-desk/freescout) (PHP),
provides the `tickets` capability and publishes `ticket.created.v1`.

| | |
|---|---|
| Licence | AGPL-3.0 (`AGPL-3.0-only` in SPDX). Network copyleft: see [modules/README.md](../README.md#licences-and-network-copyleft). We run it unmodified. |
| Footprint (estimate) | 250 MB idle request, 512 MB limit, 2 GiB disk. The 250 MB figure is SPEC.md section 6.1's catalogue estimate for the PHP stack (nginx, PHP-FPM, cron and queue worker in one container); **not measured**. FreeScout's own documentation lists no RAM figure. |
| Database | MariaDB (shared instance in `can-data`, own database and user) |
| Public host | `support.<domain>` |
| Email | needs `email.send` (SMTP relay credentials injected as the `can-smtp` Secret). Inbound by IMAP or inbound-parse webhook: [docs/email-policy.md](../../docs/email-policy.md) |
| Capability | `tickets` (`schemas/capabilities/tickets.v1.json`), served by `support-adapter` (:8081, off until the adapter image exists) |
| Backup | `hooks/backup.sh` (`mariadb-dump --single-transaction`) and `hooks/restore.sh` through the can-core dump endpoint; attachments live on the PVC and are covered by the raw PVC backup |
| Data classes | `support-conversation`, `support-attachment`: 3 years, standard sensitivity |

FreeScout's knowledge base is a paid add-on; the public help centre comes from the wiki module.
SSO is `none`: FreeScout has no OIDC in its free core.

## Open items (not verifiable offline)

- **Non-root image.** Module namespaces enforce the `restricted` pod-security profile. The
  community image `tiredofit/freescout` is the usual way to run FreeScout (there is no official
  image) but starts its supervisor as root, so it may be rejected at admission. The chart already
  asks for `runAsNonRoot` and `NGINX_LISTEN_PORT=8080`; if the image refuses, build a small wrapper
  image (tasks T032) or relax the namespace for this module explicitly.
- **Pin the image.** `image.tag` and `image.digest` are `REPLACE_WITH_...` placeholders;
  `make release-check` fails until they are resolved.
- **Variable names.** `DB_*`, `ADMIN_*`, `SITE_URL` and `NGINX_LISTEN_PORT` follow that image's
  documented variables; the `MAIL_*` names are Laravel's and may need mapping.
- **Probe path.** `/login` as health endpoint (FreeScout has no dedicated one).
