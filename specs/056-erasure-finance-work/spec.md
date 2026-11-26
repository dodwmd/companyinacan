# 056: erasures in finance and work survive a restore

**Problem.** can-core collected no tombstones from `finance` and `work`
([048](../048-erasure-collection/spec.md)), so a customer or vendor erased in finance came back with a
restore of an older backup, and the tracker had no way to erase a person at all.

## Requirements

1. Both modules have an erasure feed (`GET /v1/erasures`, admin with an unlimited token) that can-core
   collects, and a `purge -subject S -class C` command that `can restore` runs.
2. A subject is opaque. finance: `party-<id>-<creation time>`; the time keeps a replay away from a later
   party that was given the same id after an earlier restore. work: `person-<SHA-256 of the identifier>`,
   truncated; a replay finds the identifier among those in the restored database.
3. `purge` accepts one class each (`finance-party-contact`, `work-issue`), refuses a subject of another
   shape, succeeds without changing anything when the subject is not in the database, and can be repeated.
4. A replay never removes data newer than the erasure: the restore's own rule applies (048 FR-9).
5. finance replays the live party erasure itself, so what legal retention keeps is the same. Parties erased
   before this release get a tombstone from the migration.
6. work gets a live erasure of a person, `POST /v1/people/erase` (`work.admin`, unlimited token, the
   identifier in the body only).
7. Erasures are audited without personal data and the database files and write-ahead log are scrubbed; a
   scrub that did not finish is finished later.
8. An erasure ledger line left whole but without its newline by a crash is not joined to the next one.

## Personal data in work

| Held | On erasure |
|---|---|
| reporter, assignee, comment author, activity actor, who added a link or attachment, who made a token | replaced by a random pseudonym `erased-<hex>` |
| the same inside activity details and events still to be sent | replaced |
| watcher entries, cached replies for retries, delivered events that name the person | deleted |
| API tokens of the person | revoked, subject replaced, name emptied |
| titles, descriptions, comment texts, file names and URLs | **kept**: business content, edit by hand |
| the admin audit log | **kept** unchanged (accountability); deleted by audit retention |

Identifiers are compared exactly: each spelling of a person is erased by itself.

## Limits

- The work subject is a hash without a key: a guessed identifier can be tested against it. It is
  pseudonymous data, in a ledger that is root-only on the server and encrypted in the bucket. A key would
  have to be older than every backup it is used on.
- No event announces a person erasure. can-core's queued events, notifications and mail that name the
  person by identifier are not removed, live or on restore.
- finance records held by identifier of staff (timesheets, expenses, `created_by`) have no erasure.
- No web page for the work erasure.
- What became a legal record after a backup was made (an invoice approved, a contract sent) is not in that
  backup: a replay on it deletes the draft it still was.
