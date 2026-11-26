# Install configuration and erasure collection

Two gaps found after E08c: some in-house modules could not start as can-core installed them, and nothing
wrote the erasure tombstone ledger that a restore replays.

## Requirements

- **FR-1** Every in-house module installs through can-core and becomes Ready. Configuration that only
  `company.yaml` knows (jobs' `company.name` and `company.base_url`, files' `share.base_url`) is set by the
  installer, and a change of it is an upgrade. A credential one module needs of another (provisioning's
  directory token) is generated and copied by can-core, with the least permissions that do the job.
- **FR-2** can-core collects the tombstones of every enabled module with an erasure feed into an
  append-only, hash-chained ledger that survives dump runs, without anyone acting.
- **FR-3** The ledger holds opaque subject ids, data classes and dates only. A subject that is not an opaque
  id is refused, counted and reported; it is never written or logged.
- **FR-4** The collected entries reach the host ledger and the bucket without waiting for the nightly backup.
  The host part runs as root over a directory can-core writes: links, edits of the chain and oversize files
  are refused.
- **FR-5** What is not covered is said: modules with data classes and no feed, feeds that could not be read,
  and a collection that is old.
- **FR-6** A restore merges, and never overwrites, the erasures already on the server.

## Design decisions

| # | Decision | Why |
|---|---|---|
| D1 | can-core pulls the feeds; modules push nothing | the feeds and their tokens exist; a lost push cannot be noticed, a failed pull can |
| D2 | Every feed is read from the start each time | tombstone sequence numbers restart when a module is restored or reinstalled |
| D3 | can-core writes to `<dumps>/.erasure-ledger`, root imports it | can-core gets no access to the root-owned ledger or the bucket credentials |
| D4 | `can-erasure-sync.timer`, every 15 minutes | bounds what a lost server takes with it |
| D5 | `board` and `entity` tombstones of shared classes are replayed into both instances | one binary writes both classes whichever instance was asked |

## Out of scope

Erasure feeds for `finance`, `work`, `forms`, `provisioning` and the wrapped apps; module-to-module network
policies (provisioning still cannot reach the people module); serving `board` and `entity` from one instance.
