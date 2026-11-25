# Data Model: Forms Gateway (E05)

## Form (config, not stored)

| Field | Type | Rules |
|---|---|---|
| `id` | string | `^[a-z][a-z0-9-]{0,40}$`, unique |
| `name` | string | shown in notification subject; whitespace collapsed to one line |
| `fields` | list of Field | 1 to 30 |
| `notify_email` | list of string | bare addr-specs |
| `redirect` | string | same-origin path starting with `/`, not `//`, no backslash or CR/LF |
| `honeypot` | string | default `website`; must not equal a field name |
| `min_fill_seconds` | int | default 3, `0` disables; range 0 to 600 |
| `captcha` | bool | requires `captcha.provider != none` |
| `retention_days` | int | default from global `retention_days` (730) |

## Field (config)

| Field | Type | Rules |
|---|---|---|
| `name` | string | `^[a-z][a-z0-9_]{0,40}$` (so `_t` and provider response names are reserved), unique per form |
| `type` | enum | `text`, `email`, `select` |
| `required` | bool | |
| `min_length`, `max_length` | int | `0 <= min <= max <= 10000`; max defaults to 1000 (email 254) |
| `options` | list | required and non-empty for `select`; submitted value must match exactly |
| `multiline` | bool | text only; line breaks rejected unless true |

Values are trimmed; empty optional values are dropped; invalid UTF-8 and control characters
(other than line breaks in multiline fields) are rejected.

## Lead (SQLite table `leads`)

| Column | Type | Notes |
|---|---|---|
| `seq` | INTEGER PK AUTOINCREMENT | paging cursor, newest first |
| `id` | TEXT UNIQUE | `ld_` + 24 hex (96 random bits) |
| `form_id` | TEXT | indexed with `seq` |
| `status` | TEXT | `new` (default), `contacted`, `qualified`, `closed`, `spam` (CHECK); indexed with `seq` |
| `email`, `name` | TEXT | convenience copies: first email-typed field; field `name` |
| `fields` | TEXT (JSON) | validated submitted values |
| `note` | TEXT | staff note, up to 2000 characters |
| `created_at`, `updated_at`, `retention_until` | INTEGER | unix seconds UTC; `retention_until` indexed for purge |

State transitions: any status to any status (staff decision); no deletion through the API,
only retention purge.

## Delivery (`lead_deliveries`)

`id`, `lead_id` (FK, `ON DELETE CASCADE`), `kind` (`email` | `webhook`), `ok`, `attempts`,
`last_error` (at most 500 characters), `at`.

## Migrations

`schema_migrations(version, applied_at)`; embedded `migrations/NNN_name.sql`, applied in order,
each in a transaction, idempotent on reopen. `001_leads.sql`, `002_deliveries.sql`.

## Event: `lead.created.v1`

Envelope `{id, type, time, source: "forms", data}`; data `{lead_id, form_id, submitted_at,
email?, name?, party_id?, fields}`. See `schemas/events/lead.created.v1.json` and
[contracts/events.md](contracts/events.md).
