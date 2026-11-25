# Feature Specification: Forms Gateway and `leads` Capability (Epic E05)

**Feature Branch**: `003-forms-gateway`

**Created**: 2026-11-25

**Status**: Draft

**Input**: User description: "Epic E05: working lead capture. In-house Go service as a module; same-origin `/_forms` route with prefix stripping; honeypot, timing check, optional Turnstile/hCaptcha; SQLite storage; `leads` capability; email notification via `email.send`; direct webhook until the event bus exists; `lead.created.v1` event schema defined."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A visitor submits a form and the owner gets the lead (Priority: P1)

A visitor fills in the contact form on the static site and submits it. The submission is
validated, stored, and the owner is told about it by email. The visitor lands on a thank-you
page (or receives a JSON acknowledgement when the site submits with `fetch`).

**Why this priority**: This is the product's first working business function (SPEC.md section 2,
goal 1: "working contact forms").

**Independent Test**: Post a valid urlencoded and a valid JSON submission to
`/_forms/contact` (with and without the prefix) against the in-process handler; assert the lead
is stored, the visitor gets a 303 or 201, and the fake mailer received one message.

**Acceptance Scenarios**:

1. **Given** a configured form, **When** a valid submission arrives urlencoded or as JSON, **Then**
   it is stored with the form's retention date and the visitor gets a redirect (HTML) or `201`
   (JSON).
2. **Given** the request path is `/_forms/contact` or, after Traefik strips the prefix,
   `/contact`, **Then** both route to the same form.
3. **Given** a missing required field, a malformed email, an over-long value, a value outside a
   select's allow-list, or a field the form does not declare, **Then** the submission is rejected
   with `422` and per-field error codes, and nothing is stored.
4. **Given** the mail relay is down, **Then** the submission still succeeds and the failed
   notification is recorded against the lead.
5. **Given** a body above the size limit or an unsupported content type, **Then** `413` / `415`.

---

### User Story 2 - Spam is stopped without annoying humans (Priority: P1)

Bots that fill every input, post instantly, post from one address in volume, or fail an optional
challenge are rejected or silently discarded; ordinary visitors never notice.

**Why this priority**: A public form without spam control is unusable within days.

**Independent Test**: Table-driven tests on the honeypot, timing token (forged, other-form,
expired, too fast), rate limiter (including bounded memory under many distinct addresses), and
the captcha verifier interface with a fake.

**Acceptance Scenarios**:

1. **Given** the honeypot field is non-empty, **Then** the response looks successful, nothing is
   stored and nobody is notified.
2. **Given** a form requires a minimum fill time, **When** the token is missing, forged, issued
   for another form, expired, or younger than the minimum, **Then** the submission is rejected
   with a specific code so the page can recover.
3. **Given** more than the configured submissions per minute from one client address, **Then**
   `429` with `Retry-After`. Forwarding headers are honoured only when `trust_proxy` is set.
4. **Given** a flood of distinct addresses, **Then** tracked state never exceeds
   `max_tracked_ips`.
5. **Given** captcha is enabled for a form, **Then** a failed challenge is `400`, an unreachable
   provider is `503` (fail closed), and the provider token is never stored.

---

### User Story 3 - Staff read and triage leads through the `leads` capability (Priority: P2)

The admin UI (and other modules) list, read, annotate and export leads over an authenticated
HTTP API.

**Why this priority**: Without it the leads are only visible in email.

**Independent Test**: Exercise every endpoint with admin, read-only and absent credentials.

**Acceptance Scenarios**:

1. **Given** no or a wrong bearer token, **Then** `401`; **Given** a read-only token on a write
   endpoint, **Then** `403`.
2. **Given** several leads, **Then** list is newest-first with filters (form, status, since) and
   cursor paging that neither skips nor repeats rows.
3. **Given** a lead whose fields start with `=`, `+`, `-`, `@`, tab or CR, **Then** the CSV export
   prefixes them with `'` so spreadsheets do not execute them.
4. **Given** a status update or note, **Then** it is persisted and `updated_at` changes; invalid
   statuses are `400`.

---

### User Story 4 - Other systems learn about new leads (Priority: P2)

A configured webhook receives a signed `lead.created.v1` envelope; the event's JSON Schema is
published so the CRM and other subscribers can code against it before the event bus exists.

