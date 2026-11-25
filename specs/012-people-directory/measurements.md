# Measurements: can-people idle RAM

Recorded 2026-11-25 on the development machine (Linux, Go 1.27, `CGO_ENABLED=0 go build -trimpath -ldflags '-s -w'`,
binary 12.4 MB). **Not measured on k3d or a container**, so the number is an indication, not the CI footprint check.

Method: start `can-people serve` with a fresh SQLite file, a 0600 keyring and the service-account token on
127.0.0.1; read `VmRSS` from `/proc/<pid>/status`; then create 100 people and issue 300 `GET /v1/people?limit=100`
requests (each filters rows through the policy layer); read again, wait 20 s, read once more. Fake data only.

| Point | RSS |
|---|---|
| Idle after start (3 s) | 14 148 kB (13.8 MiB) |
| After 100 creates + 300 list requests | 22 836 kB (22.3 MiB) |
| 20 s later | 22 836 kB (22.3 MiB) |

Declared in `modules/people/module.yaml` and the chart: request 30 MB, limit 64 MB. Constitution II fails a
component that exceeds its declared `ram_mb` by more than 20% (36 MB) at idle: 13.8 MiB is comfortably inside;
the post-load figure is also inside. The target in SPEC.md section 6.1 is ~30 MB.

To reproduce: build the binary, write the three files described in `docs/people.md`, run the loop above.
