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
- **FR-8** A module can order erasures only of its own data: its feed is limited to its declared data classes,
  the entry names it, the restore applies it to it alone, and its share of the ledger is bounded.
- **FR-9** A restore never destroys data that was not erased: an erasure is not replayed into a dump taken more
  than a minute after it (its time is taken before the transaction commits), and `files` names the erased files,
  not the data subject. A dump whose age cannot be told stops the restore. One bad tombstone or one slow feed
  costs only itself.
- **FR-7** A dump run without the applied company.yaml fails and writes no completion marker: `can restore` needs it.

## Design decisions

| # | Decision | Why |
|---|---|---|
| D1 | can-core pulls the feeds; modules push nothing | the feeds and their tokens exist; a lost push cannot be noticed, a failed pull can |
| D2 | Every feed is read from the start each time | tombstone sequence numbers restart when a module is restored or reinstalled |
| D3 | can-core writes to `<dumps>/.erasure-ledger`, root imports it | can-core gets no access to the root-owned ledger or the bucket credentials |
| D4 | `can-erasure-sync.timer`, every 15 minutes | bounds what a lost server takes with it |
| D5 | An erasure is applied to the module instance that reported it, never to a sibling | the other instance's database was not erased |
| D6 | Replay is bounded by the dump's time, not by teaching each `purge` command a cut-off | data in a dump older than the erasure all existed when it was erased; one rule covers every module |

## Purge semantics, per module

| Module | `purge` takes | What bounds a replay |
|---|---|---|
| people, equity, board, entity | everything of the class for the subject | the dump's time (D6) |
| jobs | everything of the class for the candidate | the dump's time |
| forms | one lead (ids are never reused) | the dump's time |
| files | a file id (the feed), or a data subject (by hand, or an earlier release) | the file ids, and the dump's time |

can-core's derived copies (events, notifications, queued mail) are removed on restore whatever the erasure's time:
nothing removes them while the server runs, so no dump of can-core's state reflects an erasure.

Each of these updates the tombstone's time when the same subject is erased again, so that the later erasure is
replayed into a dump made between the two. Retention sweeps of people, equity and board write no tombstones.

## Out of scope

Erasure feeds for `finance`, `work`, `provisioning` and the wrapped apps; module-to-module network
policies (provisioning still cannot reach the people module); serving `board` and `entity` from one instance.
