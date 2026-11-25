# Measurements: can-core MVP (E03 part 1)

Recorded 2026-11-25 on the author's workstation (Linux 7.3.5, amd64, Go 1.27.3). Not a k3d measurement: that
belongs to the E00 footprint harness and the part-2 chart.

| Check | Result | Target |
|---|---|---|
| Binary size (`go build -trimpath -ldflags '-s -w'`, CGO off) | 13.3 MB | informational |
| Idle RSS 3 s after `can-core serve` (fresh database, both listeners up) | 38 MB (VmRSS 38116 kB) | <= 48 MB (declared `ram_mb`) |
| RSS after 10 rounds of failed Argon2 logins plus module-API probes | 55 MB (VmRSS 55156 kB, VmHWM 56576 kB) | <= 96 MB (declared `ram_limit_mb`) |
| Soft Go memory limit | 64 MiB default (`CAN_MEMLIMIT_MB`) | keeps the heap from growing past the limit under GC pressure |

How it was measured: `bin/can-core admin bootstrap`, then `can-core serve` with `CAN_TOKEN_REVIEW=fake`, read
`/proc/<pid>/status` (`VmRSS`, `VmHWM`), exercised the login and module endpoints, `SIGTERM` (clean shutdown logged),
then `can-core audit verify` on the resulting database (OK, 11 entries).

Notes:
- The step from 38 MB to 55 MB is Go retaining the 19 MiB Argon2 working set after logins. The idle figure the
  footprint check should use is the post-login steady state (about 55 MB) until a `debug.FreeOSMemory` tweak is
  evaluated; this is inside the declared limit but above the declared request, so the request may need to move to
  about 56 MB when the chart lands (the CI check in Constitution II fails above request + 20%, that is 57.6 MB for
  a 48 MB request, so the 55 MB figure passes today only narrowly). Re-measure on k3d in part 2 before fixing the
  numbers in the chart.
- SPEC 3.2 budgets about 100 MB for can-core + site + forms together; can-core alone using 55 MB leaves 45 MB for
  the other two, which is tight but plausible for static-file and form-post services.

## Test and CI results

`make test` (lint, unit, contract, licence check) is green locally. CI on PR #9 (commit 9da6930): `lint`, `unit`, `contract`, `licences`, `dco` all green.

## Part 2 (E03b)

Recorded 2026-11-25 on the author's workstation (not k3d), `can-core serve` with `CAN_TOKEN_REVIEW=fake`, installer off,
fresh database, 3 s after start.

| Check | Result | Part 1 |
|---|---|---|
| Binary size (`-trimpath -ldflags '-s -w'`, CGO off) | 16.96 MB | 13.3 MB |
| Idle RSS | 41.5 MB (VmRSS 41552 kB) | 38 MB |

The +3.5 MB comes from the Kubernetes client, installer, chart renderer and the `lib/pq` Postgres driver. For
comparison, importing the Helm engine alone adds about 15 MB of resident memory (research R16). The post-login figure
(about 55 MB before) was NOT re-measured; if it grows by the same 3.5 MB it is about 58 MB, just over the CI check bound (57.6 MB for a
48 MB request); the request should move to about 56 MB, or `debug.FreeOSMemory` after Argon2 should be evaluated.
The dev-cluster workflow prints the in-cluster RSS of the can-core process after the lifecycle test; the real in-cluster
figures, and what the workflow run showed, are recorded in the PR description.
