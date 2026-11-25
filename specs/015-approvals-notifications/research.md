# Research and decisions (E11a)

**R1. Subjects as strings, not a users table.** `user:<name>` for admin users, `party:<id>` for people
who do not sign in. RBAC, approvals and notifications key on the string, so the admin user model, login
and throttling stay as they are, and `party:` subjects (employees without admin access, later the
portal) work without a schema change. Cost: usernames must match `[A-Za-z0-9_.@-]{1,64}`; `admin
bootstrap` now enforces it.

**R2. Roles are computed, assignments are stored.** Role definitions come from enabled modules'
manifests plus built-in roles (`owner`, `operator`, `auditor`); only assignments and operator overrides
are stored. A module disabled stops granting. Re-registration is seen within the 30 s cache TTL.

**R3. Break-glass.** `owner` holds `*`, cannot be edited or overridden, cannot be defined by a module
and cannot be left without a holder. `can-core admin roles grant <user> owner` works on the database
directly for recovery.

**R4. One terminal approval event.** `approval.decided.v1` with an `outcome` replaces separate
approved/rejected events (cancelled and expired also need reporting). Targeted to the owning module with
`PublishTo`, because the existing fan-out would show every subscriber the identifiers of every module's
requests.

**R5. Fail closed on staffing.** At creation every step of the first stage is staffed or the request is
refused; a later stage that cannot be staffed rejects the request with reason `no_approver`. A role
counts only if at least `quorum` holders other than the requester exist.

**R6. Distinct approvers.** One person (or their delegator) votes once per request, enforced by a
unique index and a check; this is the separation-of-duties rule that also stops an approver from
approving two steps.

**R7. Serialising mutations.** A mutex in the engine plus pre-transaction resolution; the unique vote
index is the backstop. Avoids deadlocks on the single SQLite connection.

**R8. Notifications are idempotent per (subject, dedupe key).** Rules use `<event id>|<type>`;
approvals use `<request>:<type>:<step>`. Redelivered events cannot notify twice.

**R9. Content safety.** Templates are project-owned; values are 200-character strings with control
characters removed; HTML via `html/template`; subjects are single line. Module-supplied `title`/`body`
render only for module-namespaced types, so a module cannot imitate platform notifications.

**R10. No private data in events.** Event data and rule-created notifications keep only `id` and `*_id`
members. Free text (titles, payloads, comments) stays in the database behind `CanView`.

**R11. Email.** No `email.send` provider exists yet; `Mailer` is the seam, `NoMailer` fails visibly and
`FakeMailer` is for tests. Sends are synchronous with a 10 s timeout; a failure never fails the caller.

**R12. Rate limiting.** Only outbound email and webhook per recipient (30 per hour). In-app rows are
stored for every notice. No credential is throttled.
