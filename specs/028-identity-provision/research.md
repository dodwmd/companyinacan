# Research and Decisions: Identity Provisioning (E13d)

**R1. An in-house module, not a can-core service.** SPEC.md describes `identity.provision` as backed by
the SSO user store inside can-core. The epic asks for a module with its own SQLite state, connectors and
a public SCIM endpoint, which does not belong in can-core's footprint. The module provides the
capability; can-core keeps only the revocation of its own credentials (R3). SPEC.md's wording is not
changed in this epic.

**R2. Events are hints, the directory is the authority.** Every workflow re-reads the person and
refuses on a status mismatch. The only thing taken from an event is the id of the person and an
effective date. `candidate.hired.v1` has no person id and would start a second joiner next to
`person.created.v1`, so it is not subscribed.

**R3. The kill switch cannot be a generic revoke.** Options: (a) revoke any `user:` subject for a module
that asks; (b) have can-core call the people API to check the status; (c) have can-core record the
people module's own `person.offboarded.v1` and bind the account by a hash. (a) lets a compromised module
end anyone's access. (b) needs a credential for the people API that can-core does not have (it reads no
module secrets). (c) works with what exists: the bus knows each event's authenticated source, so a
forged event from another module is ignored, and `subject_ref` (SHA-256 prefix of `sso_subject`, 128
bits) binds the account without putting the subject in an event. Cost: the people module's event gained
one optional field, and the revocation applies only when `sso_subject` is the can-core subject.

**R4. Persisted state machine, one worker.** Runs and steps are rows. A step is written `running` with
its attempt count before the connector call and settled in one transaction with its audit entry and
event. After a crash a `running` step is retried with the same key. One worker pass advances a bounded
number of steps; there is no goroutine per run.

**R5. Leaver steps never block each other.** A dead step in a leaver run does not fail the run
immediately: the other accounts are still disabled and the run ends `failed`. The can-core revocation is
ordered after the account steps because can-core refuses it until the people module has reported the
person `former`, which can lag the last day by a minute.

**R6. SCIM users are read-mostly.** The directory owns users. Allowing an IdP to create, rename or
deactivate users would let an external system alter employment records. Only the preferred name and the
work phone, which the person may change themself, are writable. `active` is derived, with `leave`
masked as active. Candidates are hidden.

**R7. Filters are parsed, not interpreted.** `eq`/`and` only, an attribute allow-list, bounded length and
terms, single pass, matched in memory. A fuzz target exists. Pagination scans the directory page by page
and is bounded; a bigger directory needs a narrower filter (`tooMany`).

**R8. SSRF.** Connectors use one client: non-public destinations are refused at connect time with
`netguard.DenyPrivate`, redirects are not followed, no proxy, bounded body, deadline. `allow_private`
exists for an in-cluster target the operator names. Error values carry a machine code only.

**R9. Measurements** (this machine, 2026-11-25, `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`,
linux/amd64): binary 12,468,384 bytes; idle VmRSS (6 s after start, empty database) 14,356 and 14,492
kB in two runs (about 14.0 and 14.2 MiB); after 300 authenticated API requests and 300 SCIM requests
with a bad token 19,148 and 19,444 kB (about 18.7 and 19.0 MiB). Not measured with live connectors, a
large directory or concurrent runs.

**R11 (QA follow-up).** Access removal is never gated by an approval (an unanswered approval would leave a
leaver enabled and, being sequential, block every later account and the kill switch): only deletion is,
and it is planned last. The worker selects only due runs (`wake_at`), oldest examination first, so runs
that can only wait cannot starve due work. can-core's revocation also disables the administrator
account, and an administrator `revoke-now` covers a person still `offboarding`.

**R10. Secrets and egress are platform gaps.** can-core's installer generates only its own keys into
`can-module` and refuses references to other Secrets; the module namespace policy allows egress only to
DNS, can-core, Traefik and the databases. Both are recorded as gaps rather than worked around with
chart tricks that could not be tested.
