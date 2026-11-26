# Research: 050 files web UI

## R1. Search

The Store has no search: file, folder and space names are sealed per field, so a search would have to open
every name the caller may see through a new read path with its own role filter and audit rule. Not added.

## R2. RAM and binary size, measured locally

Static linux/amd64 build (`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`), local backend, no scanner,
on the developer machine; `/proc/<pid>/status`. "Base" is the commit before this feature. API load: one
24 MiB upload, three downloads, 200 lists. UI load: sign-in, one 24 MiB upload, three downloads, 50 small
uploads, 525 page renders.

| Build and load | RSS idle | RSS loaded | Anonymous loaded |
|---|---|---|---|
| base, API load | 12.9 MiB | 19.3 MiB | 8.7 MiB |
| new, UI off, API load | 14.6 MiB | 21.5 MiB | 8.6 MiB |
| new, UI on, UI load, default GC | 14.7 MiB | 24.9 MiB | 11.7 MiB |
| new, UI on, UI load, soft limit 10 MiB | 14.7 MiB | 22.7 MiB | 8.7 MiB |

The declared 20 MB is kept (loaded is under 20 x 1.2, the limit stays 64 MiB). To get there the service sets a
soft Go memory limit of 10 MiB when the UI is on and `GOMEMLIMIT` is unset. These are single runs of the
binary on one machine, one request at a time, not a container on a cluster.

The binary grew from 12.2 MiB to 14.8 MiB: `html/template` calls methods by name, which stops the linker
from dropping unused ones (the other UI modules are 16 to 17.5 MiB). The size budget in
`dev/footprint/budgets.json` is raised from 13.5 to 16 MiB.
