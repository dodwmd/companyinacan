# Measurements: E04 Site renderer and static server

Recorded 2026-11-25 on the author's workstation (Linux 7.3.5, amd64, Go 1.27.3). Numbers are
from single local runs; the in-cluster budget should be re-checked in the E00/E02 harness.

| Check | Result | Target |
|---|---|---|
| `can site build` of the full fixture (all sections, logo, gzip), wall time incl. process start | 23 ms | < 1 s (SC-001) |
| `can-static` binary, `CGO_ENABLED=0 -trimpath -ldflags "-s -w"` | 6.4 MiB (6 750 368 bytes) | n/a |
| `can-static` idle RSS (`GOMEMLIMIT=24MiB`, 5-file site) | 6.7 MiB (VmRSS 6 852 kB) | under 8 MiB |
| `can-static` peak RSS after 4 000 gzip GETs of `/` and 2 000 GETs of a 404, 32 concurrent curl clients | 13.0 MiB (VmHWM 13 356 kB) | under the 16 MiB request |
| `FuzzServePath`, 15 s | 761 k executions, no secret leaked, no unexpected status | no escapes |

Method for the RSS numbers: start the binary, read `/proc/<pid>/status` (`VmRSS`, `VmHWM`), drive
load with `xargs -P 32 curl`, read again.

Resulting budget (documented in `deploy/can-static/README.md`): `ram_mb: 16`, `ram_limit_mb: 32`,
`GOMEMLIMIT=24MiB`, `GOGC=50`.

Not measured here: container image size, behaviour under a real PVC (network block storage
latency), CPU use.
