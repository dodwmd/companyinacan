# Provisioning: joiners, movers, leavers and SCIM

The in-house `provisioning` module (`cmd/can-provision`, library `core/provision`) provides the
`identity.provision` capability. It has two halves that share one SQLite file:

1. a **SCIM 2.0 server** over the people directory, so external applications and identity providers
   can read users and manage access groups;
2. a **provisioning engine**: joiner, mover and leaver workflows made of ordered, idempotent steps
   against *connectors* (downstream systems), with retries, dead-lettering, compensation, approvals for
   privileged access, an audit trail and a can-core kill switch for the tokens and sessions of people
   who left.

**Status: experimental.** Everything below was built and tested offline against fakes. This is a tool,
not legal, security or HR advice: who must lose access when, and what must be retained, is the
operator's responsibility. [Unverified](#what-is-unverified-and-known-gaps) lists exactly what has not
been proven. Spec: [`specs/028-identity-provision/`](../specs/028-identity-provision/).

## The flows

Every workflow starts from an event or an API call, **re-reads the person from the directory** and
refuses to run when the directory status does not fit. An event is a hint, never an authority: a
forged `person.offboarded.v1` for an active person starts nothing.

| Trigger | Workflow | What it does |
|---|---|---|
| `person.created.v1` with status `onboarding` or `active`; `person.status_changed.v1` to `onboarding` or `active`; a completed checklist task with action `identity.provision` | **joiner** | per connector: create the account (adopting one that exists), add default groups and the person's SCIM groups; then create the manager and IT checklist from the people template |
| `POST /v1/provision/runs` (`mover`), or a SCIM group membership change for a person who has accounts | **mover** | per connector with an account: update the account, add the groups gained, remove the groups lost (only groups this module added) |
| `person.status_changed.v1` to `offboarding`; to `former`; `person.offboarded.v1`; a completed task with action `identity.deprovision` or `identity.disable` | **leaver** | per connector: remove group memberships, disable the account; optionally delete it N days later; revoke the person's can-core tokens and sessions; create the offboarding checklist |

`candidate.hired.v1` is deliberately **not** subscribed. It carries a candidate id, not a person id; the
hire hand-off makes the people module create the person, and `person.created.v1` is what starts the
joiner. Subscribing to both would start two joiners.

### Effective dating

A leaver's steps carry a `not_before`: the **start of the last day** (`effective_date`, in the
configured time zone). Nothing is disabled before it. For a person in `offboarding` with an `end_date`
on record the run waits (state `scheduled`); with no end date it waits indefinitely for the
`person.offboarded.v1` event, which is emitted when people applies the change to `former`. If
offboarding is called off (status back to `active`) a run in which nothing happened yet is cancelled.

**Urgent terminations**: `POST /v1/provision/people/{id}/urgent` (`provision.admin`, never for
yourself, only for someone who is offboarding or former) releases every access-removal step at once and
lifts the approval gate for access removal (not for deletion). The flag is audited.

### Steps, retries, dead letters, compensation

* Every step is persisted before and after it runs; a step found `running` after a crash is retried
  with the **same idempotency key** (`<run id>:<step number>`) so a connector can de-duplicate.
* Transient failures (network, 5xx, 408, 429) back off exponentially (`base_delay` doubling up to
  `max_delay`) and stop after `max_attempts`; permanent failures (4xx, unimplemented, hostile
  responses) stop at once. A stopped step is **dead-lettered**: `provision.failed.v1` is published and
  the run appears in `GET /v1/provision/dead-letters` until an administrator retries it.
* A failed **joiner or mover** is compensated: completed steps are undone newest first (accounts are
  disabled, not deleted, unless `compensate_with_delete`). A failed **leaver** is never rolled back
  (access is not handed back automatically) and one dead step does not stop the others: the remaining
  accounts are still disabled and the run ends `failed`.
* The task step is optional: a checklist that cannot be created never fails a run.

