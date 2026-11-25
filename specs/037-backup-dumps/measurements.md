# Measurements: E08b on k3d (GitHub-hosted `ubuntu-latest`)

One run each, 2026-11-25, commit f873d03. Fresh install, default bundle (forms), almost no data: these say nothing
about how the times grow with data, and a real server and bucket will differ.

`make-up-e2e` (run 45715055166):

| Step | Value |
|---|---|
| dump run, request to completion marker (state.db and company.yaml; no module hooks) | 2.2 s (includes the up to 5 s request poll) |
| backup script: restic backup, forget, prune into a local filesystem repository | 2.2 s |
| largest process of the backup script, max RSS (restic) | 83124 kB |
| can-core VmRSS and VmHWM after the dump | 80540 kB (limit 96 MiB) |

`dev-cluster` (run 45715055118):

| Step | Value |
|---|---|
| dump run with one module hook Job (stub module, includes pulling the curl image) | 14.4 s |

can-core's 80 MB is higher than the 55 MB after logins recorded in specs/005. The same run had done admin logins
(Argon2) before the dump, so the share of the dump is not known. It leaves about 18 MB under the limit; measure a
larger state database before relying on it.

There is no backup pod (design A'): the backup's memory is restic's on the host.
