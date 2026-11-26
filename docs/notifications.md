# Notifications (can-core)

A per-person inbox plus optional email and team webhook channels. Status: implemented and tested
offline (epic E11a). Email goes through the `email.send` outbox of [email.md](email.md) (E03d). Spec: `specs/015-approvals-notifications/`.

## What people get

- **Inbox** at `/notifications` (and `GET /api/v1/notifications`): newest first, unread badge in the
  navigation (capped at "99+"), mark one or all read. You can only read and mark your own.
- **Settings**: email address, quiet hours (with a time zone), digest (`off`, `hourly`, `daily`).
  During quiet hours and in digest mode emails are queued and sent as one message; **urgent**
  notifications always go out at once.
- **Preferences** per notification type and channel (`email`, `inapp`); `*` is the default for types
  without their own setting. Urgent notices ignore the in-app mute.
- **Limits**: at most 30 emails per person per hour **per origin** (the rest are logged as suppressed;
  the inbox still gets the notice). The approval engine's notices are counted apart from other notices,
  and per module that opened the approval: a module's other notices do not use up their allowance, nor do
  another module's approvals. A module's own approvals share one allowance, and all mail still shares the
  relay. Job applications and website leads, which anyone on the
  internet can raise, are e-mailed and pushed one by one up to five an hour per person; further ones
  go into one digest e-mail an hour later, without push. Nothing here throttles logins or valid API credentials.

## What triggers them

1. **Approvals**: approvers when a step opens, escalation targets, requesters on the outcome.
2. **Module rules** in `module.yaml`:

   ```yaml
   notifications:
     - { event: lead.created.v1, recipients: ["role:sales", "field:owner", "channel:ops"] }
     - { event: issue.priority_changed.v1, type: work.p0, recipients: ["field:manager"], urgent: true }
   ```

   Recipients: `role:<name>` (a role of the declaring module, or `module:role`; up to 50 unscoped holders), `user:<subject>`, `field:<key>` (a top-level event
   data member holding a subject such as `user:alice`), `channel:<name>` (an outbound webhook channel).
   At most 50 recipients per rule. Only enabled modules' rules apply (cached 30 s).
3. **Modules directly**: `POST /v1/notifications` (module API; the module lists `notify` under
   `requires.capabilities`). The type must start with the module id and a dot (so module ids that name the platform, such as
   `can-core`, `platform` or `approvals`, are refused), `dedupe_key` makes a
   reminder idempotent (keys are per module), `link` must be a site-relative path, `data` holds up to 20 short strings
   (`title` and `body` are shown for module-defined types).

Rules from manifests honour each recipient's in-app mute even when `urgent`, may only use types in
their own `<module-id>.` namespace, and dedupe per module. A recipient receives at most 200 in-app
notices per hour from one origin (a module, or can-core's approvals); the rest are dropped. A rule on an
event its own module publishes only reacts to that module's (or can-core's) event: another module
publishing the same event name does not set it off.

Notifications created from events copy only `id` and `*_id` members of the event data (and `count`, a
number), never names or emails. When a rule names a role and no recipient of the rule is a person (nobody
holds the role), the **owners** are told instead, so a new company is not silent.

## Reminders

What the shipped modules announce (spec: `specs/060-reminders/`). Roles are the module's own; grant them
in the admin UI under access. Each type has its own preference per channel.

| Notification type | Raised when | Told |
|---|---|---|
| `invoice.overdue` | an issued invoice with a balance passed its due date (finance, hourly) | `finance:finance` |
| `contract.expiring` | a contract entered its renewal reminder window | `finance:finance` |
| `contract.signed`, `contract.declined` | all signers signed, or one declined | `finance:finance` |
| `expense.submitted`, `timesheet.submitted` | waiting for a decision in finance | `finance:approver` |
| `expense.decided`, `timesheet.decided` | approved or sent back | whose it is |
| `application.received` | a job application arrived | the job's hiring manager (if an account), `jobs:recruiter` |
| `work.issue.p0` (urgent) | an unresolved issue became P0, or a P0 changed hands | the assignee, `work:manager` |
| `timeoff.requested` | time off waiting for a decision | the manager (if the directory links an account), `people:hr` |
| `timeoff.decided` | approved or refused | who submitted it |
| `entity.filing.due` | a filing reached a reminder point | `entity:secretary` |
| `lead.created` | a website form lead (forms) | `forms:sales` |

"Told" needs an account: a person known only by a free-form id (`person_17`, a service token) cannot be
notified. E-mail follows [email.md](email.md): to an address the person entered and is allowed to receive,
counted against the module's quota, with a fixed wording unless `email.modules.<id>.notification_text` is on.