### Approvals and separation of duties

A step is **sensitive** when it grants a privileged group (`workflows.privileged_groups`), creates an
account on a `privileged: true` connector, deletes an account, or disables the accounts of a person who
belongs to a privileged group or a privileged connector. A sensitive step opens a can-core approval
(`privileged-access`, or `privileged-leaver`) whose requester is the person the run is about, so
can-core's separation of duties keeps them from approving their own access or removal. The module then
checks again on the signed `approval.decided.v1`: it rejects the decision if any approver is the
person, their linked account, or whoever started the run. Without an approvals engine a sensitive step
is rejected (fail closed). A rejected step fails a joiner or mover; for a leaver the other accounts are
still disabled.

The module never completes a checklist task and never assigns one to the person it concerns; the
checklist templates carry the assignees (the people module refuses an `identity.*` action assigned to
the person themself).

### The can-core kill switch

The last leaver step asks can-core to end the person's personal access tokens and sessions:
`POST /v1/identity/revoke {party_id, subject}` on the module API, authenticated by the module's
projected token. can-core allows it only when

* the caller provides `identity.provision`,
* the **people module** (the authenticated source of the event, never a request field) published
  `person.offboarded.v1` for that `party_id` and its `effective_date` has been reached,
* `subject` is the account the directory linked to that person: the event carries `subject_ref`, the
  first 32 hex characters of the SHA-256 of the person's `sso_subject`, so the link is checked without
  the subject or anything personal being in an event.

It is idempotent, audited in the same transaction as the change (party id and counts), and rate limited
to 30 calls per module and minute. It cannot revoke anyone else. The person's `sso_subject` must be
the can-core subject (`user:<name>`, for SSO users `user:sso-...`) for the revocation to apply; a
person with any other `sso_subject` is skipped.

## Connectors

| `type` | What it is |
|---|---|
| `scim-client` | pushes to a downstream SCIM 2.0 server: looks the user up by `userName` before creating (so a retry adopts), treats "already gone" as success for disable and delete, never creates a missing group |
| `webhook` | one signed JSON POST per operation: headers `Idempotency-Key`, `X-Provision-Timestamp`, `X-Provision-Signature: v1=HMAC-SHA256(secret, "<ts>." + body)`; it may answer `{"external_id": "..."}` |
| `google-workspace`, `github`, `slack` | **stubs**: no network, every operation fails permanently with `unimplemented`. They exist so the interface and the dead-letter behaviour are real; a workflow that names one fails loudly instead of pretending access was changed |

All outbound calls go through one HTTP client that refuses non-public destinations at connect time
(so DNS rebinding does not help; `core/server/netguard`), does not follow redirects, ignores proxy
environment variables, bounds the response to 1 MiB and every call to a deadline. Configured URLs must
be `https` without credentials, query or fragment and, unless `allow_private` is set for an in-cluster
target you chose on purpose, must not be a literal private address. Error text never contains a
downstream response: only a short machine code is stored.

### Configuration (`provisioning.yaml`)

```yaml
directory: { url: "http://people-api.mod-people.svc.cluster.local:8081", allow_insecure_http: true }
tasks: { joiner_template: tpl_..., leaver_template: tpl_... }   # people checklist templates
workflows:
  privileged_groups: [admins]
  dry_run: false            # the chart ships true: record plans, call nothing
  delete_after_days: 0
  timezone: Australia/Sydney
retry: { max_attempts: 5, base_delay: 30s, max_delay: 1h, step_timeout: 30s }
scim: { max_scan: 5000, require_if_match: false }
connectors:
  - { name: wiki, type: scim-client, url: "https://wiki.example.com/scim/v2", token_env: CAN_PROVISIONING_CONN_WIKI_TOKEN, default_groups: [all-staff] }
  - { name: hooks, type: webhook, url: "https://hooks.example.com/provision", secret_env: CAN_PROVISIONING_CONN_HOOKS_SECRET, privileged: true }
  - { name: gws, type: google-workspace }   # stub
```

