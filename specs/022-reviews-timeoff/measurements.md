# Measurements: can-people idle RAM with reviews and time off

Recorded 2026-11-25 on the development machine (Linux, Go 1.27, `CGO_ENABLED=0 go build -trimpath -ldflags '-s -w'`,
binary 12.5 MiB). **Not measured on k3d or a container**, so the numbers are an indication, not the CI footprint check.

Method (as `specs/012-people-directory/measurements.md`, plus the new surface): start `can-people serve` with a
fresh SQLite file, a 0600 keyring and the service-account token on 127.0.0.1; read `VmRSS` from
`/proc/<pid>/status`; create 100 people; create a review template, a cycle with all 100 as participants and open it;
create a leave type and run the accrual job (100 ledger entries); then issue 300 rounds of `GET /v1/people?limit=100`,
`GET /v1/reviews?cycle_id=...` and `GET /v1/timeoff/calendar`; read again, wait 20 s, read once more. Fake data only.

| Point | RSS |
|---|---|
| Idle after start (3 s) | 15 264 kB (14.9 MiB) |
| After 100 creates | 20 120 kB (19.6 MiB) |
| After the review and leave setup and 900 requests | 22 816 kB (22.3 MiB) |
| 20 s later | 22 816 kB (22.3 MiB) |

Before E14b: 13.8 MiB idle, 22.3 MiB after load. Idle grew by about 1.1 MiB (more code and embedded SQL); the loaded
figure is unchanged within noise. The manifest keeps 30 MB requested and 64 MB limit: Constitution II fails a
component more than 20% over its declared `ram_mb` at idle (36 MB), and 14.9 MiB is far inside.
