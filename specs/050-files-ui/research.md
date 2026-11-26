# Research: 050 files web UI

## R1. Search

The Store has no search: file, folder and space names are sealed per field, so a search would have to open
every name the caller may see through a new read path with its own role filter and audit rule. Not added.

## R2. RAM and binary size, measured locally

Static linux/amd64 build (`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`), UI on, local backend, no
scanner, on the developer machine; `/proc/<pid>/status` and `/proc/<pid>/stat`. Sequential load: one 24 MiB
upload, three downloads, 50 small uploads, 200 page renders. Concurrent load, three rounds: six clients
browsing while two upload and four download 24 MiB. Two runs each.

| Build | RSS idle | RSS after sequential | RSS peak, concurrent | Anonymous, concurrent | CPU, sequential pages |
|---|---|---|---|---|---|
| no memory limit (shipped) | 14.7 MiB | 24.0 to 24.2 MiB | 26.9 to 27.1 MiB | 13.4 to 13.5 MiB | 0.17 to 0.18 s |
| `GOMEMLIMIT=24MiB` (one run) | 14.7 MiB | 24.0 MiB | 27.5 MiB | 13.8 MiB | 0.17 s |
| 10 MiB soft limit (rejected) | 14.9 MiB | 22.1 to 22.3 MiB | 23.1 to 23.4 MiB | 9.0 to 9.3 MiB | 2.35 to 2.37 s |

A 10 MiB soft limit was tried first to stay within the former 20 MB declaration. It made the collector run
almost continuously (about 13 times the CPU) to save 2 to 4 MiB, so it was removed. The declared RAM is
raised from 20 to 28 MB instead (as work and finance, which also have a UI); the limit stays 64 MiB. Without
the UI the module is as before. These are runs of the binary on one machine, not a container on a cluster.

The binary grew from 12.2 MiB to 14.8 MiB: `html/template` calls methods by name, which stops the linker
from dropping unused ones (the other UI modules are 16 to 17.5 MiB). The size budget in
`dev/footprint/budgets.json` is raised from 13.5 to 16 MiB.

The compose container measured 9.0 MiB anonymous after the files e2e scenarios, on the former loaded budget of
9 MiB, so the `files` footprint budgets are now 12 MiB loaded anonymous and 36 MiB peak RSS (idle unchanged).

## R3. Reading an upload form

`mime/multipart` accepts up to 10 MiB of headers per part and buffers them. Six concurrent requests of that
kind took the process from 17 to 75 MiB (past the 64 MiB limit), before any role check or upload slot. The UI
now (1) checks the permission, the scanner and the space role and takes one of the Store's upload slots before
a byte of the body is read, and (2) reads at most 64 KiB before the file's own bytes: preamble, part headers,
CSRF token and fields together. The same attack then peaks at 21 MiB. The API's upload takes the file as the
raw body, with no part headers to buffer, and reads nothing for a caller who may not upload.
