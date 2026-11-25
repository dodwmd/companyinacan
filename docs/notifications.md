# Notifications (can-core)

A per-person inbox plus optional email and team webhook channels. Status: implemented and tested
offline (epic E11a). No email provider exists yet, see "Gaps". Spec: `specs/015-approvals-notifications/`.

## What people get

- **Inbox** at `/notifications` (and `GET /api/v1/notifications`): newest first, unread badge in the
  navigation (capped at "99+"), mark one or all read. You can only read and mark your own.
- **Settings**: email address, quiet hours (with a time zone), digest (`off`, `hourly`, `daily`).
  During quiet hours and in digest mode emails are queued and sent as one message; **urgent**
  notifications always go out at once.
- **Preferences** per notification type and channel (`email`, `inapp`); `*` is the default for types
  without their own setting. Urgent notices ignore the in-app mute.
- **Limits**: at most 30 emails per person per hour (the rest are logged as suppressed; the inbox still
  gets every notice). Nothing here throttles logins or valid API credentials.

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
   `requires.capabilities`). The type must start with the module id and a dot, `dedupe_key` makes a
   reminder idempotent (keys are per module), `link` must be a site-relative path, `data` holds up to 20 short strings
   (`title` and `body` are shown for module-defined types).

Rules from manifests honour each recipient's in-app mute even when `urgent`, may only use types in
their own `<module-id>.` namespace, and dedupe per module. A recipient receives at most 200 in-app
notices per hour; the rest are dropped.

Notifications created from events copy only `id` and `*_id` members of the event data, never names or
emails.

## Dedupe

A notice with the same recipient and `dedupe_key` is dropped (the first one stays). Rules use the event
id, so a redelivered event cannot notify twice.

## Channels

- Email: the `Mailer` interface (the platform's `email.send`). Templates are project-owned; values are
  escaped in HTML, stripped of control characters in the subject and text.
- Webhook (Slack-style `{"text": ...}`): named channels in company.yaml, https only, no redirects,
  errors never contain the URL, and the connection is refused at dial time for private, loopback,
  link-local, multicast, carrier-grade NAT (100.64.0.0/10), 6to4, NAT64 (judged by the embedded IPv4), benchmark and reserved ranges (the URL is operator-trusted; this limits a typo or a poisoned
  file probing the cluster):

  ```yaml
  notifications:
    webhooks:
      ops: https://hooks.example.com/services/T000/B000/XXXX
  ```

  `can apply` stores them; they are used by `channel:<name>` recipients.
- Fakes (`notify.FakeMailer`, `notify.FakeWebhook`) are what the tests use; no test touches a network.

## Delivery log and events

Every channel decision is a row: `sent`, `failed`, `queued` or `suppressed` (preference, rate limit).
Users with `notifications.admin` read it at `GET /api/v1/notification-deliveries`. Every successful
delivery publishes `notification.sent.v1` (`notification_id`, `subject`, `type`, `channel`; no address,
title or body). Notifications are pruned after 180 days.

## Gaps

- No `email.send` provider: the wired mailer fails every send visibly (log shows `failed`, the inbox
  still works). Sends run in the request that caused them with a 10 s timeout.
- No push (Android) channel; the interface is the same as the other channels.
- The delivery log has no UI page, only the API.
