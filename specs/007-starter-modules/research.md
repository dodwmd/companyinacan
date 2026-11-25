# Research: Starter Modules (Epic E07)

Verification status legend: **known** = I am confident of it from upstream documentation as I
remember it; **estimate** = a planning figure, not measured; **unverified** = depends on something
I could not check offline and must be confirmed by a real run. No digest, tag or measurement below
was fetched or measured here.

## R1. Capability name for the support module: `tickets`, not `helpdesk`

SPEC.md 4.3 defines the capability `tickets` ("Create/read/update/reassign support tickets,
FreeScout to in-house support") and `schemas/names.yaml` already knows it. Introducing `helpdesk`
would split one concept in two and break the swap story. Decision: the support module provides
`tickets` with schema `tickets.v1`. `newsletter` and `analytics` did not exist and were added to
`names.yaml`.

## R2. Analytics: GoatCounter (EUPL-1.2) over Umami (MIT)

| | GoatCounter | Umami |
|---|---|---|
| Idle RAM (SPEC 6.1 catalogue, **estimate**) | ~20 MB | ~100 MB |
| Runtime | single Go binary | Node.js (Next.js) application |
| Storage | SQLite (file on a PVC) or Postgres | Postgres (v2 also MySQL; recent versions Postgres) |
| Extra datastore for a company with no Postgres module | none | a Postgres instance, a further ~tens of MB (**estimate**) and a database to back up |
| Cookies / personal data | none; no IP stored (upstream design statement) | cookie-free, hashed identifiers |
| Licence | EUPL-1.2 (network copyleft) | MIT |
| Image | small static binary image | Node base image, several hundred MB (**estimate**) |

Upstream documentation gives no hard RAM figure for either project that I could cite; the
numbers above are the SPEC catalogue's own estimates, which is why `ram_mb` is labelled an
estimate in the manifest and READMEs. Decision: GoatCounter, about 5x less idle RAM and no
Postgres dependency, so the cheapest server can run analytics without enabling a database.
Cost: EUPL-1.2 instead of MIT, handled in R8, and a less featureful UI. Umami remains the swap
candidate behind the same `analytics` capability.

## R3. FreeScout deployment

- FreeScout is AGPL-3.0 PHP/Laravel on MySQL/MariaDB (**known**). There is no official upstream
  container image; the community image `tiredofit/freescout` is the commonly used one (**known to
  exist; its current tags, variables and user model unverified**).
- It bundles nginx, PHP-FPM, cron and the queue worker (s6 supervisor). It runs its init as root
  and drops privileges for services (**from memory, unverified**), which conflicts with the
  `restricted` pod-security profile of module namespaces. Options when verified: a wrapper image
  that runs everything as uid 1000, or an explicit, reviewed exemption for this namespace.
  The chart assumes non-root and fails closed (admission rejection) rather than weakening policy.
- Variables used: `DB_TYPE DB_HOST DB_PORT DB_NAME DB_USER DB_PASS ADMIN_EMAIL ADMIN_PASS SITE_URL
  APPLICATION_NAME NGINX_LISTEN_PORT` (image documentation, **unverified**); `MAIL_*` are Laravel
  names (**unverified** that the image or mailbox settings honour them; FreeScout stores outgoing
  mail per mailbox).
- The tag is the placeholder `REPLACE_WITH_TAG`, because I cannot enumerate tags offline.
- 250 MB idle and 512 MB limit: SPEC catalogue estimate for the PHP stack.

## R4. Listmonk and GoatCounter specifics

- Listmonk: Go binary with a Postgres backend (**known**). Official image `listmonk/listmonk`
  (**known**); health endpoint `/health` (**known**); config from `LISTMONK_<section>__<key>`
  environment variables (**known**); `--install --idempotent --yes` and `--upgrade --yes`
  (**known**); `LISTMONK_ADMIN_USER` and `LISTMONK_ADMIN_PASSWORD` creating the first super admin
  on install (**from memory, verify**). Tag `v5.0.0` is a release I am confident existed; newer 5.x
  releases are likely, so the tag must be re-resolved when pinning. Entry point behaviour (privilege
  drop with su-exec) is **unverified**; the chart avoids it by setting `command`.
- GoatCounter: Go binary, SQLite or Postgres (**known**); image `arp242/goatcounter`
  (**known**); `serve -listen -tls -db sqlite+<file> -automigrate` (**known**, exact `-tls proxy`
  semantics **unverified**); `/status` health endpoint (**from memory, verify**); first site and
  user created with `goatcounter db create site` (**unverified** semantics and idempotence); tag
  `v2.6.0` **unverified**; non-root and read-only root filesystem **unverified**.

## R5. Footprint: estimates and how they will be checked

| Module | `ram_mb` | `ram_limit_mb` | Basis |
|---|---|---|---|
| support | 250 | 512 | SPEC catalogue ~250 MB; limit about 2x for PHP bursts and imports |
| newsletter | 50 | 128 | SPEC catalogue ~50 MB; limit 2.5x: Go GC plus campaign batches |
| analytics | 20 | 96 | SPEC catalogue ~20 MB; app limit 64 MB (3x idle, headroom for dashboard queries over SQLite) plus 32 MB adapter |

Pod requests (app plus adapter) sum to exactly the declared `ram_mb`; limits sum to the declared
limit (asserted by the contract test). SPEC says CI fails above 1.2x of `ram_mb` once measured:
the measurement task (T040) runs each module on a real k3s node, lets it settle for 10 minutes and
records `kubectl top` into `measurements.md`, as E00 did for the platform. Until then no
figure here is a measurement.

## R6. Hooks need a tools image

Upstream images have no curl or database clients and can-core has no shell, so hooks run in
`ghcr.io/dodwmd/can-hooks` (`deploy/hooks`, Alpine plus curl, gzip, mariadb-client,
postgresql17-client, sqlite). The package names are from memory (**unverified**, never built here)
and the base digest is the documented placeholder.

## R7. Charts: Helm, rendered in tests

The project's chart engine is Helm (SPEC 4.2). `helm` v4.3.2 exists on the development machine and
is on GitHub's ubuntu runners; tests call `helm template` and skip when it is absent. No custom
templater: that would duplicate Helm to test Helm. Constraint kept in mind: charts are embedded
in the can-core image, so no `dependencies:` and no library chart; shared templates are copied and
a test keeps the three copies aligned by asserting the same render invariants against each.

## R8. Licences: AGPL-3.0 and EUPL-1.2

AGPL-3.0 (`AGPL-3.0-only`, `AGPL-3.0-or-later`) is already on `schemas/accepted-licences.yaml`.
FreeScout's licence is AGPL-3.0 and Listmonk's is AGPL-3.0 (**known**); the manifests use
`AGPL-3.0-only`. GoatCounter is EUPL-1.2 (**known**); added to the accepted list (OSI-approved;
EUPL's appendix lists AGPL-3.0 as a compatible licence, **known**). Both are network copyleft;
unmodified third-party containers speaking HTTP are aggregation, so Apache-2.0 repository code
is not affected. Modified forks must offer source to network users. Written into
`modules/README.md` and each module README. This is engineering guidance, not legal advice.

## R9. Dump transport is provisional

SPEC 6.5: backup hooks "HTTP-PUT their dump to a module-API endpoint on can-core, authenticated
with the projected token". E08 has not fixed the path. The hooks use
`PUT|GET $CAN_API_URL/v1/dumps/$CAN_MODULE/<name>` with `Authorization: Bearer <token>` and the
variable names in contracts/hooks.md. E08 owns the final shape and must update the six scripts and
this contract together; the contract test pins the current path so the change is deliberate.

## R10. Adapters are deferred

A capability needs a process that speaks it. FreeScout, Listmonk and GoatCounter do not speak
`tickets.v1` and friends, so each needs an in-house adapter (translates calls, publishes events,
receives the signed inbound webhook, creates the GoatCounter site). Writing three Go services is a
separate epic-sized piece of work with its own footprint; E07 delivers the contract, the chart
slot and the manifests, and says so. Consequence: `provides` is declared but unserved until the
adapters ship; consumers must treat the capability as absent when `adapter.enabled` is false.

Marker (QA follow-up): `provides[].status: planned` and `subscribes[].status: planned` keep the
contract declared while the registry ignores it, instead of removing the declarations; module
`status: not-deployable|experimental` flags support (root-only image) and newsletter/analytics
(unverified; analytics also needs a manual first site, newsletter a manual SMTP step).

## R11. Image pins: placeholders and the existing script

Following `deploy/forms`: unresolved digests are `sha256:REPLACE_WITH_DIGEST` (or
`REPLACE_WITH_TAG` for FreeScout) and `scripts/check-image-pins.sh` fails release builds while any
remain. The script now also scans `modules/*/chart/values.yaml`. The chart helper renders
`repository:tag` while the digest is a placeholder (development) and `repository:tag@digest`
afterwards.

## R12. Routing and network

Traefik is k3s's bundled ingress; `traefik.io/v1alpha1` `IngressRoute` is its native CRD and
supports the certificate resolver (`letsencrypt`) configured in `deploy/base/traefik-config.yaml`
and the priority needed for the support `/inbound` route. `ingress.tls=false` selects the plain
`web` entry point for local dev, where no certificate can be issued. The module namespace policy
(`deploy/templates/module-namespace.yaml`) allows Traefik and can-core in; it excludes the pod
CIDR from egress, so pod-to-pod traffic within a module needs an explicit allow in both directions,
which the chart adds only with the adapter.

## Honest gaps (nothing here was run)

No pod was started, no image pulled, no dump taken or restored, no mail sent, no probe called.
The unverified items above (image users and flags, probe paths, Alpine package names, GoatCounter
site bootstrap) are each a task in tasks.md Phase 7.
