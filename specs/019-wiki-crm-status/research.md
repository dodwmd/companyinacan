# Research: Wiki, CRM and status page (Epic E10, first slice)

Everything here was gathered offline-first. "Fetched 2026-11-25" means I read it from GitHub's
repository API or Docker Hub's tag API with a summarising fetch tool; nothing was pulled, built or
run. Anything marked **unverified** is from memory or inference and must be checked before a release.

## R-wiki. DokuWiki over BookStack, Wiki.js, Docmost, Outline

| Candidate | Stack and data | Licence (source) | Idle RAM (flag) | Verdict |
|---|---|---|---|---|
| **DokuWiki** | PHP, flat files, no database | GPL-2.0 (GitHub API, fetched) | ~50 MB for PHP-Apache with few pages: **my estimate, unverified** | **Picked.** No database to run or back up; fits the 2 GB target with room |
| BookStack | PHP/Laravel + MariaDB | MIT (GitHub API, fetched) | ~200 MB (SPEC.md section 6.1 catalogue) plus the database | Documented alternative: nicer editor, heavier |
| Wiki.js | Node.js + Postgres/MariaDB/SQLite | AGPL-3.0 (GitHub API, fetched) | not measured; Node plus a database | Network copyleft, more RAM; not picked |
| Docmost | Node.js, Postgres + Redis | AGPL-3.0 (GitHub API, fetched); some features under a separate enterprise licence (**from memory, unverified**) | well above 300 MB (**unverified**) | Heavy; not picked |
| Outline | Node.js, Postgres + Redis + S3-compatible storage | GitHub cannot classify it (NOASSERTION); believed BSL 1.1, i.e. source-available (**unverified**, matches SPEC.md which already excludes it) | heavy | Excluded (Principle VI) |

DokuWiki's own images: the Docker Hub repository `dokuwiki/dokuwiki` exists and was updated
2026-11-23 (fetched). Its README (fetched from github.com/dokuwiki/docker) states port 8080, the
`/storage` volume, `--user 1000:1000` support (the storage volume must be writable by that uid), and
the variables `PHP_UPLOADLIMIT`, `PHP_MEMORYLIMIT`, `PHP_TIMEZONE`. Tags are `stable`, `oldstable`,
`master` and dated versions; I use `stable` and mark pinning as a release task.

Costs of the pick: dated UI, no OIDC in core, permissions through ACL pages. SPEC.md section 5 named
BookStack or Wiki.js; this deviates and the row is updated.

## R-crm. EspoCRM

| Candidate | Stack | Licence (source) | Footprint | Verdict |
|---|---|---|---|---|
| **EspoCRM** | PHP + MariaDB/MySQL, cron daemon | AGPL-3.0 (GitHub API, fetched) | ~350 MB per SPEC.md section 6.1 (**not measured**) | **Picked**: business CRM (contacts, accounts, leads, deals, workflows), uses the shared MariaDB, SPEC's existing choice |
| Twenty | Node.js, Postgres + Redis | GitHub: NOASSERTION; believed AGPL-3.0 with a commercial `ee` directory (**unverified**) | over 1 GB (**unverified**) | Heavy; licence mix needs reading |
| SuiteCRM | PHP + MariaDB | AGPL-3.0 (GitHub API, fetched, SuiteCRM-Core) | heavy (SPEC.md) | Heavier than EspoCRM for the same job |
| Monica | PHP/Laravel | AGPL-3.0 (GitHub API, fetched); described upstream as a personal CRM | moderate | Personal relationships, not a sales pipeline |
| Odoo CRM | Python + Postgres | Community edition LGPL-3.0, Enterprise edition proprietary (**from memory, unverified**) | far above the 2 GB target as a platform | Not picked; open-core split and size |

The Docker Hub repository `espocrm/espocrm` is marked LEGACY and points at the Docker Official Image
`espocrm` (fetched). The module uses `docker.io/library/espocrm`. Its README (fetched from
docker-library/docs) gives: `ESPOCRM_DATABASE_{PLATFORM,HOST,PORT,NAME,USER,PASSWORD}`,
`ESPOCRM_ADMIN_{USERNAME,PASSWORD}`, `ESPOCRM_SITE_URL`; daemon and websocket as the same image with
entrypoints `docker-daemon.sh` and `docker-websocket.sh`; Apache on container port 80; volumes
`/var/www/html/{data,custom,client/custom}`; tags like `10.1.4-apache`. It **does not say which user
runs the processes** and lists no memory requirement: hence `status: not-deployable` until a non-root
wrapper is shown to work, and the footprint stays SPEC's estimate.

Lead integration: the CRM consumes `leads` (read of captured submissions) and, through its planned
adapter, subscribes to `lead.created.v1`. Design in
[contracts/lead-to-crm-adapter.md](contracts/lead-to-crm-adapter.md).

## R-status. Uptime Kuma over Gatus, Cachet, Statping

