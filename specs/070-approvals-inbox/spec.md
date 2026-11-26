# 070: timesheet, expense, purchase order and time off approvals in one inbox (E11g)

**Depends on**: approvals and notifications (`specs/015`), module wiring (`specs/016`), upgrade
approval (`docs/releasing.md`), reminders (`specs/060`), users and roles (`specs/044`), launch tickets
(`specs/036`). Implements part of `SPEC.md` section 3.7 ("one approval engine").

## Problem

The finance and people manifests declare `timesheet`, `expense`, `purchase-order` and `timeoff`
approvals, but only invoices and jobs requisitions use can-core's engine. The other four were decided by
each module's own route, with no policy, delegation, escalation or common inbox.

## Behaviour

On an installation with can-core (`CAN_API_URL`); standalone modules behave as before.

| Record | Asks the engine | On the decision |
|---|---|---|
| timesheet, expense | on submit | approved or rejected in finance |
| purchase order (a draft) | `POST /v1/purchase-orders/{id}/request-approval` (UI: "Request approval"; `finance.write`) | approved and numbered; a rejection leaves the draft |
| time off | on submit | approved (balance and ledger) or rejected in people |

- **Who decides** is the approval policy (`company.yaml` `approvals:`), else the manifest's default
  chain. finance: `approver|owner` (the `finance:approver` role; the owners when nobody but the
  requester holds it). people: `manager|hr|owner`: the requester's manager when the directory links
  their account, else HR or the owners.
- **Engine changes.** A default chain entry `a|b|c` is a step for `a` with `b`, `c` as fallback. The
  provider of the `directory` capability, and only it, may name the requester's `manager` in
  `POST /v1/approvals`; it stands for `manager_of_requester` in that request. Roles named in a chain
  with `|` count as approval roles (only an owner or a holder assigns them).
- **Module routes honour the engine.** Approve answers 409 until the engine has approved. When the
  decision could not be applied (locked period, ledger, leave balance or calendar) it is kept, and the
  module's approve route applies it for someone with the module's permission and separation of duties.
  Reject (send back) answers 409 while the request is open in the inbox, and works while the record is
  not there (queued, refused). Editing or cancelling a purchase order and cancelling time off withdraw
  the request. A request cancelled or expired in the inbox returns the record to draft.
- **Separation of duties.** The engine refuses the requester. The module also refuses an approval when
  any approver owns, created, edited or submitted the record (it records a rejection), and one that
  names no approver. A module token's subject is sent as `party:<subject>`, which no account matches.
- **Fail closed.** Records wait in the module's `approval_queue`, written with the submission. While
  can-core is unreachable the submission is accepted, nothing can approve the record, and it is sent
  when can-core answers (every 15 s). A refusal (422: nobody but the requester could approve) is shown
  on the record and retried hourly. A purchase order request answers 503 or 422 to the caller.
- **Upgrade.** Records already submitted or pending are queued at the first pass after the upgrade,
  without another submission. Draft purchase orders are not requested for anyone.
- **Batching.** can-core allows a module 120 new requests an hour and 600 a day. The queue sends at
  most 90 an hour and 500 a day, oldest first, and stops on 429 until `Retry-After`; the rest waits.
  finance's invoice requests do not go through the queue and use the remaining room. The counters are
  per process; can-core's limit still holds after a restart.
- **What can-core holds**: record id, requester account, amount and currency, category and date
  (expense), week and hours (timesheet), and `module_path`. No names or free text. For time off: the
  request id only (dates and type reveal absence or health).
- **Inbox.** Requests appear in the admin UI under Approvals. A request page offers "Open <module>"
  (through the Apps launcher, to `module_path`) to viewers who may sign in to that module.
- **Notifications.** The engine tells approvers and the requester. The reminder rules of
  `specs/060-reminders` for `expense.*`, `timesheet.*` and `timeoff.*` are removed; the events stay.

## Manifests and versions

finance 0.4.0 (image 0.4.0), people 0.5.0 (image 0.3.0, subscribes to `approval.decided.v1`). Both
upgrades wait for the operator's approval (new event received; default chains need different people).

## Not in scope (follow-ups)

- **Equity**: grant and exercise approval stay in the equity service (`specs/068-equity-board-role`);
  its `ApprovalEngine` adapter is still not built.
- Review releases (`review-release`), board resolutions, manual journals, voiding.
- Manager steps for finance requesters (finance has no reporting lines), and amount thresholds per
  currency (a policy compares minor units whatever the currency).
- Polling can-core for lost decisions: delivery relies on the signed event and can-core's retries; a
  dead-lettered delivery is requeued in the admin UI.

## Verification

Unit tests: `core/server/approvals` (chain, manager), `core/server/moduleapi`, `core/finance` and
`core/people` (`queued_test.go`, against the fake can-core), `core/server/adminui` (module link). Compose
end to end: `test/e2e/approvals_test.go` and the people scenario. Not verified on a cluster.
