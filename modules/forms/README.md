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
trust_proxy: true           # use the right-most X-Forwarded-For entry (set by Traefik)
max_body_bytes: 65536
cors_allowed_origins: []    # empty = same-origin only
rate_limit: { per_minute: 10, max_tracked_ips: 10000 }
retention_days: 730
email: { url: http://can-core.can-system.svc/v1/email/send, from: forms@example.com }
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

## The `leads` capability (port 8081, bearer token)

| Request | Permission | Notes |
|---|---|---|
| `GET /v1/leads?form=&status=&since=&limit=&cursor=` | `leads.read` | newest first; `next_cursor` when more |
| `GET /v1/leads/{id}` | `leads.read` | includes notification `deliveries` |
| `PATCH /v1/leads/{id}` `{"status":..,"note":..}` | `leads.write` | status: new, contacted, qualified, closed, spam |
| `GET /v1/leads/export.csv` | `leads.read` | formula-injection safe (`=+-@` cells get a leading `'`); capped at 10,000 rows |

Authentication goes through the `Authenticator` interface. Until can-core exposes token
introspection, static bearer tokens (`CAN_FORMS_API_TOKEN` read+write, `CAN_FORMS_API_READ_TOKEN`
read-only) are used.

## Webhook

`POST` of the `lead.created.v1` envelope (`schemas/events/lead.created.v1.json`) with headers
`X-Can-Event-Id`, `X-Can-Event-Type`, `X-Can-Timestamp` and
`X-Can-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + body))`. Receivers must verify
the signature and reject timestamps older than a few minutes (`forms.VerifySignature`).
Delivery is at least once: network errors, 5xx and 429 are retried with exponential backoff
(1 s, 2 s, 4 s, ... up to `max_attempts`); other 4xx are permanent. Redirects are not followed.
This is the interim path until the can-core event bus exists.

## Privacy and retention

Stored: submitted field values, status, note, timestamps. Not stored: client IP, user agent,
spam tokens. The access log omits query strings, client addresses and submitted data.
Every lead gets `retention_until` at insert time (`retention_days`, default 2 years, matching
`data_classes` in `module.yaml`) and an hourly job deletes expired rows. Backups are SQLite
online copies (module backup hook arrives with E08).

## Footprint

Idle RAM target 15 MB (limit 32 MB). Measured on the static linux/amd64 build: about 12 MB RSS
idle and 14 MB after 200 requests. Memory-bounded by design: one SQLite connection, a rate
limiter capped at `max_tracked_ips`, a 256-entry notification queue, request bodies capped at
`max_body_bytes`.
