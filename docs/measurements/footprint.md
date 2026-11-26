# Footprint: method, before and after, budgets

Measured 2026-11-26 on one development machine: linux/amd64, 32 hardware threads, rootless Podman 6.1 with
podman-compose 1.6.0, Go 1.27.3, cgroup v2. **Not a 2 GB server and not a cluster.** All figures are MiB.
"Before" is `main` at `171a7b1`; "after" is the same plus the changes below. Two rounds of each; "a / b" below
is idle / loaded unless said otherwise.

**Corrected.** The first version of these figures ran the end-to-end suite with its restart test, which does
`compose down` and `up`: the "after the suite" samples (and the first loaded budgets) mostly described containers
that had just been recreated, and understated the loaded figures by up to a third. Everything in the stack tables
was measured again with `E2E_RESTART=0`, the same processes from the idle sample to the last one.

## Method

Compose stack of `make e2e-up`, run twice (before, after), each time: build, wait 30 s, sample ("idle"), run the
end-to-end suite once with `E2E_RESTART=0` (the load: about 2 minutes, every module, admin logins; **no
restart**), sample ("loaded"), wait 60 s, sample again. The checker records each container's id, process, start
time and restart count when idle and refuses to report a loaded figure for any other run. For every container:

- `cgroup`: `memory.current`; `anon` and `file` from `memory.stat`;
- `RssAnon`, `VmRSS`, `VmHWM` from `/proc/<pid>/status` of the main process.

Which figure means what:

| Figure | Includes | Use |
|---|---|---|
| `RssAnon` | Go heap, goroutine stacks, runtime, SQLite (modernc allocates its own) | what a change in the code moves; budgeted |
| `VmRSS` | `RssAnon` plus the pages of the binary that are resident (11 to 28 MiB per service) | what a fresh server really needs |
| cgroup `memory.current` | anon, kernel memory, and page cache first touched by this container; **also a healthcheck process while one runs** (2 to 3 MiB, at random) | what `docker stats` and a pod limit see |

On this machine the binaries' pages were already cached by the image build, so they are charged to no container:
the cgroup and `docker stats` totals below are **lower than on a server that has just booted**. Sum of `VmRSS`
is the upper figure.

Heap detail comes from pprof: build with `-tags canpprof` and set `CAN_PPROF_ADDR=127.0.0.1:<port>`
(`core/obs/pprof_diag.go`; no release or compose image has the tag, and a non-loopback address is refused). The
local profiles ran one binary at a time with the compose dev config.

## Whole stack (12 containers: 9 services, Caddy, two development stubs)

| | before | after |
|---|---|---|
| cgroup, idle | 110 to 111 | **73** |
| cgroup, loaded | 163 to 164 | **120 to 121** |
| cgroup, 60 s later | 153 to 154 | **111 to 113** |
| `docker stats` sum loaded, in its decimal MB | 166 | 121 to 122 |
| sum of `VmRSS`, idle | 246 to 247 | **207 to 209** |
| sum of `VmRSS`, loaded | 288 to 289 | **240 to 244** |

**The "under 100 MiB" figure is met idle and not met under load.**

## Per service (`RssAnon` idle / loaded; `VmHWM` is the peak of the same process)

