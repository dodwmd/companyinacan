# Feature specification: a restore that works (epic E08c)

**Branch**: `epic/E08c-restore` | **Input**: SPEC.md section 6.5, [specs/008-backup-restore](../008-backup-restore/spec.md)
(state machine, ledger, kit), [specs/037-backup-dumps](../037-backup-dumps/spec.md) (what a snapshot holds).

Before this feature `can restore` stopped at its first step: neither the cluster adapter nor a restore mode existed.

## User scenarios

1. **The server is gone** (P1). The operator has the recovery kit. They make a new server with k3s
   (`can up --provision-only`), put `can`, `restic` and the kit on it and run `sudo can restore --kit`. The admin logs
   in with the same password and TOTP, module data is back, and nothing that had been erased came back.
2. **Something fails half way** (P1). The message names the step; the server stays in restore mode (nothing public,
   nothing sent); running the same command again continues.
3. **Wrong server** (P1). Running it where something already runs is refused unless the company is named.
4. **Finding out early** (P2). `can backup verify` fails for a snapshot that a restore would refuse or could not
   complete.

## Decisions

| | Decision | Why |
|---|---|---|
| D1 | `can restore` runs **on the server**, as root | restic, the volume directories and the dumps directory are local; no new remote file protocol. Provisioning stays `can up --provision-only` |
| D2 | can-core exports Secrets into the dumps (`can-core/secrets.json`) | SPEC 6.5. Module keys otherwise exist only inside the k3s datastore copy, and a raw datastore restore would start every workload before erasures are applied |
| D3 | Restore mode is a **latch on can-core's volume**, set by `CAN_RESTORE=<id>`, removed only by `can-core restore complete` | re-applying the normal manifests (or `can up`) must not end it |
| D4 | The cluster half runs as `can-core restore ...` through `kubectl exec` | no new network API; can-core already has exactly the permissions to create module namespaces, Secrets and Jobs |
| D5 | Modules are installed **quiesced** from the recorded module list: 0 replicas, CronJobs suspended, no Jobs, no Ingress/IngressRoute | disabled modules have data too; no seeding; no public route to un-purged data |
| D6 | Order: volume files first, then restore hooks | SPEC 6.5 had hooks first; a hook is replace-all, so a later raw copy would put older files over it, and the copy refuses a volume with data |
| D7 | Erasure replay fails **closed** | a tombstone no module can replay, or whose purge fails, stops the restore until `--accept-unerased module:class` names it |
| D8 | Go-live re-applies modules by clearing their recorded settings hash | the normal reconcile restores replicas and routes; no second copy of the desired state |

## Requirements

- **FR-001** Steps: fetch and check; dumps; platform with can-core in restore mode; Secrets and quiesced modules;
  volume files; restore hooks; erasure replay; go live. Each is recorded and idempotent.
- **FR-002** Before the server is changed: snapshot choice (specs/008 rules), marker and manifest hashes, state
  database integrity, company.yaml, Secrets export, every dump loadable, ledger present and not behind.
- **FR-003** A server that is not blank (dumps or host-state files, ledger, restic password, can-core or its volume,
  module namespaces) is refused unless `--confirm-overwrite <company name>`.
- **FR-004** In restore mode can-core runs no email worker, webhook dispatcher, dump runner, reconcile, timer or
  housekeeping; the admin listener answers only the probes; the module API only the probes and the dump download.
- **FR-005** Import preserves the admin account and TOTP seed. At go-live queued mail is discarded (it may have been
  sent after the backup), sessions and launch tickets are deleted.
- **FR-006** Purge replay: the module's `hooks.purge` script; `people`, `equity`, `board` run their `purge` command
  in a Job from their own Deployment; `files` and `jobs` are started without routes and called on `/v1/purge`.
  can-core removes its queued events, notifications and mail that name an erased subject, and the dumps of purged
  modules.
- **FR-007** No incomplete restore without a named override: `--without-secrets`, `--accept-unrestored`,
  `--accept-unerased`, `--allow-no-ledger`, `--accept-ledger-behind`, `--force-older`.
