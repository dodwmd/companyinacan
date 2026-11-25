# Research and decisions: E17b board and entity

> Tool, not legal advice. Nothing here is a statement of what any jurisdiction requires.

## Decisions

**R1 One binary, two modules.** Options: (a) one module with two capability sets, (b) two modules and two
services, (c) two modules served by one binary. Chosen (c). A resolution authorises register entries and a
filing, minutes and registers share people and ids, and the audit chain and keyring are one concern; two services
would double the idle RSS (a measured ~14.4 MiB each) and the key handling. Two manifests keep roles, data
classes and capability names apart so an operator who only wants board packs does not take `entity` roles, and
`entity` is not a sub-feature of `board`. The price: can-core cannot deploy two manifests as one release (no
Helm templates exist yet; `modules/entity/chart/values.yaml` names `sharedRelease: board`). (a) was rejected
because the manifest schema gives one `data_classes` list and one role set per module and the equity lesson is
that a role defined broadly gets used broadly.

**R2 Quorum and thresholds are data and integers.** `quorum_num/quorum_den` of the members in term, an absolute
minimum, a flag for whether the recused count, and `pass_num/pass_den` strict or inclusive. `QuorumRequired` is
`ceil(eligible × num / den)` computed as `(eligible × num + den − 1) / den` in int64, floored at the minimum and
at one; `TallyPasses` compares `for × den` with `num × (for + against)`. No float touches either; a property test
compares the integer rule with the exact comparison on a million-sized counts and checks an abstention never
changes the result, and a fuzz target checks quorum minimality. Jurisdictions differ (proxies, casting votes,
weighted votes, reserved matters); none is modelled, and the docs say so.

**R3 Who may vote is fixed when voting opens.** The voter list is a snapshot (members in term, minus subjects and
the recused) held in a table that triggers freeze; it makes quorum, the eligible count and "everyone signed" exact
and prevents a member added or removed mid-vote from changing the arithmetic. Interests must be declared before
voting opens: a late declaration means withdrawing and proposing again, which is explicit and auditable (the
alternative, removing someone from a vote in progress, needs a rule about votes already cast).

**R4 Proposer is never the sole approver.** The equity QA finding (grantee and director self-approval, creators
and attesters in the chain) generalises: a resolution must have a "for" vote from someone who is not the proposer
(by subject folding or person id, one function). A resolution requested by another module counts the requester as
the proposer. A secret ballot cannot tell who voted, so the rule becomes "at least two for ballots". It is
conservative and documented.

**R5 Subjects are recused by data, not by good manners.** `subjects` on a resolution (the grantee of an equity
grant, the director whose pay is voted) take the person off the voter list and out of the quorum count
automatically, and withhold the text from them, so a self-approval does not depend on the director declaring
anything.

**R6 Secret ballot without a link.** Receipts (`sha256(resolution|person)`) stop a second vote; ballots hold a
random id and the sealed choice with no voter, no time and no rowid order (`WITHOUT ROWID`, random key). The
audit trail still says that a person voted (not how), which is stated as a limit, as is the small-electorate
problem. The tally is computed at close by opening every ballot.

**R7 Written consent = unanimity with signature records.** Other consent rules (majority consent, deemed consent,
silence) are jurisdiction-specific and not implemented. A signature is an opaque reference to the signing
provider's record, never bytes. It closes by itself on the last signature or the first decline, and at the
deadline by the sweep. Silence is never consent.

**R8 Amendments create versions; minutes are versions.** A resolution version table (append-only, only before
voting) and a minutes table with `draft | approved | superseded` give a complete history without ever editing
what was approved or voted. A content hash is stored next to each sealed text and frozen by trigger, so the
immutability rule can be enforced in SQL without reading plaintext, key rotation can still re-seal, and a
swapped ciphertext is caught on open.

**R9 Registers are ledgers validated by replay.** An entry is accepted only if replaying the whole register with
it keeps the invariants (no double office, no negative holding on any date, cease at zero, charge open to
satisfy). That is the only way a backdated entry is checked against later dates, and it costs a replay of one
entity's register per write, which is small. Corrections are new entries naming the old one (`corrects_id`); views
ignore the corrected entry, the ledger keeps both. Views take `as_of` (effective date) and `known_at` (recorded
by), because the second is what "what did the register show on that day" means when entries are backdated.

**R10 Entity vs equity.** The members register does not read `equity` and equity does not read it; both link by
opaque class ids. Reconciling them is a later feature (and a policy question: the share ledger in `equity` is the
operator's working record, the members register is the statutory one).

**R11 Filing reminders.** Obligations are data; due dates are instances; the next instance is created when one is
recorded, anchored on the first due date so month ends do not drift. The sweep announces the reached, unannounced
reminder point once and marks earlier points sent; delivery is at-least-once through the outbox and consumers
de-duplicate on the event id. The event carries ids and dates only.

**R12 Roles.** `auditor` is reserved for can-core, so the external auditor is `audit-viewer`. `admin` sees
structure and nothing else (the equity finding that an operator role should not read the sensitive content).
`integration` exists so the can-core service account can request a resolution and read its outcome without
holding `secretary`. `employee` exists only to see action items assigned to them. Role names are distinct from
the equity module's (`board` there is a role for approving grants; here `director` is the member of a body).

**R13 Approvals declaration.** `approvals[].type` must match `^[a-z][a-z0-9-]*$`, so `board.resolution` cannot be
written; the manifest declares `resolution` and the documents call it `board.resolution`. The default chain is
`[board]`: the vote of the body is the chain.

**R14 Erasure.** The board module stores no names. Erasure clears the sealed text of declared interests; the
entity module strips addresses, dates of birth and email from register detail and the agent record, keeping the
name the register must show. Everything that is the record of a decision or of who sat (terms, votes, attendance)
stays under legal retention, and the response lists it. This matches the equity pattern (the ledger survives,
the identity does not).

## What was not verified (no cluster, no container runtime)

- Footprint on k3d and in a container; the measured RSS is a local process.
- Helm templates, the keyring Secret mount, token introspection routing, the equity approval adapter, and one
  release for two manifests.
- Behaviour with a real identity provider that supplies `person_id`.
- Any legal correctness of a body's configured rules: that is the operator's and counsel's job.

## Alternatives rejected

- Storing names in the board module: duplicates the directory and makes erasure harder; opaque ids only.
- A running tally while voting: leaks how a vote is going to those still to vote.
- A generic workflow engine for resolution states: three explicit transitions and a trigger are simpler.
- Putting quorum rules in code per jurisdiction: unbounded, wrong by default; data on the body instead.
