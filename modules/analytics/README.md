# analytics

> **Status: experimental.** Never run on a cluster; the `analytics` capability is marked `planned`
> (adapter not built, T032). The module is **not self-starting**: the first GoatCounter site and
> user are created by hand, see "First site" below.

Cookie-free web analytics for the company site. Wraps [GoatCounter](https://github.com/arp242/goatcounter)
(Go, SQLite) and provides the `analytics` capability.

| | |
|---|---|
| Licence | EUPL-1.2. Network copyleft: see [modules/README.md](../README.md#licences-and-network-copyleft). We run it unmodified. |
| Footprint (estimate) | 20 MB idle request, 96 MB limit (app 64 MB, adapter 32 MB), 1 GiB disk. SPEC.md section 6.1 puts GoatCounter at ~20 MB against Umami's ~100 MB; upstream describes it as a lightweight single binary that scales on SQLite. **Not measured**; see research.md R2 for the comparison. |
| Database | SQLite file on the module's PVC (`/data/goatcounter.sqlite3`), so no shared database is needed |
| Public host | `stats.<domain>` (dashboard, and `/count` for the tracker) |
| Capability | `analytics` (`schemas/capabilities/analytics.v1.json`), served by `analytics-adapter` (:8081, off until the adapter image exists) |
| Backup | `hooks/backup.sh` takes an online SQLite `.backup` and PUTs it to can-core; `hooks/restore.sh` puts it back. SQLite files on PVCs are excluded from the raw PVC backup, so this dump is the only copy |
| Data class | `pageview-aggregate`: 3 years, no cookies, no stored IP addresses |

Single replica with `strategy: Recreate`: SQLite allows one writer, so a rolling update must never
run two pods on the volume.

## Open items (not verifiable offline)

- **First site (manual).** GoatCounter needs a site and an admin user created once (`goatcounter db create
  site ...`). The command line and its idempotence were not verified; until the adapter's
  `register_site` verb exists the operator does this once with `kubectl exec`.
- **Image, flags and probe.** Image `arp242/goatcounter`, the `serve -listen -tls proxy -db
  sqlite+... -automigrate` flags, the `/status` probe and non-root operation come from upstream
  documentation as remembered. `image.digest` is a `REPLACE_WITH_DIGEST` placeholder.
- **Snippet.** The site renderer does not yet inject the tracking snippet; that is a site/analytics
  integration for the E04 renderer once `analytics` is enabled.