- **FR-008** `can backup verify` applies the FR-002 checks of the snapshot.
- **FR-009** The ledger and the repository credentials are written back to the server; `can up` afterwards keeps the
  restored admin account.
- **FR-010** (#49 follow-ups) A database module that was never provisioned does not block backups; a re-run keeps the
  backup region; rotated credentials are tried before they replace `restic.env`; alerts lead with
  `DATABASE IS NOT BACKED UP` and warnings are not worded as failures.

## Success criteria

- **SC-001** In CI (`restore-e2e`): seed a company on k3d, back it up with the installed script to a local restic
  repository, erase a person, destroy the cluster, restore inside the node of a new cluster. The admin login,
  Secrets, a sealed field, a form lead, a raw volume file and a hook-restored dump are back; the erased person is
  absent from the API and from the database files; an unreplayable tombstone stopped the first attempt; re-applying
  the normal manifests did not end restore mode; a second restore over the live server is refused.

## Threat model

Assets: the recovery kit; the repository and ledger object; the Secrets export; the restored personal data; the
erasure promise.

| Threat | Mitigation | Residual risk |
|---|---|---|
| The Secrets export leaks | written 0600 into a directory only can-core (uid 65532) and root read; leaves the server only inside the encrypted repository; never printed (parse errors do not echo it) | root on the server, or a compromised can-core, reads it; both could already read every module Secret |
| Someone with the kit reads everything | none added: the kit has always been the key to the repository | the kit is now also enough to reconstruct all module keys. Guard it as the company's master key |
| Restore overwrites a working server | root required; blank check before fetching and again before the first change; confirmation by company name; refused restores leave no state; the old timer and can-core are stopped first, and a volume is not written while a pod uses it | the check looks for what this platform creates; unrelated services on the host are not detected |
| Erased data comes back | ledger read before changes and again at replay; union with the snapshot and local copies; fail closed; no routes or replicas before replay; purged modules' dumps removed | erasures never written to the ledger are unknown. **can-core does not write the ledger yet** (`can backup ledger-add` by hand). A consistent roll-back of the whole bucket is undetectable (specs/008 R12) |
| Restore sends mail or webhooks again | workers not started in restore mode; module email and events refused; queued mail discarded at go-live | undelivered events to modules are delivered after go-live (consumers de-duplicate by event id). `files` and `jobs` run during their purge and may attempt outbound calls of their own |
| Restore mode ends early | latched on disk; only `restore complete` removes it | an operator with cluster-admin can delete the latch |
| Hostile data in a snapshot (the repository is writable by whoever has the bucket credentials) | manifest hashes; paths in the manifest cannot escape; volume paths come from the cluster, not the snapshot; copies never write through links already in the destination; module directories are chosen by claim name | the snapshot's state database, Secrets and files are trusted: whoever can write the repository **and** has its password controls the restored company. Symlinks inside volumes are restored as they were |
| Credentials revoked after the backup work again | sessions and tickets deleted at go-live | personal access tokens, admin users, role assignments and passwords are those of the backup: review them after a restore |
| Module routes or purge API reachable during replay | Ingress objects are not applied; admin listener is closed | pods in the cluster that NetworkPolicy admits could reach a module started for its purge |
| The old server is still alive | warned at the end of a restore | nothing stops two servers from backing up to one repository or both sending mail |

## Out of scope, and not verified

Creating the server and installing k3s (`can up --provision-only`), installing restic, publishing the site (it comes
from the company repository, not the backup), routes, DNS and the backup timer (`can up` afterwards). Not restored:
database **servers** in `can-data` (the operator's own; databases are loaded into them from dumps), Traefik's
certificate store, the raw k3s datastore. Not verified: a real S3 bucket, a real server, systemd, the release image
paths (`--release-manifest`), `--images-from` import into k3s, `--confirm-overwrite` on a real cluster, restore hooks
of the shipped wrapped modules and database modules (the `can-hooks` image is not published), `equity`, `board`,
`files` and `jobs` purge replay against a real cluster (unit tests with fakes only), large data sets and the
15-minute target.
