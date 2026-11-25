# Approvals and access (can-core)

One engine for "request, approve, record". Modules ask; the company decides who approves; can-core
records every step. Status: implemented and tested offline (epic E11a); not yet verified on a cluster.
Spec: `specs/015-approvals-notifications/`.

## For module authors

Declare what you use in `module.yaml`:

```yaml
approvals:
  - { type: expense, default_chain: [manager, finance] }   # used only when the company set no policy
permissions: [expenses.read, expenses.approve]
roles:
  finance: [expenses.read, expenses.approve]                # default grants; the operator can override
subscribes:
  - { event: approval.decided.v1, service: expenses, port: 8080, webhook: /hooks/approval }
```

Create a request (module API, bearer token as for every call):

```
POST /v1/approvals            Idempotency-Key: <your key>
{"type":"expense","requester":"user:alice","ref":"exp-42","title":"Laptop","amount":120000,"payload":{...}}
```

- `201` new, `200` same key and body again, `409` same key with a different body, `403` type not
  declared, `422` no policy or no approver can be found (the request is refused, not left open).
- `requester` is `user:<admin username>` or `party:<id>`. `amount` is in minor units.
- The result arrives as a signed `approval.decided.v1` webhook (outcome `approved`, `rejected`,
  `cancelled` or `expired`), to your module only. Deduplicate on the event id; confirm with
  `GET /v1/approvals/{id}`. `POST /v1/approvals/{id}/cancel` withdraws a pending request.
- **Verify the envelope, not just the signature.** Check the HMAC headers as for every webhook, then
  check `source == "can-core"` and `type == "approval.decided.v1"` before acting. Modules cannot
  publish `approval.*` or `notification.*` events (the module API refuses them and manifests that
  declare them are rejected), but receivers should still not act on an event whose source is not
  can-core, and should match `request_id` to a request they created.
- Events carry identifiers only. Titles and payloads stay in can-core and are shown to approvers.
- Permission checks: `POST /v1/check {"subject":"user:bob","permission":"expenses.approve","resource":""}`
  answers `{"allowed":true|false}`. You may ask about permissions your manifest declares. Errors answer
  503, never "allowed". Role changes apply at once, also when made by the CLI or `can apply` in another
  process (the cache is dropped when the database changes), except changes of a module manifest that
  bypass the registry, which are seen within 30 seconds.

## Names modules may not use

Manifests (`can validate` and the registry both enforce it) may not declare permissions starting with
`platform.`, `rbac.`, `approvals.`, `notifications.` or `*`; may not define the roles `owner`, `operator`
or `auditor` (module roles never merge into built-in ones); may grant in a role only permissions the
same manifest declares (and so only ones it owns, see below); may not publish `approval.*` or `notification.*` events; and may set a
notification rule `type` only inside their own `<module-id>.` namespace. An approval type can be
declared by one enabled module only (policies are keyed by type), so a second module declaring
`expense` is refused at registration. The same holds for **permissions**: a permission belongs to the
first enabled module that declares it, and a second enabled module declaring it is refused. A module
that is disabled gives its permissions and approval types up; enabling it again is refused while
another enabled module holds one (ownership is never inherited, nor taken back, silently), and `can
apply` checks this before installing anything. Which module "wins" therefore depends on enable order:
an operator who enables a module first holds the names, so enable the modules you trust first.
**Roles are namespaced by module** (`work:viewer`, `forms:viewer` are different roles); built-in roles
stay `owner`, `operator`, `auditor`. A policy's bare role name means the role of the module the request
belongs to (`finance` for an `expenses` request is `expenses:finance`); write `hr:manager` for another
module's role. A permission declared by two enabled modules (only possible if rows bypassed the
registry) grants nothing through any module role. Permission names are not forced to start with the module id,
because the existing manifests use capability-style names (`leads.read`); the reserved list is what
keeps platform permissions out of reach.

## Policies (company.yaml)

```yaml
approvals:
  expense:
    steps:
      - name: manager
        approvers: [{ manager_of_requester: true }]
        fallback: [{ role: finance }]       # used when the requester has no manager
      - name: finance
        approvers: [{ role: finance }]
        quorum: 2                            # two distinct finance approvers
        when: { amount_gte: 100000 }         # skipped below the threshold
      - name: legal
        parallel: true                       # opens together with the previous step
        approvers: [{ user: "user:lena" }]
    expires_after: 7d                        # outcome `expired`
    escalate_after: 48h                      # once, adds these approvers to the open steps
    escalate_to: [{ role: director }]
```