| Service | before | after | peak before | peak after |
|---|---|---|---|---|
| can-core | 42 to 43 / 56 to 57 | **14 / 15 to 16** (19 to 20 a minute later) | 77 to 78 | **51 to 53** |
| people | 8.1 to 8.3 / 11.0 to 11.1 | **4.9 to 5.0 / 8.5 to 8.7** | 25 | 22 |
| work | 7.0 to 7.2 / 9.1 to 9.5 | **4.7 / 8.6 to 8.7** | 22 to 23 | 22 |
| jobs | 8.1 / 10.0 to 10.6 | **5.8 to 5.9 / 9.6 to 9.8** | 24 | 23 |
| finance | 5.7 to 5.9 / 9.2 to 9.5 | 5.6 to 5.8 / 9.4 to 9.5 | 24 | 24 |
| equity | 4.0 to 4.2 / 6.7 to 6.8 | 4.0 to 4.2 / 6.6 to 6.7 | 18 | 18 |
| files | 3.8 to 3.9 / 6.4 to 6.7 | 3.9 / 5.4 to 6.6 | 17 to 18 | 16 to 18 |
| forms | 4.2 to 4.4 / 6.4 | 4.3 to 4.4 / 6.2 to 6.4 | 17 | 17 |
| site | 1.9 / 2.0 | 1.8 to 2.0 / 2.0 to 2.2 | 7.7 | 7.7 to 7.9 |
| Caddy (not ours) | 11.5 to 11.9 / 15 | 11.5 to 12.3 / 14.5 to 15 | 44 | 43 |

Run-to-run variation of a service is about 1 MiB; differences of that size are noise. So the gains are in
can-core (idle and loaded) and in `people`, `work` and `jobs` when idle and, less, under load.

## Where the memory was

**argon2id at start-up and after a login (can-core).** `auth.NewService` computed a real argon2id hash (19 MiB
of working memory) to have something to verify unknown user names against, so an idle can-core held it as
garbage for a long time. Local profile before: resident heap 27.9, `RssAnon` 30.8 idle, 51.0 after one login.
The dummy is now built from random salt and key with the same parameters (verifying it costs exactly what a real
verification costs), and the memory is returned after every hash (about 4 to 6 ms on a 13 ms hash on this machine). After: resident heap 8.8, `RssAnon` 12.1 idle,
12.0 after one login, peak 46. **argon2 parameters are unchanged** (m=19 MiB, t=2, p=1; a test now fails if
they are lowered). A login still needs the 19 MiB while it runs.

**Backup dump (can-core).** Reproduced with a 40 MiB state database in the can-core image:

| cgroup MiB | before the dump | after the dump | of which page cache |
|---|---|---|---|
| before | 32.1 | **78.1** | 40.5 |
| after | 12.7 | **14.1** | 0.0 |

The reading of about 81 MiB against the 96 MiB limit was mostly the page cache of the dump copy, which is
reclaimable (it does not by itself cause an out-of-memory kill) but is counted in the working set. Each dump file
is now dropped from the cache after its `fsync` (`FADV_DONTNEED`), and the run ends with a garbage collection.
The cache grows with the database, so the "before" figure had no ceiling. Module backup hooks need a cluster and
were **not** measured; their uploads are written through the same files, which `syncAll` flushes and drops.

**Template parsing.** `people`, `jobs`, `work` and the admin UI parsed every page at start (`finance` already
did not). Heap profile of idle `people`: about 3 MiB of parsed templates. Pages are now parsed on first use
(`core/lazytmpl`), and each UI has a test that parses them all. Local `RssAnon`: people 8.1 to 4.8, work 7.2 to
4.4, jobs 7.8 to 5.7. The more of a service's pages are visited, the smaller this gain.

**The rest of an idle module** (4 to 6 MiB `RssAnon`): resident Go heap 2 to 4 MiB of which live objects 1.3 to
2.5, goroutine stacks 0.8 to 1.2, GC metadata about 2. SQLite (pure-Go modernc, memory outside the Go heap) has
one writer connection per service; `work`, `finance` and `jobs` add up to three readers that close after a minute
idle. `GOMAXPROCS` matters a little: with 2 instead of 32, modules used 0.5 to 2 MiB less, so a small server
should do slightly better than these figures.

**Binaries** (release flags `-trimpath -ldflags "-s -w"`, which the images already used): can-core 19.2,
finance 16.0, people 15.5, jobs 14.9, work 14.5, equity 12.6, board 12.6, files 12.0, provision 12.0, forms
11.8, can-static 6.7. Unchanged by this work. The largest single part of can-core is the SQLite library (about
1.9 MiB of code), then the Go function tables, runtime, TLS and HTTP.