| Candidate | Stack | Licence (source) | Notes | Verdict |
|---|---|---|---|---|
| **Uptime Kuma** | Node.js, SQLite | MIT (GitHub API, fetched) | admin UI, public status pages, many alert channels; rootless image variant exists | **Picked** (SPEC.md's choice) |
| Gatus | Go, config file, optional SQLite | Apache-2.0 (GitHub API, fetched); active (pushed 2026-11-23) | tens of MB (**my estimate, unverified**); no admin UI; config must be a file, and the installer allows no ConfigMap | Documented alternative; fits better once config-from-settings exists |
| Cachet | PHP/Laravel | GitHub: NOASSERTION (believed BSD-3-Clause, **unverified**) | incident-oriented, no active probing (from memory) | Not picked |
| Statping(-ng) | Go | not checked | maintenance status unverified | Not picked |

Docker Hub `louislam/uptime-kuma` lists tags `2.5.12`, `2.5.12-rootless`, `2-rootless`, `*-slim` and
beta/nightly builds (fetched 2026-11-25). Upstream's dockerfile (fetched) shows the rootless stage
sets `USER node` and the image exposes port 3001, with data in `/app/data`. The module pins
`2.5.12-rootless` (a tag that existed) with a digest placeholder; numeric uid and whether it runs under
`restricted` are **unverified**.

Admin versus public: the installer allows `Host`, `Path`, `PathPrefix`, `Method`, `Headers` in one
`&&` chain and no Middleware objects, so IP allow-listing the admin UI is not expressible. The module
therefore routes only the public status paths by default (R-routing).

## R-routing. Public status paths, admin kept off the internet

Rules: one `Host()` per rule, `&&` chains only (core/server/installer/match.go). The chart emits one
rule per public path: `Path(/)` and `PathPrefix` for `/status`, `/api/status-page`, `/assets`,
`/upload`, `/icon.svg`, `/favicon.ico`. The dashboard, socket.io and `/setup` are simply absent, so
Traefik answers 404 for them. The path list is from my memory of upstream's URL scheme and is
**unverified**; it is a value (`ingress.publicPaths`), so it is fixable without a template change.
`ingress.publicOnly=false` routes the whole host (local dev). Exposure of the first-user setup race is
the reason for the default.

## R-licences. Licence claims and what to verify

| Module | Recorded SPDX | Evidence | Open question |
|---|---|---|---|
| wiki (DokuWiki) | GPL-2.0-only | GitHub API spdx `GPL-2.0` (fetched) | "only" vs "or later" is not stated by that API; check the repository's COPYING and file headers before release; if "or later", change to `GPL-2.0-or-later` (also accepted) |
| crm (EspoCRM) | AGPL-3.0-only | GitHub API spdx `AGPL-3.0` (fetched) | "only" vs "or later"; I believe upstream adds attribution terms (section 7 notices, logo/footer): **unverified**, read LICENSE before white-labelling |
| status (Uptime Kuma) | MIT | GitHub API spdx `MIT` (fetched) | none known; the Docker image bundles other software under its own licences |

All three are on `schemas/accepted-licences.yaml` already; no change to that file. GPL-2.0 has no
network clause; AGPL-3.0 does (modules/README.md applies). GitHub's detector names the licence of the
repository default branch only, not of the Docker image's other contents or plugins.

## R-footprint. Estimates and how they will be checked

| Module | Manifest request / limit | Basis | Status |
|---|---|---|---|
| wiki | 64 / 160 MB | my estimate for PHP-Apache + small flat-file wiki (app 56, adapter 8) | unverified |
| crm | 350 / 640 MB | SPEC.md section 6.1 (~350), split web 208 / daemon 96 / adapter 40 | unverified; EspoCRM's README lists no requirement |
| status | 128 / 320 MB | SPEC.md section 6.1 (~100) plus adapter 16 | unverified; Node plus a growing monitor list |

Sum for a 2 GB box that runs all three plus core: 542 MB of requests. Together with the existing
starters (support 250, newsletter 50, analytics 20) that is 862 MB; the real check is task T040 (10
minutes settled, `kubectl top`), which needs a cluster. The contract tests assert only that chart
request sums are at most the manifest figures, not that either is true. Constitution II caveat: these
are claims, not measurements.

## R-images. Images, tags and digests

No digest is known to me, so every `image.digest` is `sha256:REPLACE_WITH_DIGEST`
(`scripts/check-image-pins.sh` and `make release-check` fail while it remains). Tags: wiki `stable`
(floating, upstream's documented alias); crm `REPLACE_WITH_TAG`; status `2.5.12-rootless` (seen on
Docker Hub, not pulled). Adapter images `ghcr.io/dodwmd/can-adapter-{wiki,crm,status}:0.1.0` do not
exist; the adapters are off in the chart.

## R-sso. SSO

None of the three is wired to SSO (`requires.sso: none`). OIDC exists in EspoCRM's own settings and as
a community plugin for DokuWiki; Uptime Kuma has local accounts with 2FA. E11's SSO work will revisit.

## R-names. Capability and permission names

Capabilities `wiki`, `crm` and `status` are new in `schemas/names.yaml`. SPEC.md section 4.3 also lists
`contacts` and `content`; the CRM here provides only `crm` because `contacts` ties into the `directory`
party model (E13) and `content` into the CMS: a later adapter can provide them in addition. Permissions
`wiki.*`, `crm.*`, `status.*` avoid the reserved prefixes (platform., rbac., approvals., notifications.)
and every existing module's names (tested). Role `operator` is built into can-core, so status uses `ops`.

## R-tasks. "Simple tasks"

Epic E10 also lists simple tasks (Vikunja). It is not in this slice: see spec.md "Out of scope".
