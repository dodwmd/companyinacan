# analytics

> **Status: experimental.** Installed through can-core on k3d in the advisory `wrapped-modules` CI job
> ([specs/041](../../specs/041-wrapped-modules-live/spec.md)); never on a real server or domain. Backups are not run, and the `analytics`
> capability is marked `planned` (adapter not built, T032).

> **Backups: enabling this module stops them for the whole company.** Its hook needs the `can-hooks`
> image, which is not published, and while such a module is enabled every dump run fails and nothing is
> backed up (`can validate` warns; [docs/backup-restore.md](../../docs/backup-restore.md)).

Cookie-free web analytics for the company site. Wraps [GoatCounter](https://github.com/arp242/goatcounter)
(Go, SQLite) and provides the `analytics` capability.

| | |
|---|---|
| Licence | EUPL-1.2. Network copyleft: see [modules/README.md](../README.md#licences-and-network-copyleft). We run it unmodified. |
| Image | `arp242/goatcounter:2.7.0` (upstream), pinned by digest |
| Footprint | 64 MB request, 160 MB limit, 1 GiB disk. Measured 25 to 35 MiB idle on k3d, about 41 MB locally (SPEC.md estimated 20 MB) |
| Database | SQLite file on the module's PVC (`/data/goatcounter.sqlite3`), so no shared database is needed |
| Public host | `stats.<domain>` (dashboard, and `/count` for the tracker) |
| Capability | `analytics` (`schemas/capabilities/analytics.v1.json`), served by `analytics-adapter` (:8081, off until the adapter image exists) |
| Backup | `hooks/backup.sh` takes an online SQLite `.backup` and PUTs it to can-core; `hooks/restore.sh` puts it back. SQLite files on PVCs are excluded from the raw PVC backup, so this dump is the only copy |
| Data class | `pageview-aggregate`: 3 years, no cookies, no stored IP addresses |

Single replica with `strategy: Recreate`: SQLite allows one writer, so a rolling update must never
run two pods on the volume.

## First sign-in

An init container creates the site `stats.<domain>` and the user `admin@<domain>` (setting
`admin_email`) before the server starts:

```sh
kubectl -n mod-analytics get secret can-module -o jsonpath='{.data.CAN_ANALYTICS_ADMIN_PASSWORD}' | base64 -d
```

Change it in GoatCounter afterwards; the Secret is not used again. GoatCounter takes the password only
on its command line, so it is visible in the init container's process list while the site is created.
It sends no mail (password reset by email is not configured).

## Updates and security patches

The image is pinned by digest and nothing updates it automatically: a patch arrives only with a can-core
release that bumps the digest and the module's `version`.

## Open items

- **Snippet.** The site renderer does not inject the tracking snippet; add
  `<script data-goatcounter="https://stats.<domain>/count" async src="//stats.<domain>/count.js"></script>` by hand.
- `site_domain` and `retention_days` in the settings are accepted but not applied.
- A changed company domain creates a second site at the new host; the old one keeps the history.
- Upgrades to a newer GoatCounter are untested.
