# Contract: public form endpoints (port 8080)

Reached at `/_forms/...` through Traefik (prefix stripped before the service; also accepted if
not stripped).

| Request | Success | Failure codes |
|---|---|---|
| `GET /<id>/token` | `200 {"token": "..."}` | `404 unknown_form`, `403 origin_not_allowed` |
| `POST /<id>` urlencoded or JSON | HTML clients: `303` to the form's `redirect`, or `200` thank-you page; JSON clients (JSON body or `Accept: application/json`): `201 {"ok": true}` | below |
| `OPTIONS /<id>` | `204` (CORS headers only for allow-listed origins) | `403` unknown origin |
| `GET /healthz` | `200 {"ok": true}` | |

Reserved inputs: `_t` (timing token), the honeypot field (default `website`), and the provider's
response field (`cf-turnstile-response`, `h-captcha-response`). They are never stored.

Errors (JSON clients): `{"code": ..., "message": ..., "errors": [{"field","code","message"}]}`.
HTML clients get a small static page with the same status.

| Status | `code` | Meaning |
|---|---|---|
| 400 | `bad_request` | body unreadable, repeated keys, nested or non-string JSON values |
| 400 | `form_token_invalid` / `form_token_expired` / `submitted_too_fast` | timing token problems |
| 400 | `captcha_failed` | challenge failed |
| 403 | `origin_not_allowed` | `Origin` is neither same-host nor allow-listed |
| 404 | `unknown_form`, `not_found` | |
| 405 | `method_not_allowed` | `Allow` header set |
| 413 | `too_large` | body above `max_body_bytes` |
| 415 | `unsupported_media_type` | |
| 422 | `invalid_input` | per-field codes: `required`, `invalid_email`, `too_long`, `too_short`, `not_allowed`, `unknown_field`, `invalid_characters` |
| 429 | `rate_limited` | `Retry-After` seconds |
| 503 | `captcha_unavailable` | provider unreachable (fail closed) |

A non-empty honeypot returns the normal success response and discards the submission.

Every response carries `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`,
`Cache-Control: no-store`, `X-Frame-Options: DENY` and `Content-Security-Policy: default-src 'none'; ...`.