`can apply` validates and stores the policies. Requests already in flight keep the policy they started
under. Rules that cannot be configured away:

1. A requester can never approve or reject their own request, nor through a delegation.
2. A person counts once per request: the same person cannot approve two steps or twice in a quorum.
3. If a step has fewer than `quorum` possible approvers other than the requester (and no usable
   fallback), the request is refused at creation; if a later step cannot be staffed, the request is
   rejected with reason `no_approver`.
4. Any single reject ends the request. A request whose `expires_at` has passed cannot be decided, even
   in the minute before the timer runs.
5. A person who is eligible for two parallel steps (for example the same role in both) casts one vote
   in total, on the first open step they match; the other step needs someone else.
6. Manager lookups use the `directory` capability (`GET /v1/people/{id}`, `manager_id`) for `party:`
   requesters, with the credential in `CAN_DIRECTORY_TOKEN_FILE`. Without a directory provider or a
   credential, manager steps use their fallback or refuse the request. A directory outage is an error,
   not "no manager".

## Approvers (admin UI)

- `/approvals`: **Waiting on me**, **My requests**, all requests (for `approvals.admin`), and out of
  office delegation (the person you name may decide what waits on you, up to 90 days).
- `/approvals/{id}`: details from the module, steps, decision history; approve or reject with a
  comment; comment; withdraw (requester or approvals admin). Viewing is audited. People who may not see
  a request get a 404.
- Timers run every minute in can-core (`Tick`).

## Access (roles)

- Subjects: `user:<admin username>` and `party:<id>`. A new admin user has **no role**: they can use
  their inbox, notifications and settings, nothing operational (modules, audit, events, leads need
  `platform.view`, `platform.audit`, `platform.operate`).
- Role membership for approvals (who may approve a `role:` step, who is notified for a `role:`
  recipient) counts **unscoped** assignments only. A scoped assignment (`dept/sales/*`) answers
  `can.check` for resources under the scope and nothing else.
- Only an owner may assign or remove `owner` or edit a role's grants (overrides); nobody can assign a
  role to themselves; the last owner cannot be removed, also under concurrent requests. The CLI actor
  (`can-core admin roles ...`, run inside the pod; audit actor `can-core:cli`, which no admin username can
  equal) is the exception, as the recovery path. An admin who happens to be named `cli` has no special
  rights.
- Built-in roles: `owner` (everything, break-glass, cannot be edited, never left empty), `operator`,
  `auditor`; module manifests add theirs. `/access` (needs `rbac.manage`) assigns roles, optionally
  scoped to a resource prefix such as `dept/sales/*`. Scopes and the `resource` argument of
  `can.check` are compared as exact strings: modules must canonicalise them (one spelling per
  resource, no trailing slashes or case variants) before asking or storing. Overrides of a role's grants (owner only, existing roles only): `POST
  /api/v1/roles/{role}/overrides` (API only for now).
- Recovery without the UI: `can-core admin roles grant <user> owner --db <path>` (also `revoke`,
  `list`), audited as actor `cli`. The first admin created, and every admin that existed before this
  version, is an owner.
- With single sign-on ([`sso.md`](sso.md)) the unscoped roles of SSO users follow the IdP groups through
  `sso.group_roles`: assigned and removed at sign-in with audit actor `sso` (`rbac.sso_sync`, with before and
  after). The sync only changes assignments it made, never assigns `owner` without `confirm_owner_mapping`, and
  never removes the last owner. Approval steps and notifications see these roles like any other.
- Create more admin users with `can-core admin bootstrap --username <name>` (names: letters, digits,
  `_ . @ -`). An admin created by an older version with other characters gets no role from the
  upgrade; create a new admin with a valid name and grant it `owner` from the CLI.

## Measured footprint

can-core built offline (`CGO_ENABLED=0`, linux/amd64), started with an empty database, resident set
after 5 s idle, and after 600 requests to `/healthz` and `/login` (two runs each):

| | idle RSS | after 600 requests |
|---|---|---|
| main before E11a | 38.3 MiB | 43.3 MiB |
| with E11a | 40.4 MiB | 46.2 MiB |

About +2.1 MiB idle and +2.9 MiB after load, against the 48 MiB idle target. The binary grew by 0.9 MiB
(16.4 to 17.4 MB). Not measured: large approval or notification backlogs, and a cluster.
Memory is bounded by design (cache of 2048 subjects, list limits, 100-item timer batches), not by
measurement under a real backlog.