Secrets come from the environment only: `CAN_PROVISIONING_API_TOKEN` and `CAN_WEBHOOK_SECRET`
(generated by can-core), `CAN_PROVISIONING_DIRECTORY_TOKEN` (a can-core personal access token for the
people module with `people.read` and `people.write`: the directory API only returns `sso_subject` to the
`hr` role), and one variable per connector. Nothing logs a token or a secret.

**Dry run** (`workflows.dry_run: true`, or `dry_run: true` on a run) records the plan and marks every
step skipped without calling a connector. `POST /v1/provision/plans` shows the plan of a hypothetical run
without storing anything. Start with dry run on, read the plans, then turn it off.

## The SCIM server

Base path `/scim/v2` (public through the `scim` subdomain). It implements `/ServiceProviderConfig`,
`/Schemas`, `/ResourceTypes`, `/Users` and `/Groups` for the subset documented in
[`contracts/openapi.yaml`](../specs/028-identity-provision/contracts/openapi.yaml):

* **Auth**: `Authorization: Bearer scim_...`. Tokens are created by `provision.admin`
  (`POST /v1/provision/scim-tokens`), shown once, stored as a SHA-256, carry `scim.read` (GET) and/or
  `scim.write` (everything else, implies read), expire (1 hour to 1 year) and can be revoked. A valid
  token is never throttled; invalid ones are slowed after 300 per minute.
* **Format**: requests with a body must be `application/scim+json` (else 415); responses are
  `application/scim+json`; errors follow RFC 7644 (`status` as a string, `scimType`).
* **Filter**: `attr eq "value"` terms joined by `and`, attribute names from an allow-list, at most 512
  characters and 8 terms, parsed in one linear pass and matched in memory (nothing is built into a
  query). Everything else is `400 invalidFilter`.
* **Pagination**: `startIndex`, `count` (at most 100). A listing reads at most `scim.max_scan` people
  (`400 tooMany` beyond that unless the filter is `id eq`) and at most four listings run at once.
* **ETag / If-Match**: every user and group has a weak ETag; `If-Match` is honoured (`412`), and
  required with `scim.require_if_match` (`428`).
* **Users are owned by the directory.** They cannot be created, replaced or deleted through SCIM
  (`403 mutability`). `PATCH` accepts `add`, `replace` and `remove` on `displayName` (the preferred
  name) and the work phone only; `active`, `userName`, `name`, `emails` and the rest answer
  `400 mutability`. Writes are audited with ids only.
* **What is exposed**: id, userName (work email, else the id), displayName, name, work email, work
  phone, `userType`, `active` and the manager's id. **Never**: date of birth, national id, address,
  personal phone, compensation, `sso_subject`, legal-hold or erasure flags. `active` is true for
  `onboarding`, `active`, `leave` and `offboarding`: **leave is masked** because it can reveal health or
  family circumstances. Candidates are not shown.
* **Groups** are this module's access groups (up to 500, up to 1000 members each; members must be
  visible directory users). A membership change starts a mover run for the people affected who already
  have a downstream account.

### Setting up an identity provider (illustrative, not tested against any real one)

These are the usual fields; none of them has been tried against a real Okta, Entra ID or Google
Workspace tenant.

* **Okta**: add a SCIM 2.0 app with the *base URL* `https://scim.<domain>/scim/v2`, *unique identifier
  field* `userName`, authentication *HTTP Header* with `Bearer scim_...`, push groups on. Okta sends
  `application/scim+json` and `filter=userName eq "..."`. Okta wants to create users: this server answers
  `403`, so use it as a *read* source or push only groups; users come from the people directory.
* **Microsoft Entra ID**: *Enterprise application, Provisioning, Automatic*, tenant URL
  `https://scim.<domain>/scim/v2`, secret token `scim_...`. Entra uses `PATCH` with the `Operations`
  envelope and `filter=externalId eq` for matching; `externalId` is not stored here, so matching on
  `userName` is the supported path. Entra's required `active` replace is refused as `mutability`.
