# Footprint: method, before and after, budgets

Measured 2026-11-25 on one development machine: linux/amd64, 32 hardware threads, rootless Podman 6.1 with
podman-compose 1.6.0, Go 1.27.3, cgroup v2. **Not a 2 GB server and not a cluster.** All figures are MiB.
"Before" is `main` at `b4a2caa`; "after" is the same plus the changes below.

## Method

Compose stack of `make e2e-up`, run twice (before, after), each time: build, wait 30 s, sample ("idle"), run the
end-to-end suite once (the load: about 2 minutes, every module, admin logins, a restart of the stack), sample
("loaded"), wait 60 s, sample again. For every container:

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
| cgroup, idle | 109.3 | **76.8** |
| cgroup, after the suite | 143.9 | **92.2** |
| cgroup, 60 s later | 130.7 | **80.4** |
| `docker stats` sum after the suite, in its decimal MB | 149.5 | 95.9 |
| sum of `VmRSS`, idle / after the suite | 244.8 / 267.5 | **209.0 / 216.0** |

The first CI run of the check (GitHub `ubuntu-latest`, Docker Engine, after only) agrees or is lower: cgroup 70.0
idle and 70.5 after the suite, can-core `RssAnon` 11.9 and 11.8, peak 49.3.

## Per service (`RssAnon` idle / after the suite; `VmHWM` is the peak)

| Service | before | after | peak before | peak after |
|---|---|---|---|---|
| can-core | 41.8 / 55.5 | **13.1 / 13.2** (16.4 a minute later) | 72.7 | **48.6** |
| people | 8.1 / 8.3 | **4.9 / 4.5** | 22.0 | 18.0 |
| work | 7.1 / 9.2 | **4.7 / 8.3** | 22.3 | 21.3 |
| jobs | 7.9 / 7.3 | **5.9 / 5.3** | 20.5 | 18.5 |
| finance | 5.5 / 7.6 | 5.7 / 7.4 | 22.0 | 21.8 |
| equity | 4.0 / 3.8 | 4.0 / 3.9 | 14.9 | 14.9 |
| files | 3.9 / 3.3 | 4.0 / 3.3 | 13.1 | 13.1 |
| forms | 4.3 / 5.9 | 4.3 / 4.5 | 16.6 | 15.2 |
| site | 1.8 / 2.0 | 1.9 / 2.0 | 7.7 | 7.7 |
| Caddy (not ours) | 12.3 / 12.9 | 12.7 / 12.4 | 41.9 | 41.0 |

Run-to-run variation of a service is about 1 to 2 MiB; differences of that size are noise.

## Where the memory was

**argon2id at start-up and after a login (can-core).** `auth.NewService` computed a real argon2id hash (19 MiB
of working memory) to have something to verify unknown user names against, so an idle can-core held it as
garbage for a long time. Local profile before: resident heap 27.9, `RssAnon` 30.8 idle, 51.0 after one login.
The dummy is now built from random salt and key with the same parameters (verifying it costs exactly what a real
verification costs), and the memory is returned after every hash. After: resident heap 8.8, `RssAnon` 12.1 idle,
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
4.4, jobs 7.8 to 5.7. A service whose pages have all been visited ends up about where it was.

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
| `GOGC=50` on every service, whole stack | stack anon 69.5 idle and 70.8 loaded against 67.0 and 66.3 with the default: no gain (the heaps are already at the minimum). For can-core alone under 2,000 local requests it saved about 3 MiB |
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
  about 52 MiB); the k3s layer is inside its 600 target only on the large runner; "each module within 1.2 times
  `ram_mb`" holds for the in-house services' `RssAnon` and `VmRSS` here, not for any wrapped app.

Use 4 GB. The step that settles it: install the bundle on a real 2 GB server and read `free` after an hour.

## Regression check

[`footprint-budgets`](../../.github/workflows/footprint-budgets.yml) (advisory, pull requests touching code or
the stack, and manual) runs `dev/footprint/composecheck` against
[`dev/footprint/budgets.json`](../../dev/footprint/budgets.json): binary sizes; each service's `RssAnon` idle and
after the end-to-end suite, with a total for both; and each service's peak RSS. A service without a budget, or a
budgeted one that is not running, fails too. Each excess is printed with the service and both numbers, and as an
annotation. The budgets have headroom over the figures above: they catch a regression the size of the ones fixed
here (checked locally: the previous `main` fails with can-core at 41.8 against 20), not a 1 MiB drift.

Locally, with the stack up:

```sh
make e2e-up
go run ./dev/footprint/composecheck -project can-e2e -phase idle      # add -engine podman if needed
make e2e
go run ./dev/footprint/composecheck -project can-e2e -phase loaded
make e2e-down
```

## Not measured

Cluster pods (Kubernetes limits, page cache of a persistent volume), arm64, concurrent users, large databases
(except the dump above), file uploads and statement imports at their size limits, `board`, `entity` and
`provisioning` at run time (not in the Compose stack).
