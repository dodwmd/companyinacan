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

There are two ways to put a form on a site. The simple one is to link to `/_forms/<id>/form`, a page
the gateway renders itself with the timing token in it; it needs no script. It is not offered for a form
with a CAPTCHA.

A page of your own instead fetches a signed timestamp with `GET /_forms/<id>/token` when the page loads and
submits it as the hidden field `_t`. A submission that arrives sooner than `min_fill_seconds`
after that, or with a missing, forged, other-form or expired (24 h) token, is rejected.
The honeypot field (default `website`) must stay empty; bots that fill it get a normal-looking
success and nothing is stored.

### Embedding

This needs a page that is allowed to run the script. The site served by `can-static` is not: its
Content-Security-Policy blocks every script, so link to `/_forms/<id>/form` there.

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
| `POST /v1/leads/find` `{"email":..}` | `leads.erase` | ids of the leads that name the address (`address`: a field is it; `mention`: only in a text) |
| `DELETE /v1/leads/{id}` | `leads.erase` | erases one lead; safe to repeat |
| `GET /v1/erasures?after=N` | `leads.erase` | tombstones (lead id, `lead-submission`, date, time) for the backup's erasure ledger |
| `POST /v1/purge` `{"subject":..,"class":"lead-submission"}` | `leads.erase` | what `can restore` replays; an unknown lead is a no-op |

Authentication goes through the `Authenticator` interface: static bearer tokens
(`CAN_FORMS_API_TOKEN` read, write and erase, `CAN_FORMS_API_READ_TOKEN` read-only; can-core generates both when
the module is installed) and, installed under can-core, personal access tokens issued in the admin UI
and introspected through can-core (`leads.read`, `leads.write`, `leads.erase`). See [`docs/module-wiring.md`](../../docs/module-wiring.md).

## Erasure requests

When somebody asks for their data to be deleted:

1. **Find** the submissions. Admin UI: Leads, "Erasure request: find by email". Command line:

   ```sh
   kubectl -n mod-forms port-forward svc/forms-api 8081 &
   export CAN_FORMS_TOKEN="$(kubectl -n mod-forms get secret can-module -o jsonpath='{.data.CAN_FORMS_API_TOKEN}' | base64 -d)"
   can leads find            # asks for the address: not an argument, so it stays out of shell history
   can leads erase ld_... ld_... --yes
   ```

   The search compares each field and the staff note with the address, ignoring case. It does **not** find
   other ways of writing it: a `+tag` or dotted alias, the domain as punycode instead of Unicode (or the
   reverse), `bob at example dot com`, `%40`, or a submission that only gives a name or phone number. Search
   for each spelling you know of, and look through the leads view for the rest. An ASCII letter or digit right
   next to it makes it another address; anything else (a dash, an underscore, other scripts) does not, so
   expect some `mention` results that are somebody else's.
2. **Read** every `mention` first: the address is only inside a text, which may be about somebody else.
3. **Erase** each one. There is no "erase all for this address": a person decides per submission.

The admin UI needs `leads.erase` (owners; or the role `forms:admin`), a recent sign-in, and a
`CAN_LEADS_TOKEN_FILE` credential that may erase: **`CAN_FORMS_API_TOKEN` can erase since 0.2.0** (the read token
cannot), so anything already holding it now can. A forms module installed earlier is upgraded to 0.2.0 by the
next apply of a can-core that ships it; that records the new permission, role and event. Personal access
tokens issued before need to be issued again to carry `leads.erase`.

**What an erasure does:** deletes the lead and its delivery records; cancels its waiting notification email
and stops one that is being retried; records a tombstone with the lead id only; rewrites the database
(`VACUUM`) and truncates the write-ahead log (finished later if it fails or the process stops); publishes [`lead.erased.v1`](events/lead.erased.v1.json) (ids only)
to can-core and the signed webhook, retried until accepted. Who searched or erased is recorded in the
`lead_audit` table and the log, and for the admin UI in can-core's audit log. None of these holds the address
or anything that was submitted.

**What it does not reach. Check each by hand:**

- **Notification email.** Mail already sent is in the mailboxes. can-core's e-mail outbox holds its own copy
  (recipient, reply-to, subject, body) until it is sent, and for seven days if it cannot be sent
  ([docs/email.md](../../docs/email.md#data-kept)); forms cannot cancel that copy. A restore replay does remove it.
  An answer with `notification_was_under_way` means a send was in progress and may have arrived.
- **Consumers.** A webhook receiver, CRM or newsletter list got the whole submission from the webhook or typed
  in by hand. They are told the lead id; the CRM and newsletter adapters are not built, so erase there by hand.
- **Exports** (`export.csv`) and anything copied from the admin UI.
- **Backups.** Module dumps and snapshots made before the erasure hold the lead until they expire. A restore
  applies the erasure again **only if the tombstone is in the erasure ledger**, and nothing puts it there by
  itself yet. After every erasure run on the server:
  `sudo can backup ledger-add --subject <lead id> --class lead-submission`
  (`can leads erase` and the admin UI show the line).

**Retention** is separate: the hourly purge deletes expired leads and truncates the write-ahead log. It makes no
tombstone and sends no event, because a restored lead keeps its `retention_until` and is purged again.

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
dropped: the lead is stored, but forms still has no outbox. can-core routes the public listener at `/_forms` on the site
hosts (`runtime.site_route`; turn it off with `modules.forms.expose.public: false`), not the chart: see
[`docs/module-routing.md`](../../docs/module-routing.md).

Retries live in memory: if the process stops mid-retry the event is not resent (the outcome so far is in `lead_deliveries`, and the lead is stored). Consumers should still de-duplicate on the event `id`. This is the interim path until the can-core event bus (at-least-once outbox) exists.

## Privacy and retention

Stored: submitted field values, status, note, timestamps. Not stored: client IP, user agent,
spam tokens. The access log omits query strings, client addresses and submitted data.
One lead can be erased on request ([above](#erasure-requests)).
Every lead gets `retention_until` at insert time (`retention_days`, default 2 years, matching
`data_classes` in `module.yaml`) and an hourly job deletes expired rows. Backups are a
`VACUUM INTO` copy made by `can-forms backup` ([backup and restore](../../docs/backup-restore.md#in-house-modules)).

## Footprint

Idle RAM target 15 MB (limit 32 MB). Measured on the static linux/amd64 build: 13.9 MiB RSS
after start and 15.4 MiB after 200 requests (VmRSS), so the idle target is met only
approximately; the 32 MB limit has wide headroom. Memory-bounded by design: one SQLite
connection, a rate limiter capped at `max_tracked_ips`, a 256-entry notification queue, request
bodies capped at `max_body_bytes`.
