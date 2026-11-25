# Measurements: can-board idle RAM

Recorded 2026-11-25 on the development machine (Linux, Go 1.27, `CGO_ENABLED=0 go build -trimpath -ldflags '-s -w'`,
binary 12.5 MiB). **Not measured on k3d or in a container**, so the number is an indication, not the CI footprint check.

Method: start `can-board serve` with a fresh SQLite file, a 0600 keyring and the service-account token on
127.0.0.1; read `VmRSS` from `/proc/<pid>/status`; then create a legal entity and a body and issue 300 pairs of
`GET /v1/bodies` and `GET /v1/legal-entities` requests (each audited), paced under the per-user rate limit; read
again, wait 20 s, read once more. Fake data only. The service hosts both modules, so this is the footprint of
`board` and `entity` together.

| Point | RSS |
|---|---|
| Idle after start (3 s) | 14 740 kB (14.4 MiB) |
| After 600 audited reads | 21 468 kB (21.0 MiB) |
| 20 s later | 21 468 kB (21.0 MiB) |

Declared in both manifests and the chart: request 20 MB, limit 64 MB. Constitution II fails a component that exceeds
its declared `ram_mb` by more than 20% (24 MB) at idle: 14.4 MiB is inside, and the post-load figure is inside as
well. The sibling services measure 13.8 MiB (people), 15.8 MiB (equity) and 16 MiB (finance) idle. A board with
hundreds of meetings and large resolution texts will use more; sealing and opening happen per request, not in a cache.
