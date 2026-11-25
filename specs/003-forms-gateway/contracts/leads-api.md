# Contract: `leads` capability (port 8081, ClusterIP only)

Schema name in the manifest: `leads.v1`. All requests need `Authorization: Bearer <token>`.
No CORS: the API is server to server.

| Request | Permission | Response |
|---|---|---|
| `GET /v1/leads` | `leads.read` | `200 {"items": [Lead...], "next_cursor": "<seq>"}` (cursor omitted on the last page) |
| `GET /v1/leads/{id}` | `leads.read` | `200 Lead + "deliveries": [{kind, ok, attempts, last_error, at}]`, `404` |
| `PATCH /v1/leads/{id}` | `leads.write` | body `{"status"?: s, "note"?: s}`, at least one, unknown keys rejected; `200 Lead`, `400`, `404` |
| `GET /v1/leads/export.csv` | `leads.read` | `text/csv`, attachment; at most 10,000 rows (`X-Export-Truncated: true` when cut) |
| `GET /healthz` | none | `200`, `503` when the database does not answer |

Query parameters (list and export): `form` (form id), `status` (`new|contacted|qualified|closed|spam`),
`since` (RFC 3339), `limit` (1 to 1000, default 50, list only), `cursor` (opaque integer from
`next_cursor`). Invalid values are `400`.

`Lead`: `{id, form_id, status, email?, name?, fields{}, note?, created_at, updated_at, retention_until}`.

Errors: `401` (with `WWW-Authenticate: Bearer`), `403 forbidden` (missing permission), `404`,
`405`, `400 bad_request`; body `{"code","message"}`.

CSV: header `id,form_id,created_at,status,email,name,note` then the sorted union of field names.
Any cell starting with `=`, `+`, `-`, `@`, tab or CR is prefixed with `'`.
