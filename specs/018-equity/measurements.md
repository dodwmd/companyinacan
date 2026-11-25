# Measurements: can-equity idle RAM

Recorded 2026-11-25 on the development machine (Linux, Go 1.27, `CGO_ENABLED=0 go build -trimpath -ldflags '-s -w'`,
binary 12.5 MiB). **Not measured on k3d or in a container**, so the number is an indication, not the CI footprint check.

Method: start `can-equity serve` with a fresh SQLite file, a 0600 keyring and the service-account token on 127.0.0.1;
read `VmRSS` from `/proc/<pid>/status`; then create an entity, a share class, 100 stakeholders with one ledger issuance
each, and issue 300 `GET /v1/entities/{id}/captable?view=fully-diluted&names=true` requests (each audits 101 records and
opens 100 sealed names), paced under the per-user rate limit; read again, wait 20 s, read once more. Fake data only.

| Point | RSS |
|---|---|
| Idle after start (3 s) | 16 188 kB (15.8 MiB) |
| After 100 holders + 300 cap table reads | 28 216 kB (27.6 MiB) |
| 20 s later | 28 216 kB (27.6 MiB) |

Declared in `modules/equity/module.yaml` and the chart: request 16 MB, limit 64 MB. Constitution II fails a component
that exceeds its declared `ram_mb` by more than 20% (19.2 MB) at idle: 15.8 MiB is inside; the post-load figure is
above the idle declaration but well inside the 64 MB limit. The sibling services measure 13.8 MiB (people) and 16 MiB
(finance) idle.
