# forms

In-house forms gateway: the lead capture for the static site. Library `core/forms`, binary
`cmd/can-forms`, image `deploy/forms`. It is a module like any other (`module.yaml`), provides
the `leads` capability and publishes `lead.created.v1`.

## How a submission flows

```
browser --POST /_forms/contact--> Traefik (prefix stripped) --> :8080 can-forms
   same-origin check -> rate limit -> parse (urlencoded|JSON, size capped)
   -> honeypot -> timing token -> optional captcha -> field validation
   -> SQLite insert -> 303/201 to the visitor
   -> async: email.send notification, signed webhook (retries), delivery outcome recorded
```

The site fetches a signed timestamp with `GET /_forms/<id>/token` when the page loads and
submits it as the hidden field `_t`. A submission that arrives sooner than `min_fill_seconds`
after that, or with a missing, forged, other-form or expired (24 h) token, is rejected.
The honeypot field (default `website`) must stay empty; bots that fill it get a normal-looking
success and nothing is stored.

### Embedding

```html
<form method="post" action="/_forms/contact">
  <input name="name" required> <input name="email" type="email" required>
  <textarea name="message" required></textarea>
  <input name="website" tabindex="-1" autocomplete="off" style="position:absolute;left:-9999px" aria-hidden="true">
  <input type="hidden" name="_t" id="_t">
</form>
<script>fetch('/_forms/contact/token').then(r => r.json()).then(j => document.getElementById('_t').value = j.token)</script>
```

With a captcha provider enabled the provider's widget adds `cf-turnstile-response` or
`h-captcha-response`; the gateway verifies it server side and does not store it.

## Configuration (`forms.yaml`)

Secrets are never in this file; see the environment variables in `deploy/forms/README.md`.

```yaml
listen: ":8080"             # public (default)
api_listen: ":8081"         # leads capability (default)
db_path: /data/forms.db
path_prefix: /_forms        # accepted if Traefik did not strip it
trust_proxy: true           # ONLY behind Traefik: use the right-most X-Forwarded-For entry; see below
max_body_bytes: 65536
cors_allowed_origins: []    # empty = same-origin only
rate_limit: { per_minute: 10, max_tracked_ips: 10000 }
retention_days: 730
# Notification email goes through can-core's email.send (docs/email.md). An installed module uses its
# projected token and needs nothing here; elsewhere set the endpoint (token: CAN_FORMS_EMAIL_TOKEN).
# email: { url: http://core:8090/v1/email }
# notify_email: [owner@example.com]   # default for forms without their own (the module setting notify_email)
webhook: { url: https://hooks.example.com/leads, max_attempts: 5 }
captcha: { provider: none }  # none | turnstile | hcaptcha
forms:
  - id: contact
    name: Contact
    redirect: /thanks/       # same-origin path; JSON clients get 201 instead
    notify_email: [owner@example.com]
    min_fill_seconds: 3
    captcha: false
    fields:
      - { name: name, type: text, required: true, max_length: 100 }
      - { name: email, type: email, required: true }
      - { name: topic, type: select, options: [sales, support] }
      - { name: message, type: text, required: true, max_length: 5000, multiline: true }
```

Unknown fields in a submission are rejected (422), as are unknown keys in this file.
`can-forms check-config -config forms.yaml` validates a file without starting anything.

### `trust_proxy`

Set it to `true` only when every request reaches the service through a proxy that appends the
real client address to `X-Forwarded-For` (Traefik in the cluster). If the service is reachable
directly, leave it `false` (the code default): otherwise clients could spoof the header and
dodge the rate limit. The chart default is `true` because the public Service is only exposed via
Traefik.

### Token replay

The timing token is stateless, so one token can be reused for any number of submissions during
its 24 hour life; it proves "this page was loaded at least N seconds ago", not "one submission
per page load". Volume is bounded by the per-IP rate limit, not by the token.

## The `leads` capability (port 8081, bearer token)

| Request | Permission | Notes |
|---|---|---|
| `GET /v1/leads?form=&status=&since=&limit=&cursor=` | `leads.read` | newest first; `next_cursor` when more |
| `GET /v1/leads/{id}` | `leads.read` | includes notification `deliveries` |
| `PATCH /v1/leads/{id}` `{"status":..,"note":..}` | `leads.write` | status: new, contacted, qualified, closed, spam |
| `GET /v1/leads/export.csv` | `leads.read` | formula-injection safe (`=+-@` cells get a leading `'`); capped at 10,000 rows |

Authentication goes through the `Authenticator` interface: static bearer tokens
(`CAN_FORMS_API_TOKEN` read+write, `CAN_FORMS_API_READ_TOKEN` read-only; can-core generates both when
the module is installed) and, installed under can-core, personal access tokens issued in the admin UI
and introspected through can-core (`leads.read`, `leads.write`). See [`docs/module-wiring.md`](../../docs/module-wiring.md).

## Webhook

`POST` of the `lead.created.v1` envelope (`schemas/events/lead.created.v1.json`) with headers
`X-Can-Event-Id`, `X-Can-Event-Type`, `X-Can-Timestamp` and
`X-Can-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body))`. Receivers must verify
the signature and reject timestamps older than a few minutes (`forms.VerifySignature`).
Delivery is retried in-process, best effort: network errors, 5xx and 429 are retried with exponential backoff
(1 s, 2 s, 4 s, ... up to `max_attempts`); other 4xx are permanent. Redirects are not followed.
**Installed under can-core** the event goes to can-core (`POST /v1/events`) instead: ids only (`lead_id`,
`form_id`, `submitted_at`, an empty `fields`), never the name, e-mail address or message, because
can-core keeps events for 30 days and feeds notifications from them. The manifest's notification rule
turns each lead into an inbox item for the `sales` role. Four attempts with backoff, then the event is
dropped: the lead is stored, but forms still has no outbox. The public listener has no route through
can-core's Traefik (the installer allows only `<subdomain>.<domain>` hosts, not the same-origin
`/_forms` path), so the chart's ingress stays off: see [`docs/module-wiring.md`](../../docs/module-wiring.md).

Retries live in memory: if the process stops mid-retry the event is not resent (the outcome so far is in `lead_deliveries`, and the lead is stored). Consumers should still de-duplicate on the event `id`. This is the interim path until the can-core event bus (at-least-once outbox) exists.

## Privacy and retention

Stored: submitted field values, status, note, timestamps. Not stored: client IP, user agent,
spam tokens. The access log omits query strings, client addresses and submitted data.
Every lead gets `retention_until` at insert time (`retention_days`, default 2 years, matching
`data_classes` in `module.yaml`) and an hourly job deletes expired rows. Backups are a
`VACUUM INTO` copy made by `can-forms backup` ([backup and restore](../../docs/backup-restore.md#in-house-modules)).

## Footprint

Idle RAM target 15 MB (limit 32 MB). Measured on the static linux/amd64 build: 13.9 MiB RSS
after start and 15.4 MiB after 200 requests (VmRSS), so the idle target is met only
approximately; the 32 MB limit has wide headroom. Memory-bounded by design: one SQLite
connection, a rate limiter capped at `max_tracked_ips`, a 256-entry notification queue, request
bodies capped at `max_body_bytes`.
