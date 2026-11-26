# Feature 051: verified holder identity in equity (E17d)

Status: built offline, experimental. Unit-tested, and the directory client is tested against the real people
API in process. Not run on a cluster or in the compose stack. This is a tool, not legal, tax or securities
advice.

## Why

Equity's separation-of-duties checks (grants, exercises, approvals, manual ledger entries, self-erasure)
compared a stakeholder's `person_id` that nothing verified, with a caller's `person_id` that personal access
tokens did not have. A second record, a made-up id or a token without the claim got around them.

## Modes

| Mode | When | Who the caller is | How a holder record is established |
|---|---|---|---|
| `directory` | `directory.url` is configured | the person whose `sso_subject` the people directory has for the caller's subject | with a `person_id`: only `verify-person`; without one: `attest-external`; by a second actor either way |
| `attestation` | no `directory.url` (people not installed or not reachable by design) | whatever the introspection answer claims; nothing for PATs | `attest-external` by a second actor, with or without a `person_id`, which proves nothing |

## Requirements

- **FR-001** In directory mode a `person_id` given when a record is created or linked must name a person in
  the directory who is not erased.
- **FR-002** In directory mode the caller's person comes only from the directory (`GET /v1/people?sso_subject=`),
  for PATs and introspected sessions alike. An introspected `person_id` claim that disagrees leaves the caller
  unlinked. Answers are cached for 15 seconds.
- **FR-003** In directory mode these need a linked caller (403 `identity_required`): create, link, confirm,
  attest and edit a holder; draft, approve and administer grants; request, approve, pay and issue exercises;
  ledger entries; erasure and legal hold. The service account has no person.
- **FR-004** Fail closed: when the directory cannot answer, those actions and every person link check answer
  503 `directory_unavailable`. Reads allowed by role still work; self-service reads do not.
- **FR-005** A grant leaving draft, an exercise being approved, paid or issued, and every manual ledger entry
  (issue, transfer both sides, cancel, repurchase) need an established holder (erased records: FR-013). This is the "second person" for
  ledger writes: one per holder record, not one per entry.
- **FR-006** Nobody creates a record linked to their own person, links a record to or away from themself,
  writes their own identity, or erases their own record.
- **FR-007** `verify-person` is refused to whoever created the record, linked it or last set its identity. The
  holder may confirm their own record only if neither the creator nor the linker was a service account (two
  people still stand behind it; without this a company of two founders could not establish them).
  `attest-external` is refused to the same three, to the record's own person and to the service account.
  Database triggers repeat the creator rule.
- **FR-008** Changing the identity (`PUT /identity`) or the person link, and erasure, clear the confirmation
  and the attestation. A grant cannot be approved by its holder, or by whoever created, edited or submitted it.
  Who created, linked or confirmed the **holder record** is not excluded: that record already took two actors,
  and excluding them too left a company of two with nobody able to approve. An approval by one of them is
  audited as `grant.approve.by_record_establisher`.
- **FR-009** people: nobody sets, changes or clears the `sso_subject` of their own account (their current
  record, or any record to their own subject). The `sso_subject` list filter is hr and admin only.
- **FR-010** Upgrading trusts no existing `person_id` and no earlier attestation: migration 004 clears
  `external_attested_by` and `external_attested_on` (the audit log keeps who attested what). In directory mode
  a record with a `person_id` is established only by `verify-person`, so a record attested while there was no
  directory is not trusted after one is configured. Unestablished records are listed by
  `GET /v1/identity/review` and `can-equity identity-report` and counted in a start-up warning. No record is
  deleted; reads, grant terms and the ledger are unchanged; already approved grants keep vesting.
- **FR-011** Links, confirmations, attestations, cleared confirmations, refusals and every break-glass use are
  in the audit chain (ids only).
- **FR-012** Break-glass (below) is set only in the process environment, ends by itself and is at most 7 days
  ahead.
- **FR-013** An erased record cannot be confirmed again. Its shares can still be cancelled, repurchased or
  transferred away (never added to) by a caller other than whoever erased it, not by the service account
  outside break-glass, audited as `ledger.erased_holder`. It stays in the review list (`erased_holder`) while
  it holds shares. A record erased by the retention sweep or before this feature has no recorded eraser, so
  one identified finance or admin user can do it.

## What changes for an existing installation

After the upgrade no grant can leave draft, no exercise can proceed and no manual ledger entry is accepted
until the holder in question has been confirmed or attested by a second actor. This is deliberate. Before
upgrading, know who the second actor is; afterwards run `can-equity identity-report`.

For directory mode, also: give every person who acts in equity (including board members and outside finance
staff) a directory record whose `sso_subject` is their account, set by someone else; configure
`directory.url` and `CAN_EQUITY_DIRECTORY_TOKEN` (a people token with `people.read` and `people.write`, the
least that sees `sso_subject`).

## Who can do what in a company of one or two

A grant has three roles that must be different actors: the holder, whoever drafts and submits it, and the
board member who approves it. Exercises likewise: the holder, the approver and payer, and the issuer. Only
directory mode ties those actors to different people (the service account counts as an actor and is not a
person). The tests (`identity_paths_test.go`, `identity_claims_test.go`) try the assignments.

