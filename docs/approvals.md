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
- Events carry identifiers only. Titles and payloads stay in can-core and are shown to approvers.
- Permission checks: `POST /v1/check {"subject":"user:bob","permission":"expenses.approve","resource":""}`
  answers `{"allowed":true|false}`. You may ask about permissions your manifest declares. Errors answer
  503, never "allowed". Answers can be up to 30 seconds old if a role changed outside can-core's own
  UI/API (changes made there apply at once).

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
4. Any single reject ends the request.
5. Manager lookups use the `directory` capability (`GET /v1/people/{id}`, `manager_id`) for `party:`
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
- Built-in roles: `owner` (everything, break-glass, cannot be edited, never left empty), `operator`,
  `auditor`; module manifests add theirs. `/access` (needs `rbac.manage`) assigns roles, optionally
  scoped to a resource prefix such as `dept/sales/*`. Overrides of a role's grants: `POST
  /api/v1/roles/{role}/overrides` (API only for now).
- Recovery without the UI: `can-core admin roles grant <user> owner --db <path>` (also `revoke`,
  `list`), audited as actor `cli`. The first admin created, and every admin that existed before this
  version, is an owner.
- Create more admin users with `can-core admin bootstrap --username <name>` (names: letters, digits,
  `_ . @ -`).

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
