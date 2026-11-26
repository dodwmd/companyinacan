# The whole journey, measured (workflow `acceptance`, advisory)

What the [acceptance journey](../../specs/059-acceptance-journey/spec.md) measured on 2026-11-26: from the
built `can` binary, the built images and an empty directory to a restored company. Every run writes the
same report as the artifact `acceptance-report`, with the containers' memory and the logs.

**Read these first.** The server is a GitHub `ubuntu-24.04` runner VM (4 CPUs, 16 GB), reached over ssh by
the `ssh` provider; it is also the operator's machine, so uploads cross a loopback. The certificate, DNS,
mail relay and backup bucket are stand-ins (listed in each report). Nothing here is a measurement of a real
or a 2 GB server, and VM creation and the image build are not included.

## Time

| Step | Seconds | Budget | Notes |
|---|---|---|---|
| `can init` and the operator's edits | 0.1 | 30 | |
| **`make up`** on a machine without k3s | **59 to 76** (6 runs) | 600 | the promise is 300; k3s install, platform, site, forms, admin, backup timer |
| contact form submitted, from the site's link | 4 | 60 | the form refuses anything faster than 3 s |
| notification at the relay | under 1 | 180 | STARTTLS and AUTH |
| enable `work` and `support` (`make up` again) | 47 to 59 | 900 | `can up` returns after 12 to 16 s; the modules are ready 35 to 45 s later |
| SSO launch and an issue in `work` | 12 to 29 | 180 | mostly waiting for a fresh TOTP step |
| backup: dump, restic, verify, kit check | 23 to 30 | 900 | about 11 MiB of data |
| `can down` | 3 to 8 | 300 | |
| `can up --provision-only` on the "new" server | 27 | 480 | |
| `can restore` | **85 to 106** (404 before the fix below) | 1200 | `support` and its Postgres take most of it |
| `make up` after the restore | 24 to 42 | 600 | |
| **sum of the steps** | **312 to 390** | | the CI job takes about 15 minutes with the build and the idle waits |

Phases of the first `make up` (one run, seconds): base 9, images 5, company 0.3, core 6, admin 1, site 3,
routes 3, backup 9, ready 0.6; the rest is the k3s bootstrap over ssh.

## Memory (MiB, cgroup working set)

| Moment | k3s and containerd | pods | together | budget |
|---|---|---|---|---|
| after `make up`: site, forms, can-core, Traefik; idle | 564 to 608 | 68 to 71 | **633 to 678** | 1400 |
| with `work`, `support` and Postgres; idle | 735 to 743 | 180 to 183 | **915 to 926** | 2400 |
| after the restore; idle | 732 to 766 | 182 to 185 | 917 to 948 | 2400 |

The operating system is not in these figures. The report also gives `MemTotal - MemAvailable` of the whole
machine, which holds the runner and the stand-ins and moved by anything from 20 to 400 MiB between the same
moments of different runs: it says nothing about what a small server would need. The SMTP sink counts as a pod.

## What the journey found

Fixed with the change that added it:

| Found | Effect |
|---|---|
| With `--domain-live` on the plain-HTTP-fallback address, the site's catch-all and the admin host rule had the same priority | `https://admin.<domain>` served the public site after a second `can up` |
| The erasure ledger reached the bucket only once something had been erased | **a company that never erased anything could not be restored** without `--allow-no-ledger`, although `backup status` and `verify` passed |
| `can restore` waited for modules with an exposed UI or API, which cannot start before `can up` | 5 minutes of waiting and a false warning that the module had not come up |

Not fixed, each needs more than a local change:

- `can up` does not copy `can` to the server. Until the operator does, erasures never reach the bucket, and a
  restore stops without `--allow-no-ledger`.
- The `work` web UI cannot create a project. A new company needs an API call with a token, and the API is only
  reachable with `expose.api` or a port-forward.
- There is no k3d (or `local`) provider for `can up`, so only a server over ssh can walk the whole journey.

## Running it

`.github/workflows/acceptance.yml`: on demand, weekly, and on pull requests touching what it covers. It
installs and destroys k3s on the machine it runs on and refuses to run without two environment variables that
only the workflow sets. Do not run it on a computer you use.
