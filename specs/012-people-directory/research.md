# Research and decisions: E13a people

## Decisions

**Field-level encryption in the application, not the database.** SQLite has no per-column encryption and
the pure-Go driver has no cipher extension. Application-level AEAD (XChaCha20-Poly1305 from
`golang.org/x/crypto`, already a dependency) with a random 192-bit nonce needs no nonce bookkeeping.
Binding row and column through the additional data defeats copy-between-rows attacks. The key id is in the
ciphertext (`v1.<id>.<b64>`) so a keyring can hold several keys during rotation. Plaintext columns
(`directory-basic`) are left clear because colleagues read them and they are needed for filtering.

**Encrypting 1:1 notes.** SPEC.md calls application-level encryption of manager-private notes a planned
hardening; it was cheap to do now, so both shared and private notes are sealed.

**Roles come from the token, relationships from the data.** A `manager` role claim is not a relationship:
being the *current direct manager* of the report is. The terminated-manager case (valid token, ended
employment) is handled by resolving the caller's own person record on every call.

**404 versus 403.** A caller who cannot see a record gets 404 so existence is not confirmed; a caller who
can see the person but not a field or action gets 403. Mixed requests for sensitive fields are refused as a
whole.

**HR and admin cannot read 1:1 content or private goals.** SPEC.md: private notes are "hidden from every
other role in the UI, including HR unless policy says so". The conservative reading is no; making it
configurable later is a product decision, not an accident.

**Skip-level managers.** They can read basic fields and `manager`/`team` visibility goals (the "manager of
manager" case) but not sensitive fields, 1:1s or edit goals.

**Erasure subject id.** E08's ledger (`core/backup/ledger.go`, on branch `epic/E08-backup-restore`) accepts
`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` subjects and `^[a-z][a-z0-9-]{0,63}$` classes and warns that callers
must pass opaque ids. The person id (`pers_` + 96 random bits) is opaque by construction; data class names
follow the manifest pattern (hyphens, no dots), so `directory.basic` in the brief became `directory-basic`.
`core/backup` is not on `main` yet, so this module does not import it: it exposes the tombstones through
`GET /v1/erasures` and the `purge` hook, and the contract tests copy the two patterns. Wiring is E08/E11 work.

**Retention clock.** From the day a person becomes former (`ended_at`); 1:1 meetings and closed goals also
expire by age. Retention purges do not write ledger entries (a restore re-runs the sweep and re-purges);
erasure requests do, because they are exceptions to the schedule that a restore could otherwise undo.

**Events.** SPEC.md lists `person.hired.v1` and `person.left.v1`. The brief asked for the finer set
`person.created`, `person.status_changed`, `person.offboarded`. A hire is `status_changed` to `active`;
`offboarded` carries `effective_date`, as `person.left` was specified to. SPEC.md is amended accordingly.
`checklist.task_completed.v1` is added (beyond the brief) so identity provisioning can later subscribe to
tasks with a machine `action` without this module depending on identity.

**Why no import of `core/forms`.** The webhook signing scheme is eight lines; copying it avoids coupling two
modules' internals (Constitution IV) while keeping one verifier for both.

## Alternatives rejected

- *SQLCipher or filesystem encryption only*: protects the file at rest but not against anyone who can read the
  running database; per-field control needs application-level crypto anyway.
- *Roles from the database*: duplicates SSO groups; the token already carries them (SPEC.md section 3.7).
- *A policy engine (OPA, Casbin)*: a dependency and idle RAM for a decision table that fits in 150 lines and
  can be tested exhaustively.
- *Soft-delete erasure*: leaves personal data in the file; erasure overwrites (`secure_delete`).

## Unverified (needs a cluster or other epics)

- Helm templates, `restricted` pod security admission, NetworkPolicy ingress from consumers, PVC permissions
  for uid 65532 on `/data`.
- can-core: keyring Secret generation and its backup, introspection endpoint contract for user tokens,
  event webhook route, ledger append from `GET /v1/erasures`.
- Restore ordering: replaying `purge` before workloads scale up (SPEC.md section 5) is designed for but not
  exercised.
- Idle RAM on k3d (measured locally only; see measurements.md).
- Behaviour with large directories: lists are filtered per row in the service (fine for hundreds of people,
  O(n) with a chain walk per row); needs indexing or SQL-side filtering beyond a few thousand.
- Whether employee-visible directory defaults (all staff see basic fields of active people) suit every
  company; it is policy, in one place.
