# Research: how can-core's dumps reach restic

## Threat model

Assets: the dumps (every database, in clear), the restic password and bucket credentials, the host. Adversaries:
a compromised can-core (internet-facing), a compromised module pod, anyone with the bucket. Goals: a module cannot
read or alter another's dump; a compromised can-core cannot gain the host or the bucket credentials; a partial or
inconsistent set is never presented as good.

## R1. Options

| | Design | For | Against |
|---|---|---|---|
| A | `local-path` PVC for can-core; host script finds `pvc-<uid>_can-system_can-dumps` | no hostPath in any pod | path changes per cluster: script, unit sandbox, verify, status and restore all need discovery; local-path creates the directory 0777 |
| A' | **Static PersistentVolume at `/var/lib/can/dumps`, bound to one claim (chosen)** | no hostPath in any pod; path stable, so the specs/008 script, restore and verify are unchanged; 0700 and owned by can-core | the directory's owner must be set on the host; the PV is a cluster object an admin applies |
| B | in-cluster restic Job in a `can-backup` namespace mounting the PVCs | no root script reading can-core's output | **a pod can only mount claims of its own namespace**, so one Job cannot mount module volumes; per-namespace Jobs would copy the bucket credentials into every module namespace; the k3s datastore, node token and encryption key are unreachable without hostPath |
| C | relax `can-system` to `privileged` for one hostPath | smallest manifest change | removes Pod Security for every workload in the namespace, including can-core. Rejected |

## R2. Decision: A'

Pod Security checks the volumes written in a **pod** spec. A claim bound to an administrator-declared PV is allowed
under `baseline` and `restricted`. can-core's ClusterRole has no `persistentvolumes` rule, so a compromised can-core
cannot declare another volume; `claimRef` binds this one to `can-system/can-dumps` only. The credentials stay on the
host, outside every pod.

What A' keeps from the threat model of specs/008: the script is root and reads a directory can-core writes. The same
checks apply (not a symlink, not world-writable, no symlink inside, marker re-check) and the same check-then-use race
remains. A compromised can-core can put false content in dumps; it cannot reach the credentials.

## R3. Who can declare the directory

`hostPath.type: DirectoryOrCreate` so that a cluster without bootstrap (`make dev`) still schedules can-core; the
directory is then root-owned and dump runs fail with a logged permission error. `type: Directory` would leave
can-core, and so the admin UI, unschedulable. Bootstrap and the backup install set `65532:65532 0700` (`fsGroup`
does not apply to hostPath volumes).

## R4. Triggering a run

A file next to the state database, created by `can-core dump-request` through `kubectl exec`. No new network
surface or token; whoever can exec into can-core already controls it.

## R5. Failing hooks

Strict: any failed hook means no marker. The alternative (marker with a partial set) would let the raw-copy
exclusion of SQLite files drop a module silently. Cost: one broken module stops all backups; visible in
`can backup status`. The erasure ledger sync still runs first.

A hook that exits 0 without uploading also fails the run, and each run's uploads are staged and swapped in as a
whole, so an earlier file never stands in for a missing one. Disabled and failed modules are dumped too (their
upload is admitted only during their own hook). The raw-copy exclusions are decided by the script at run time from
`DUMPED`, so a module added or removed after the install cannot be excluded without a dump.

## R6. restic provisioning

Distribution packages are older than 0.17 (needed for exit code 10). The upstream release binary is downloaded and
checked against sha256 values taken from the release's `SHA256SUMS` (v0.18.0). There is no backup container image,
so no image matrix or Dockerfile pin changes. Dependabot cannot bump this pin; it is a manual, reviewed change.

## R7. Hook pod network

A new pod's NetworkPolicy rules are programmed shortly after it starts. The test hook retries refused connections
(`--retry-all-errors`); the shipped hooks use `--retry 3`, which does not retry a refused connection. Open item.