**Acceptance Scenarios**:

1. **Given** a webhook URL and secret, **Then** each stored lead is POSTed with an HMAC-SHA256
   signature over `timestamp.body`, retried with exponential backoff on network errors, 5xx and
   429, not retried on other 4xx, and redirects are not followed.
2. **Given** the schema `schemas/events/lead.created.v1.json`, **Then** valid and invalid
   fixtures behave as expected and everything the gateway emits validates.

---

### User Story 5 - Operators can run it safely (Priority: P2)

The service ships as a module manifest and a distroless image, starts from a config file plus
environment secrets, logs in structured form, and stops gracefully.

**Acceptance Scenarios**:

1. **Given** `modules/forms/module.yaml`, **Then** `can validate` passes: in-house provider,
   `leads` provided, `lead.created.v1` published, data class with retention, not sensitive.
2. **Given** SIGTERM, **Then** in-flight requests finish, queued notifications flush, the
   database closes, and the process exits 0.
3. **Given** a bad config or missing secret, **Then** it exits 2 naming the problem.

### Edge Cases

- Cross-origin `Origin` not on the allow-list: `403` before any processing; preflight likewise.
- JSON bodies with nested values, numbers, arrays, repeated urlencoded keys, trailing data: `400`.
- Line breaks in a single-line field or control characters anywhere: rejected, so submitted data
  cannot inject headers into the notification email.
- Notification queue full or shut down: the lead stays stored; a warning is logged.
- Retention: expired leads are deleted hourly and at start-up.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The service MUST accept `POST /_forms/<form-id>` (with or without the prefix) as
  `application/x-www-form-urlencoded` or `application/json` and reject other media types.
- **FR-002**: Form definitions MUST be loaded from strictly validated config; all config problems
  are reported together; unknown keys are errors.
- **FR-003**: Validation MUST support required, email, min/max length, select allow-list,
  optional multi-line, and MUST reject undeclared fields.
- **FR-004**: Anti-spam MUST include honeypot, signed-timestamp minimum fill time, per-IP rate
  limit with bounded memory, and an optional captcha verifier behind an interface.
- **FR-005**: Leads MUST be stored in SQLite (pure Go) via versioned, embedded migrations with
  indexes and a per-lead retention date; expired leads are purged.
- **FR-006**: The `leads` API MUST offer list, get, update status/note and CSV export, guarded by
  an `Authenticator` interface with `leads.read` / `leads.write` permissions, on a separate
  listener from the public route.
- **FR-007**: CSV export MUST neutralise spreadsheet formula injection.
- **FR-008**: Notifications MUST go through an `email.send` client interface and a signed,
  retrying webhook sender; failures MUST NOT lose or block the lead and MUST be recorded.
- **FR-009**: `schemas/events/lead.created.v1.json` MUST exist with contract tests; the module
  manifest MUST reference it and validate under `module.v1`.
- **FR-010**: Request bodies MUST be size-limited; responses MUST carry security headers; CORS
  MUST be same-origin unless origins are allow-listed; credentials are never allowed.
- **FR-011**: The service MUST log structured JSON without personal data, shut down gracefully,
  and expose `/healthz`.
- **FR-012**: A distroless Dockerfile fragment with documented digest pinning MUST ship under
  `deploy/forms/`.

### Key Entities

See [data-model.md](data-model.md): Form, Field, Lead, Delivery, Event.

## Success Criteria *(mandatory)*

- **SC-001**: `make test` passes with no network and no container runtime.
- **SC-002**: Idle RSS of the static binary is at most 15 MB (measured, recorded in research.md).
- **SC-003**: Every acceptance scenario above is covered by an automated test written before the
  code that satisfies it.
- **SC-004**: `modernc.org/sqlite` adds no cgo and no copyleft licence; `can licences --check`
  still passes.

## Assumptions

- The `email.send` endpoint and token introspection are provided by can-core (epic E04); until
  then the mailer URL is configured and the API uses static bearer tokens behind the same
  interfaces.
- Traefik routing, the chart templates and the admin leads view belong to E03/E06/E12; this epic
  delivers the service, its contract and its manifest.
- The event bus does not exist yet; the direct webhook is the delivery path.
