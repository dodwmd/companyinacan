# Contract: module management routes (E12b)

The normative route table is `specs/010-admin-ui/contracts/ui-routes.md` (extended by this epic; the
test `TestRoutesMatchContract` compares it with the served routes). This file adds the flows, field
limits and status codes.

## Flows

```text
Catalogue --POST /changes--> /changes/{id} (preview) --POST /changes/{id}/apply--> /runs/{id} (progress)
Module page --POST /catalogue/{id}/settings--> /changes/{id} --apply--> /runs/{id}
Wizard /setup/{step} ... /setup/review --POST /setup/apply--> /runs/{id}
Disabled module --GET /catalogue/{id}/purge--> POST (typed confirmation + password)
```

Nothing is written to `company.yaml` before an explicit `apply`. `GET` routes never change state.

## Permissions (`platform.*` are reserved for can-core, `specs/015`)

| Route group | Permission |
|---|---|
| `GET /catalogue`, `GET /catalogue/{id}`, `GET /runs/{id}`, `GET /status` | `platform.view` |
| `POST /catalogue/{id}/settings`, `POST /changes`, `GET /changes/{id}`, `POST /changes/{id}/apply`, `GET /company` | `platform.operate` |
| `GET/POST /setup...` | `platform.setup` |
| `GET/POST /catalogue/{id}/purge` | `platform.purge` |

Order of checks: session (303 `/login`), CSRF and same-site (403), permission (403), module management
configured (503), then the handler.

## Form fields

| Route | Fields |
|---|---|
| `POST /changes` | `version` (required, must equal the stored version), `enable` (repeated, module ids), `ack` (repeated, `licence:<id>`, `experimental:<id>`; at most 400) |
| `POST /catalogue/{id}/settings` | `s.<property>` for each rendered property; bool: present means true; int: base-10 integer or empty |
| `POST /catalogue/{id}/purge` | `confirm` must equal `purge <id>`; `password` |
| `POST /setup/company` | `name` (1 to 80 chars), `domain` (lowercase DNS name), `server_ram_mb` (empty or 256 to 1048576) |
| `POST /setup/template` | `template` (a known template id) |
| `POST /setup/modules` | `enable`, `ack` as for `/changes` |
| `POST /setup/email` | `host`, `port` (1 to 65535), `username`, `password` (1 to 1024 bytes, never echoed), `from` (address) |
| `POST /setup/account` | `kit` (present means acknowledged) |
| `POST /setup/apply`, `POST /setup/dismiss` | none |

## Status codes

| Code | When |
|---|---|
| 200 | page |
| 303 | after a POST (to the next page), unauthenticated, or the first-login redirect of the owner from `/` to `/setup` |
| 400 | invalid form, missing acknowledgement, not-deployable module, selection that cannot work, settings rejected by the schema, wrong purge confirmation |
| 401 | purge: wrong current password |
| 403 | no session token (CSRF), cross-site, missing permission |
| 404 | unknown module, change, run or step |
| 409 | stale form or proposal, expired proposal, purge of a module that is not disabled |
| 429 | purge: password attempts throttled (`Retry-After`) |
| 502 | email relay Secret could not be written |
| 503 | module management not configured; another apply is running (`Retry-After: 10`); no purge executor |

## Response notes

- `GET /runs/{id}` includes `<meta http-equiv="refresh" content="3">` only while the run is `running` and the query does not carry `refresh=0`.
- Error texts from the installer are sanitised (control characters, credentials, length).
- Passwords (relay, purge) never appear in any response, log line, audit entry or stored row.
