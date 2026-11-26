# 054: erasing one contact-form lead

**Problem.** The forms module could only lose a lead to retention. Answering an erasure request meant
editing the database.

**Requirements.**

1. Find the leads that name an email address, in any field, ignoring case; erase one by id. API, `can leads`,
   admin UI.
2. Permission `leads.erase`, apart from `leads.write`. The admin UI also needs a recent proof of identity.
3. Search and erasure are audited. No audit entry, log line or event holds the address or anything submitted;
   the address travels in request bodies and standard input only.
4. Erasing removes the delivery records and the waiting notification, stops a send in progress, and scrubs
   the database files and write-ahead log; a scrub that did not finish is finished later.
5. Consumers of `leads` are told the id (`lead.erased.v1`), until they accept it.
6. Tombstones (`GET /v1/erasures`) follow the erasure ledger's rules: opaque subject (the lead id), a class the
   module declares (`lead-submission`), the time of the erasure. `POST /v1/purge` is the idempotent replay
   hook of `can restore`. Lead ids are never reused, so a replay cannot destroy later data.
7. Retention purges make no tombstone: restored data keeps its `retention_until`.
8. Until a collector reads the feed, every answer, `can leads erase` and the admin UI show the
   `can backup ledger-add` command, and none of them says a restore will apply the erasure by itself.
9. The module version is 0.2.0: installations record a manifest again only when the version changes.

**Limits.** In [modules/forms/README.md](../../modules/forms/README.md#erasure-requests): mail already handed
to can-core or sent, copies held by consumers, exports, and backups until they expire.
