# Feature Specification: Outbound email (`email.send`, Epic E03d)

**Branch**: `epic/E03d-email-send` | **Date**: 2026-11-25 | **Status**: built offline, experimental

SPEC.md references: `email.send` as a can-core platform capability (3.1, 4.3), forms lead
notification (6.1, E05), notifications by email (3.7), the email policy (5: relay, never a
self-hosted mail server, 587/443), recipient and secret handling (3.1). Refines the part of E03
that was left pending (`specs/005-can-core-mvp/tasks.md` T032). Operator guide: `docs/email.md`.

## What existed before

- `core/forms` had a `Mailer` and an `HTTPMailer` posting to a configured URL; nothing served it.
- `core/server/notify` had a `Mailer` seam wired to `NoMailer` (every send failed).
- `company.yaml` had `email.relay {host, port, secret_ref}` that nothing read.
- The setup wizard wrote the Secret `can-system/can-smtp-relay` that nothing read.
- The base NetworkPolicy gave can-core no SMTP egress.

## User scenarios

1. **A lead reaches someone.** A visitor posts the contact form; the address in the form's
   `notify_email` receives a plain-text email with the submission and the visitor as Reply-To.
2. **An approver is told.** A person whose notification address is at the company domain (or is
   otherwise vouched for) gets approval and notification mail.
3. **The relay is down.** Mail waits, is retried with growing pauses, and appears as failed after the
   attempts run out; an operator sends it again once the relay is back.
4. **A module is compromised.** It can mail only its allow-listed recipients, within its quota, and
   cannot set the sender or add headers. The operator turns it off in `company.yaml`.
5. **The operator connects a relay.** `company.yaml` names host and sender; `make up
   SMTP_CREDENTIALS=file` stores the credentials in a Secret.

## Functional requirements

- **FR-001** can-core MUST serve `POST /v1/email` and `GET /v1/email/{id}` on the module API, only to
  enabled modules declaring `email.send`.
- **FR-002** TLS (STARTTLS or implicit, certificate verified, 1.2+) MUST be required by default, with
  no fallback. Plaintext MUST need an environment switch on can-core, and credentials MUST never be
  sent without TLS.
- **FR-003** Credentials MUST come only from a mounted Secret, MUST be bound to the host stored with
  them, and MUST NOT appear in logs, audit records, errors, the database, the UI or process
  arguments.
- **FR-004** Connections to non-public addresses MUST be refused at dial time unless the operator
  allows private ones; link-local, multicast and unspecified addresses are refused always.
- **FR-005** Every recipient of module mail MUST match the module's allow-list, checked when queued
  and again when sent. Default: the company domain and the module's `notify_email` setting. A public
  form MUST NOT be able to choose a recipient.
- **FR-005a** Notification mail caused by a module MUST obey that module's switch and quota, MUST
  carry a fixed text instead of module-authored content unless the operator opted in, and MUST only
  go to the company domain, `email.notification_recipients` or the person's verified single sign-on
  address (verified when it was stored, with single sign-on still on and requiring verification).
  Module-chosen notification types MUST NOT be linkable in mail.
- **FR-006** Per-module hourly and daily quotas and a waiting-message cap MUST be enforced in the
  transaction that queues.
- **FR-007** No input may add or end a header: addresses are bare ASCII addr-specs without `%` or
  `!`; subjects with any control character are refused; bodies are quoted-printable plain text.
- **FR-008** Queued mail MUST be durable, retried with backoff, dead-lettered, and requeueable.
  `source_id` MUST make a request idempotent.
- **FR-009** Every state change MUST be audited in the same transaction; queuing is audited before
  the caller is answered; showing the delivery list is audited before it is shown. Audit, logs,
  errors and events carry no address, subject or body.
- **FR-010** Content MUST be blanked when sent (7 days after dead) and rows deleted after 30 days.
- **FR-011** A message another module sent and a missing id MUST be the same 404.
- **FR-012** The admin UI MUST show relay state and per-message delivery status with masked
  recipients.
- **FR-013** Forms lead notifications and the notification email channel MUST use this path.
- **FR-015** A lead notification MUST be stored with the lead and retried (honouring `Retry-After`)
  until accepted or seven days have passed, and a failure MUST be visible to the operator.
- **FR-016** A relay-level failure MUST pause delivery instead of spending every message's attempts
  (one message left without an answer is not one; three different ones in a row are);
  senders MUST take turns; a shutdown after the relay accepted a message MUST NOT resend it.
- **FR-014** The compose stack MUST include an in-repository SMTP sink, and an end-to-end test MUST
  show that a form submission produces an email.

Quotas limit mail volume only. Nothing here throttles authentication or other requests of a valid
credential.

## Out of scope (not built)

Real-relay verification; the `can-smtp` Secret for SMTP-speaking apps (FreeScout, Listmonk);
`make email` and DNS records (SPF, DKIM, DMARC); inbound mail; HTML and attachments; `AUTH LOGIN` or
XOAUTH2; confirmation links for outside notification addresses; an admin JSON API for the delivery list; erasure reaching
mail still queued.

## Success criteria

- SC-1 `go test -race` over `core/server/email`, `moduleapi`, `adminui`, `forms`, `test/wiring` and
  the contract tests passes; no test contacts a real relay.
- SC-2 `test/e2e/email_test.go` passes against the compose stack.