## Tried and not adopted

| Idea | Result |
|---|---|
| `GOGC=50` on every service, whole stack | stack cgroup anon 71 idle and 90 loaded against 64 and 90 to 91 with the default: no gain (the heaps are already at the minimum). For can-core alone under 2,000 local requests it saved about 3 MiB |
| SQLite `cache_size` for can-core cut from 2 MB to 256 KB, 40 MiB database, 2,000 requests | under 1 MiB difference: not worth slower queries |
| `mmap_size` | already off (SQLite default); turning it on would add file pages |
| `-gcflags=all=-l` (no inlining) or compressing binaries | not tried: slower code, and a compressed binary turns shareable file pages into private memory |
| `GOMEMLIMIT` for the modules | not set: their heaps are far below the 64 MiB pod limits, so it would change no figure here. It remains a possible safety net for large imports and exports; can-core already has 64 MiB |

## 2 GB server: are the SPEC targets met by the full default bundle?

**Not established, and not claimed.** The bundle SPEC 3.2 and 10 name for 2 GB is the platform plus site, forms,
support, newsletter and analytics, idling under 1.5 GB of total used RAM (OS, k3s and databases included).

- On `main`, `support` is marked `not-deployable`, so that bundle cannot even be installed.
- The bundle has never run together, and nothing here has run on a 2 GB machine.
- What exists are separate measurements: k3s with Traefik, CoreDNS and local-path 510 to 563 MiB on a 16 GB
  CI runner ([phase0-ci.md](phase0-ci.md)); and, from the not yet merged pull request #56 (k3d, empty data),
  analytics 26 to 33, support 63 to 66, newsletter 26 to 27 and a shared Postgres 35 to 36 MiB.
- Adding those to can-core, site and forms gives roughly 0.75 GB before the host operating system. That leaves
  room on paper; it is arithmetic across different machines and not a measurement, and it says nothing about real
  data, peaks (image pulls, Helm, backups) or page cache.
- Of the per-layer targets: "can-core + site + forms at most about 100 MB" is met in Compose (sum of `VmRSS`
  about 52 MiB idle, 57 loaded); the k3s layer is inside its 600 target only on the large runner; "each module within 1.2 times
  `ram_mb`" holds for the in-house services' `RssAnon` and `VmRSS` here, not for any wrapped app.

Use 4 GB. The step that settles it: install the bundle on a real 2 GB server and read `free` after an hour.

## Regression check

[`footprint-budgets`](../../.github/workflows/footprint-budgets.yml) (advisory, pull requests touching code or
the stack, and manual) runs `dev/footprint/composecheck` against
[`dev/footprint/budgets.json`](../../dev/footprint/budgets.json): binary sizes; each service's `RssAnon` idle and
after the end-to-end suite (run with `E2E_RESTART=0`), with a total for both; and each service's peak RSS.
The loaded phase fails, without judging any budget, if a container was restarted or recreated since the idle
phase. A service without a budget, or a
budgeted one that is not running, fails too. Each excess is printed with the service and both numbers, and as an
annotation. The budgets have headroom over the figures above: they catch a regression the size of the ones fixed
here (checked locally: the previous `main` fails with can-core at 42 against 20 idle and 56 against 30 loaded),
not a 1 MiB drift.

Locally, with the stack up:

```sh
make e2e-up
go run ./dev/footprint/composecheck -project can-e2e -phase idle -state /tmp/containers.json    # add -engine podman if needed
E2E_RESTART=0 make e2e
go run ./dev/footprint/composecheck -project can-e2e -phase loaded -state /tmp/containers.json
make e2e-down
```

## Not measured

Cluster pods (Kubernetes limits, page cache of a persistent volume), arm64, concurrent users, large databases
(except the dump above), file uploads and statement imports at their size limits, `board`, `entity` and
`provisioning` at run time (not in the Compose stack).