**Once only, and no flood.** Each scheduled check records what it announced in the module's own database,
in the transaction that queues the event, so a restart repeats nothing. A check that finds more than five
at once (the first run after the upgrade, or after downtime) raises one summary with the number; this
holds for contract renewals too. A person is told about one P0 issue at most once an hour.
Overdue invoices and filings are checked hourly; the invoice check can be run now with
`POST /v1/invoices/overdue-sweep` (finance admin; `as_of` not in the future).

**After a restore.** finance marks the invoices already overdue and the renewals already due as
announced, so nothing is repeated (the restore says how many; the same moment it switches customer
payment reminders off, [finance.md](finance.md)). Entity filing deadlines are **not** held: a reminder
point reached since the dump is announced again, summarised when more than five. A backup taken before
this release behaves like the upgrade.

**For event consumers.** `contract.expiring.v1` and `entity.filing.due.v1` now have two forms (the
schema is a `oneOf`): the per-record one as before, or `{"count": n}` for a backlog.

Time off requests already pending when people is upgraded do not notify their requester of the decision:
the account that submitted them was not recorded.

**Upgrading.** The new manifests publish new events and add notification rules, which widens what the
modules may do: the `finance`, `jobs`, `work`, `people` and `entity` upgrades **wait for your approval**
(`can apply --plan`, then `can apply --approve <module>=<code>`, or the admin UI catalogue; see
[admin-ui.md](admin-ui.md)). Until then the running release stays and nothing new is announced. A first
install and a restore do not ask. Contracts whose renewal reminder was raised before the upgrade are not
raised again.

Not announced: purchase order drafts, review releases, equity grant approvals, board resolutions and
minutes, and imported P0 issues.

## Dedupe

A notice with the same recipient and `dedupe_key` is dropped (the first one stays). Rules use the event
id, so a redelivered event cannot notify twice.

## Channels

- Email: the `Mailer` interface, wired to the platform's `email.send` outbox ([email.md](email.md)): plain text
  only is sent, to vouched addresses, and module-authored content is replaced by a fixed text
  ([email.md](email.md#notification-mail)). A delivery logged as `sent` means "handed to the outbox";
  what the relay did is in Admin UI, Email. Templates are project-owned; values are
  escaped in HTML, stripped of control characters in the subject and text.
- Webhook (Slack-style `{"text": ...}`): named channels in company.yaml, https only, no redirects,
  errors never contain the URL, and the connection is refused at dial time for private, loopback,
  link-local, multicast, carrier-grade NAT (100.64.0.0/10), IETF protocol assignments (192.0.0.0/24), 6to4 (2002::/16 and the 192.88.99.0/24 relay), Teredo (2001::/32), local-use NAT64 (64:ff9b:1::/48), IPv4-compatible (::/96), documentation (including 3fff::/20), benchmark and reserved ranges; IPv4-mapped and NAT64 (64:ff9b::/96) addresses are judged by the embedded IPv4 (the URL is operator-trusted; this limits a typo or a poisoned
  file probing the cluster):

  ```yaml
  notifications:
    webhooks:
      ops: https://hooks.example.com/services/T000/B000/XXXX
  ```

  `can apply` stores them; they are used by `channel:<name>` recipients.
- Web Push (`webpush`, epic E19a; see [pwa.md](pwa.md)): one message per subscribed device of the
  recipient, encrypted (RFC 8291) and VAPID-signed (RFC 8292), through the push service the browser named.
  The payload is a generic title by type, a fixed line and a site path, never the notification's data.
  Per-type preference (default on), quiet hours hold ordinary notices (urgent bypass), digests do not
  apply, the same hourly limit, three attempts with `Retry-After`, 404/410 deletes the device. Urgent
  notices go with `Urgency: high` and a day's TTL, others `normal` and an hour.
- Fakes (`notify.FakeMailer`, `notify.FakeWebhook`, a fake `PushSender`, an `httptest` push service) are
  what the tests use; no test touches a network.

## Delivery log and events

Every channel decision is a row: `sent`, `failed`, `queued` or `suppressed` (preference, rate limit).
Users with `notifications.admin` read it at `GET /api/v1/notification-deliveries`. Every successful
delivery publishes `notification.sent.v1` (`notification_id`, `subject`, `type`, `channel`; no address,
title or body). Notifications are pruned after 180 days.

## Gaps

- Without a configured relay every email send fails visibly (the log shows `failed`, the inbox
  still works). Queuing is local, so sends no longer wait for the relay; the per-recipient hourly limit
  counts messages handed to the outbox.
- Push is Web Push only (no FCM or UnifiedPush native channel; that is the native app's job, if it is
  ever built).
- The delivery log has no UI page, only the API.
