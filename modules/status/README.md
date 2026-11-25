# status

> **Status: experimental.** Never run on a cluster. The `status` capability is `planned` (adapter not
> built, T032). The first admin account is created by hand through a port-forward (see "First run").

Public status page and uptime monitors with alerting. Wraps [Uptime Kuma](https://github.com/louislam/uptime-kuma)
(Node.js, SQLite) and provides the `status` capability.

| | |
|---|---|
| Licence | MIT (GitHub reports MIT). Not copyleft: no network-copyleft duties beyond keeping the notice. See [modules/README.md](../README.md#licences-and-network-copyleft) for the other modules. |
| Footprint (estimate) | 128 MB request (app 112 MB, adapter 16 MB), 320 MB limit, 1 GiB disk. SPEC.md section 6.1 says ~100 MB; Node.js with a growing monitor list can exceed it. **Unverified, not measured.** |
| Database | SQLite file on the module's PVC (`/app/data/kuma.db`) |
| Public host | `status.<domain>` |
| Routing | **Public paths only by default** (`ingress.publicOnly: true`): the status pages and their assets are routed; the admin dashboard, its socket.io channel and the first-run setup are not. Reach the admin UI with `kubectl -n mod-status port-forward svc/status 3001:3001`, or set `ingress.publicOnly=false` (local dev, first run) to route the whole host, which exposes the login page. |
| SSO | `none`. Uptime Kuma has local accounts and optional 2FA; no OIDC claimed. |
| Capability | `status` (`schemas/capabilities/status.v1.json`), served by `status-adapter` (:8081, off until the adapter image exists) |
| Backup | `hooks/backup.sh` takes an online SQLite `.backup` and PUTs it to can-core; `hooks/restore.sh` puts it back. SQLite files on PVCs are excluded from the raw PVC backup, so this dump is the only copy |
| Data classes | `monitor-heartbeat` 180 days, `incident-history` 3 years, standard sensitivity, no personal data |

Single replica with `strategy: Recreate`: SQLite allows one writer.

## Monitoring from outside

A status page on the same server cannot report that the server is down. The README of a real
deployment should add one external monitor (a hosted ping) that watches `status.<domain>` itself.

## Alternative

**Gatus** (Apache-2.0, Go, a few tens of MB, config as a YAML file) is lighter and fully
config-as-code, but it has no admin UI and its config would have to arrive as a file, which the
installer's object allowlist (no ConfigMaps) does not currently allow. Cachet and Statping are
unmaintained or heavier; see research.md R-status. A sibling module providing `status` would use it.

## First run (manual) and open items (unverified offline)

- **First user.** Uptime Kuma lets whoever opens it first create the admin account. That is why the
  setup page is not routed publicly: do it through the port-forward before anything else.
- **Image.** `2.5.12-rootless` was listed on Docker Hub when this was written; not pulled or run. The
  rootless build runs as the image's `node` user; the numeric uid 1000 in the chart is an assumption.
  `image.digest` is a `REPLACE_WITH_DIGEST` placeholder.
- **Public paths** (`/status`, `/api/status-page`, `/assets`, `/upload`, `/icon.svg`, `/favicon.ico`)
  and the data file name `kuma.db` come from upstream's layout as remembered. If a status page renders
  without styling or data, a path is missing from `ingress.publicPaths`.
- **Mapping the root.** Read from upstream's source (`server/server.js`, `server/routers/api-router.js`,
  master branch, not run here): for `/`, a request whose host is a **domain-mapped** status page is
  served that page directly; otherwise `/` redirects to the status page chosen as the entry page, or
  to `/dashboard`. The page then calls the unauthenticated `GET /api/entry-page`, which returns only
  `{type, statusPageSlug}` or `{type, entryPage}`; it needs no login, so it is in `ingress.publicPaths`
  (without it the root page cannot resolve its slug). Consequence under `publicOnly`: `/` works only
  for a domain-mapped or entry-page status page; if neither is set, `/` redirects to `/dashboard`,
  which is not routed, so the root is unusable and visitors must use `/status/<slug>`. Set the
  entry page or a domain mapping in the Kuma UI (port-forward) to make `/` useful.
- **Alerts** use the `can-smtp` relay only if configured by hand in the Kuma UI; nothing is injected.
