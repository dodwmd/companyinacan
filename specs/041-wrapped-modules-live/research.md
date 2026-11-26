# Research and measurements

Dated 2026-11-25. Image facts were read from Docker Hub's registry API and by running the images.

## R1 - Images

| Use | Image (multi-arch index digest) | Licence | Publisher | Runs as |
|---|---|---|---|---|
| analytics | `arp242/goatcounter:2.7.0` `sha256:791df3317bf5ce379511181a4802aaf29127a318b372d4fe39ec59e1c5af44be` | EUPL-1.2 (the image's own label says `NOASSERTION`; the licence is the repository's) | upstream author | uid 1000 |
| newsletter | `listmonk/listmonk:v6.2.0` `sha256:f535d59e14991337a9f2d570273685378ae86b0d7698c3e00da444e3bc205286` | AGPL-3.0 | upstream | any uid (entrypoint bypassed) |
| support | `pikapods/docker-freescout:1.8.252-r2` `sha256:fcb46451971ae10046e4008f036203f64fd0dc6f637cfd45ab0c9ee31ccdebfa` | AGPL-3.0 (FreeScout 1.8.252) | **third party** (PikaPods), on serversideup/php | `www-data`, uid 82 |
| shared database | `postgres:17.12-alpine` `sha256:f9cf3e1f0066f0eb0c48707d864f0ac5941e9ff454a072077d97a4d278d07d73` | PostgreSQL | Docker official image | uid 70 |

FreeScout publishes no image. The alternatives looked at: `nfrastack/freescout` (successor of
`tiredofit/freescout`) and `bfren/freescout` start an init system as root and cannot run under the
`restricted` profile; `bfren`'s latest was from 2022. Building our own image from the release archive
remains the fallback if the PikaPods image stops being maintained. Trusting it is a supply-chain
decision for the maintainer: the digest pins exactly what was tested, and nothing updates it
automatically.

All three are multi-arch indexes with amd64 and arm64 except the PikaPods image, which is a
**single amd64 image**: support does not run on an arm64 server.

## R2 - Behaviour found by running them

- GoatCounter refuses passwords over 50 bytes, and takes a password only on the command line or from a
  terminal. `-tls http` is right behind a proxy.
- GoatCounter answers `/status` for any Host.
- FreeScout answers 403 to a Host other than `APP_URL`'s, redirects plain HTTP when `APP_URL` is
  `https` unless `X-Forwarded-Proto: https`, answers 500 on `/login` while migrating, and `/healthcheck`
  200 from nginx throughout. Hence the probes.
- FreeScout's system mail settings live in its `options` table; `MAIL_*` environment variables are
  not used once the application sets its mail driver. The previous chart's variables had no effect.
- Outgoing and incoming mail **per mailbox** (replies to customers, fetching) are separate settings of
  each mailbox, which the operator enters.
- Listmonk keeps SMTP settings in its database.
- The starter charts referenced a Secret `can-database` with keys `host`, `user`, ... that can-core
  never wrote; it writes `can-db` with `DB_HOST`, `DB_USER`, ...
- `dbprov.Postgres` with a non-superuser `CREATEROLE` admin fails on Postgres 17
  (`must be able to SET ROLE`).

## R3 - Memory

Local runs (rootless podman, x86_64, 32 cores; cgroup memory as `podman stats` reports it), with a
nearly empty database. nginx and Go size some pools by core count.

| Component | Idle |
|---|---|
| GoatCounter 2.7.0 | 41 MB (process RSS 63 MB, of which 25 MB file-backed) |
| FreeScout 1.8.252 | 86 to 115 MB; cgroup peak 140 MB during first-boot migrations |
| Listmonk 6.2.0 | 31 to 35 MB |
| Postgres 17 (`shared_buffers=32MB`) | 36 to 50 MB |

SPEC.md section 6.1 estimated ~20 MB, ~250 MB and ~50 MB. GoatCounter is double its estimate (the
embedded GeoIP database), FreeScout well under it. `ram_mb` is now 64, 192 and 50, plus the shared
database, which the footprint meter already counts once (150 MB).

k3d figures from the CI job are in R4.

## R4 - k3d measurements (CI)

Not recorded yet.