| Two founders, both administrators and board members | Directory mode | Attestation mode |
|---|---|---|
| Establish each founder's record | each creates the other's; each confirms their own | one creates, the other attests |
| Founder shares (ledger) | each issues the other's | each issues the other's (nothing stops a founder using a token from issuing their own) |
| Grant to an employee | one drafts, the other approves | service account (or an introspected user with a `person_id`) drafts, a founder approves: tokens carry no person and cannot draft, as before |
| Exercise by an employee | one approves and records payment, the other issues | same |
| Grant to a founder | **break-glass**: the service account drafts, the other founder approves | service account drafts, the other founder approves |
| Exercise by a founder | the other founder approves and records payment, **break-glass** service account issues | the other founder, then the service account issues |

**Two founders, directory mode, after approval.** Terminating a grant and recording a milestone or an
acceleration trigger need an actor who did not create, edit, submit or approve it. With one founder drafting an
employee's grant and the other approving it, nobody is left: **break-glass**, and the service account records
it. Activating and cancelling have no such rule.

**A single administrator, directory mode:** establishing a record and a grant to an employee take break-glass
(the service account is the second actor; the administrator, holding the board role, approves). A grant to
**themself** cannot be approved by them in any way: it takes another board member, as before this feature.
Their founder shares are recorded with break-glass.

**A single administrator, attestation mode** (what `can` installs without the `people` module): none of that
holds. An administrator who also holds the board role and has the service account token **can give themself a
grant, without break-glass**:

1. the service account creates a holder record for them (creator);
2. the administrator attests it (confirmer);
3. the service account drafts and submits the grant (drafter);
4. the administrator approves it with the board role (approver).

The same steps give an employee a grant, and the administrator can then write ledger entries, their own shares
included. Every step is audited, but nothing refuses it. What stands in the way is only who holds the board
role and who can read the service account token (whoever controls the deployment can).

## Break-glass (single-administrator companies, and recovery)

A personal account cannot be two actors (in attestation mode one person with the service account token already
is two: see above). Set `CAN_EQUITY_BREAK_GLASS_UNTIL=YYYY-MM-DD` (inclusive, UTC) on the
can-equity process, restart, do the task with the **service account** token, then remove the variable. Inside
the window the service account needs no person, may confirm or attest records it did not create, and may write
the ledger and run the grant and exercise chain for unestablished holders. It still cannot approve a grant
(it never has the board role). Personal accounts get no waiver. Every such use writes a `break_glass:<action>`
audit entry, and start-up logs a warning. Setting it needs control of the deployment; whoever has that could
also edit the database, so it adds an audited path rather than a new power.

In people, the equivalent for linking the only administrator's account is the service token
(`CAN_PEOPLE_API_TOKEN`).

## Threat model

**Attacker:** a company insider with equity write rights (hr, finance, admin, board) who wants to grant,
exercise or record shares for themself, approve it alone, or hide it.

**Stopped (in directory mode):** a made-up `person_id` (including one recorded or attested while there was no
directory); a second or borrowed-identity record used alone;
confirming or attesting one's own entry; renaming or relinking an established record and carrying on; using a
second account or the service token as the "other person" (it is not linked to a person, and the directory
links one account per person); linking or unlinking one's own account; acting while the directory is down.

**Still possible, and not claimed otherwise:**

1. **Collusion.** Two people can do together anything the rules ask two people for.
2. **A careless confirmer.** The service compares no names: a confirmer who does not read the identity confirms
   a shell record or a borrowed person id.
3. **One confirmation, many entries.** After a record is established, one finance or admin user can write any
   number of ledger entries to it, if they are not recorded as its holder. A shell record that got past 1 or 2
   stays usable until its identity or link changes. Per-entry approval of ledger writes is not built.
4. **Control of the directory or of accounts.** Someone who can edit the people directory can invent persons and
   link other accounts to them; someone who can also create can-core accounts can then be two actors alone. The
   people service token, and the token equity holds (it has `people.write`), can set any `sso_subject`,
   including the caller's own.
5. **Control of the deployment.** Environment (break-glass, removing `directory.url`), secrets, pod exec or the
   SQLite file defeat every rule here. The audit chain shows changes only against a copy of its head kept
   elsewhere.
6. **Attestation mode is much weaker.** There is no link between an account and a holder, so a holder can be
   the attester of their own record entered by someone else, or write their own ledger entries once someone
   else attested the record. One administrator with the board role and the service account token can give
   themself a grant without break-glass. Two accounts or credentials of one human are two actors. The service account can
   create records and run grants and ledger writes on established holders, as before (it cannot attest).
7. **Reads, structure, convertibles and valuations** have no holder identity check.
8. **Approving one's own shell record.** Someone who entered a record for themself under another name, and got
   a second actor to confirm or attest it, can approve a grant to it that somebody else drafted. (Before this
   feature the record's creator and attester could not.) It is audited, and it needs a second actor twice.
9. **Work already in flight.** Grants approved before the upgrade stay approved and can be activated, cancelled
   or terminated; their exercises need an established holder.

## Not built (follow-ups)

can-core's wiring of directory mode and a read-only lookup credential are built by
[specs/057](../057-module-capability-wiring/spec.md).

- Per-entry second-actor approval of manual ledger writes.
