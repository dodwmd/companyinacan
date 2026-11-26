# Feature specification: Business overview in the admin UI (E17e, completed in E17f)

**Epic**: E17e (first "reporting" slice of E17). **Depends on**: RBAC (`specs/015`), module wiring and token
introspection (`specs/016`), module single sign-on (`specs/036`), the in-house modules.
**Status**: built and tested offline, against the real services in one process and in the compose stack; no
cluster or browser run.

## Problem

The admin UI's dashboard is technical: modules, event deliveries, audit entries. The owner asked for
"something like Webmin but for business people": one page that says how the business is doing and what needs
attention, without opening every module.

## What changes

1. **Overview page** (`GET /overview`, first item of the navigation, any signed-in person). A "needs attention"
   table, then one table per area, with links into the module UIs through the Apps launcher. It is where
   sign-in lands: `GET /` answers 303 to it (an owner who has not finished the setup wizard still goes to
   `/setup`). The technical dashboard is at `/dashboard` (`platform.view`).

| Area | Figures | Who sees them |
|---|---|---|
| Approvals (can-core's engine) | requests waiting for you | everyone, their own |
| forms | new leads | `leads.read` |
| finance | overdue invoices and receivables, each by currency; contracts awaiting signature; contracts ending or renewing within 60 days | `finance.read` (write, approve and admin carry it; ledger permissions alone do not) |
| work | open P0 and P1 issues, and how many are overdue | `work.read` (or write, admin) |
| people | headcount; off today | the directory and leave-calendar rules, see below |
| jobs | open requisitions; new applications (active, still in the first stage); your interviews awaiting your scorecard | the module's role matrix and scope, see below |
| equity | grants waiting for approval; active grants with vesting due within 30 days | whoever may list grants across holders, see below |
| files | storage used (MiB); files whose retention date ends within 30 days | administrators: everything; others: the spaces they manage |
| entity | filings overdue and due within 30 days | `entity.read` and `entity.write` (the secretary) |
| Backups | the last database dump run | `platform.view` |

2. **A `summary` capability** (`schemas/capabilities/summary.v1.json`), aggregate: each module that owns a figure
   serves `GET /v1/summary` on a service it already has. The answer is a version, and figures of a **key from a
   closed list, a count and optionally amounts by currency**. There is no string a module chooses except a
   three-letter currency code: labels and links are can-core's.
3. **Summary grants**: how can-core asks on behalf of a person (below). No new credential is configured,
   stored or rotated.

## How can-core authenticates to a module on behalf of a user

A **summary grant** is the launch-ticket and introspection pattern applied to one read:

1. The signed-in person loads `/overview`. For each enabled module that offers `summary` **and in which RBAC says
   this person holds at least one of the module's permissions**, can-core mints `cansum_` + 256 random bits,
   kept only as a hash, only in memory, bound to (module, person), valid 15 seconds.
2. can-core calls `GET http://<service>.mod-<id>.svc.cluster.local:<port>/v1/summary` with it as bearer.
3. The module asks can-core `POST /v1/tokens/introspect` over the module API, authenticated by its own projected
   ServiceAccount token, naming itself as audience. can-core answers **once**, only to the module the grant was
   minted for: `kind: "summary"`, the subject, and the permissions the person holds in that module now.
4. The module maps those permissions exactly as it does for personal tokens and sessions, applies its own policy,
   and returns only figures that person may see (possibly none).

Properties: the grant opens nothing but `/v1/summary` (every other route takes `kind: "user"` personal tokens,
which start `canpat_`); it is useless to any other module, a second time, or after 15 seconds; a module that
receives one learns nothing it would not learn from that person's single sign-on. can-core has no token, database
access or service account that reads module data without a person.

Rejected: a static per-module "reporting" token held by can-core (a standing super-credential; the existing
`CAN_LEADS_TOKEN_FILE` of the leads view is not used or extended); personal access tokens minted per person
(long-lived, revocable credentials created behind the person's back); signing assertions with the webhook secret
(a bearer assertion any holder of the secret could forge for any person, and modules would trust claims instead
of asking); events pushed to a can-core read model (can-core would hold a copy of business data and apply
permissions itself).

## Rules

- **Fail closed and visibly.** Each module has 2 seconds (introspection included); a module that is down, slow,
  answering anything but 200, redirecting, too large (16 KiB) or out of bounds is shown as **unavailable**, never
  as zero. Redirects are not followed and no proxy is used. The same holds when can-core cannot read the module's
  record or the person's roles (database or RBAC error): nothing is asked and the area is shown as unavailable
  rather than left out. Such an area can name a module the person turns out to have no right in. Whenever
  anything is unavailable the page does not say "nothing needs your attention"; it says the list may be
  incomplete.
- **Strict parsing.** Unknown keys, and keys owned by another module, are dropped; a negative or huge count, a
  repeated figure, a malformed currency or another version spoils the whole answer.
- **Cache.** 30 seconds per (module, person, sorted permissions); failures 5 seconds. A role added or removed
  changes the key, so it takes effect on the next load; a disabled account has no session. Changes inside a
  module (a new reporting line, a team move) show within 30 seconds. At most 512 entries, in memory (when full of
  unexpired entries it is emptied).
- **Calls.** For one key at most one call is in flight: concurrent page loads wait for it, and it is not cancelled
  when the load that started it is abandoned. At most 4 calls are in flight per module, so a module that hangs
  holds up only itself. A load that never got one of those slots, or got it too late to be answered, shows the
  module as unavailable and caches nothing.
- **A person with no right in a module** causes no call to it and sees no mention of it.
- **Leave stays masked.** people counts with the directory's own rules: headcount is the employed people
  (active, leave, offboarding) the viewer can see; "off today" is the "who is out" calendar for the viewer
  (everyone for hr and admin, otherwise their own team), counting approved leave only, never a type. A viewer
  with no person record who is not hr gets no figure rather than a zero. Whoever could not see that a person is
  off cannot learn it from the count. Counting reads no leave type, so it writes no leave-type audit entry.
- **Jobs.** The same mapping from permissions to one role, and the same scope predicates, as the lists: a hiring
  manager counts their own jobs, an interviewer has no requisition figure and counts only assigned applications.
  Interviews are not scheduled in the service, so "due" means: assigned to you, application still active, your
  scorecard not submitted. Counting reads no candidate field and is not audited (like the funnel counts).
- **Equity.** The same mapping from permissions to roles as personal access tokens, so a person is never the
  board this way: **someone holding only the board role sees no equity figures** (pending can-core approvals are
  still counted under Approvals once equity's board step runs there). A holder's own vesting is not reported. The
  read is audited.
- **Files.** Retention dates are shown to space managers and administrators, so only they get figures; a person who
  manages no space gets none rather than a zero. Storage counts every version not yet purged, trash included,
  rounded up to whole MiB. Trash purge dates are not counted, only retention dates. The read is audited.
- **Entity.** Officers and directors hold the same can-core permissions while only officers may see every
  entity's registers, so neither gets filing counts; only the secretary does. `can-board` accepts summary grants
  and nothing else from can-core; its API is unchanged. The counts are audited as a register read.
- **Backups.** can-core only knows the database dump run (`LAST-RUN`). The off-site copy is made on the server;
  the page says so.
- **Accessibility.** Tables with captions and row headers, one `h1`, "needs attention" in words (not colour
  only), buttons that name what they open, no script; phone layout as elsewhere.

## Threat model

| Threat | Control | Residual |
|---|---|---|
| A signed-in person sees figures above their rights (finance numbers for a non-finance user) | RBAC standing before any call; the module computes permissions from introspection and applies its own policy; cache keyed by person and permissions | A module's own policy bug; aggregates of small sets can identify a record (one overdue invoice is that invoice's amount, "1 off today" in a team of two) — only to people who may already read the records |
| Cached answer served to the wrong person or after revocation | Key contains subject and permissions; lookups of rights happen on every load | Up to 30 s for changes inside a module |
| A compromised module obtains data or access through the grant | Audience-bound and single use; the module learns only subject and its own permissions | A hostile module can show false counts for **its own** keys; it cannot add text, links or another module's figures |
| Grant observed on the cluster network (plain HTTP, as webhooks) or in a log | Never logged; redeemable only by the audience module's identity, once, within 15 s | An attacker who already is the audience module gains nothing new |
| Anyone who reaches `/v1/summary` (API exposed with `expose.api`) sends junk | Shape check before any call; made-up grants are inactive; failures count against the module's limiter on failed authentication (429 over it); an address over the limit has one request introspected at a time, of the module's 4; a grant is checked before anything is decided, so a valid one is never refused for earlier failures; can-core slows repeated failures | A flood from many addresses can still make that module's overview figures "unavailable". Behind a proxy without `trust_proxy` all outside callers share the proxy's address (can-core's calls are direct and keep their own) |
| can-core is compromised | None added: it already is the identity authority (sessions, tickets, introspection) | It can ask any module for aggregates in anyone's name, and could already sign in as them |
| SSRF through a module-chosen address | Addresses come from the registry's validated in-cluster names | none known |
| Slow modules stall can-core | 2 s per module, concurrent, at most 4 calls at once to one module, one call in flight per person and module, bounded cache and grant table (1024) | Reloading calls a module at most once per 30 s (5 s after a failure) for one person and set of permissions, **except** while its slots are full (nothing is cached for a call that only waited, so each such load queues again) and after the cache was emptied |
| Personal data in transit or at rest in can-core | The contract carries counts and sums only; nothing is persisted | Totals are business-sensitive: pages are `no-store` |

Views are not audited one by one in can-core (they would flood the log); failures are logged without the grant.

## Not covered

- The wrapped apps report nothing.
- "Approvals waiting" is can-core's engine only: leave, timesheet and expense approvals that a module decides
  itself are not counted.
- No history, charts, export or search (rest of E17).
- Installed modules offer the capability only after they are upgraded to a manifest that declares it; until then
  their area is simply absent.
- equity has values but no chart templates on this branch, so its figures have been tested at module level and in
  the compose stack only.
- entity is experimental and not deployable by can-core yet (its charts are being added elsewhere), so its
  figures have only been tested at module level.