* **Google Workspace**: has no outbound SCIM client for arbitrary targets; use a connector or the
  `google-workspace` stub's future implementation. Nothing is promised.

## API summary

All under `/v1/provision`, bearer token with `provision.read` (list, plan), `provision.write` (start a
run, retry) or `provision.admin` (cancel, urgent, link account, SCIM tokens, audit). Full contract:
[`openapi.yaml`](../specs/028-identity-provision/contracts/openapi.yaml). Errors are RFC 9457 problems
that never include a downstream response, a token or personal data.

## Security model

* **Fail closed.** No directory answer, no approvals engine, an unreadable token, a sensitive step
  without a second person: nothing proceeds.
* **Bounded work.** Plans (200 steps), PATCH operations (20), filters (512 characters, 8 terms), bodies
  (64 KiB), scans (`max_scan`, 4 at once), steps per worker pass (25), connector calls (deadline), group
  members and groups, SCIM tokens (20), responses (1 MiB).
* **Audit before return.** A state change and its hash-chained audit entry commit in one transaction;
  `can-provision verify-audit` recomputes the chain. Entries hold the actor, the action, ids and counts:
  no names, emails, group names, tokens or downstream responses.
* **No personal data in events.** `provision.step.completed.v1` and `provision.failed.v1` carry run,
  person and connector ids and machine values only.
* **Idempotency everywhere.** Triggers (event ids, `Idempotency-Key`), runs (one live run per person
  and kind), steps (keys passed to connectors), outbox events (the can-core `source_id`).
* **Separation of duties.** See approvals above; nobody flags their own termination.
* **No credentials in the database.** Connector secrets are environment only; the SCIM tokens are
  stored hashed.

## What is unverified and known gaps

* **No connector has talked to a real service.** `scim-client` and `webhook` are tested against
  `httptest` fakes only; their `Verified()` is false. `google-workspace`, `github` and `slack` are
  stubs: unverified and unimplemented (each would need an OAuth client or app, the admin API scopes, and
  its own suspend, group and delete calls).
* **The SCIM server has not been used by any real identity provider.** The field lists above are
  illustrative. RFC 7644 features not implemented: `PUT`, bulk, sort, `/.search`, `or`/`not`/`co`
  filters, `attributes`/`excludedAttributes`, user creation and deletion.
* **Cluster networking.** The default module-namespace `NetworkPolicy` (`deploy/templates/module-namespace.yaml`)
  allows a module to reach DNS, can-core, Traefik and the databases only. This module also needs
  egress to the people module's API and to its connectors' hosts; until the platform grows a supported
  way to allow that, the module **cannot do its job on a cluster**. It has never run on one.
* **Secrets for the directory token and the connectors.** can-core generates only its own keys into
  the module's `can-module` Secret, and the installer refuses references to any other Secret. There is
  no supported way yet to provide `CAN_PROVISIONING_DIRECTORY_TOKEN` or connector credentials.
* **The kill switch needs `sso_subject` to be the can-core subject** (`user:...`). For urgent
  terminations the can-core revocation still waits for the people module to report the person
  `former` (can-core refuses before that), so it retries and, if the status is never set, dead-letters.
* **Mover triggers.** The people module has no "department changed" event; movers start from SCIM
  group changes or the API, not from directory edits.
* **No UI.** Operators use the API; there is no portal or admin UI page.
* **Restart behaviour** (state in SQLite, `running` steps retried with the same key) is tested with a
  database file reopened in-process, not with a killed process.
* Memory: 14.0 to 14.2 MiB RSS idle and 18.7 to 19.0 MiB after 600 requests, static linux/amd64
  build, measured locally (target 20 MiB); not measured with live connectors or a large directory.

Not legal, security or HR advice.
