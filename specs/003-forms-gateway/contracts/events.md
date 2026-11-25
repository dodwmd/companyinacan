# Contract: `lead.created.v1`

Schema: `schemas/events/lead.created.v1.json` (JSON Schema 2020-12, `additionalProperties: false`
at both levels). Producer: forms. Subscribers declare it under `subscribes` (for example the CRM).

```json
{
  "id": "evt_0123456789abcdef01234567",
  "type": "lead.created.v1",
  "time": "2026-11-25T01:02:04Z",
  "source": "forms",
  "data": {
    "lead_id": "ld_0123456789abcdef01234567",
    "form_id": "contact",
    "submitted_at": "2026-11-25T01:02:03Z",
    "email": "ada@example.com",
    "name": "Ada",
    "fields": { "email": "ada@example.com", "message": "Hello" }
  }
}
```

- Delivery is at least once; consumers de-duplicate on `id`.
- `party_id` is optional and only set once a directory party exists (not by this service).
- Direct webhook (interim until the event bus): `POST` with headers `X-Can-Event-Id`,
  `X-Can-Event-Type`, `X-Can-Timestamp` (unix seconds) and
  `X-Can-Signature: sha256=hex(HMAC-SHA256(secret, timestamp + "." + raw body))`. Reject a
  mismatch or a timestamp outside a few minutes.
- Retries: network errors, 5xx and 429, exponential backoff starting at 1 s, doubling, capped at
  5 minutes between attempts, at most `webhook.max_attempts` (default 5). Other 4xx are permanent.
- Evolution: adding an optional property needs a new schema version only if consumers could be
  broken; removing or retyping a property is `v2`.
