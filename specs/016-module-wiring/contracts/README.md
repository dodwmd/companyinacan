# Contracts (E03c)

The machine-readable parts live with the APIs they extend, and are covered by the existing route
contract tests:

| Contract | File |
|---|---|
| Module API: `POST /v1/tokens/introspect` (personal access tokens, `audience`, `kind`, `subject`, `permissions`), `POST /v1/events` (`source_id`) | `specs/005-can-core-mvp/contracts/module-api.yaml` |
| Admin API: `GET/POST /api/v1/tokens`, `DELETE /api/v1/tokens/{id}` | `specs/005-can-core-mvp/contracts/admin-api.yaml` |
| Admin UI: `GET/POST /tokens`, `POST /tokens/{id}/revoke` | `specs/010-admin-ui/contracts/ui-routes.md` |
| finance: `POST /v1/invoices/{id}/request-approval` | `specs/014-finance-basics/contracts/openapi.yaml` |

## Module event endpoint (new, not part of any module's public API)

Every module that subscribes to events serves `POST /v1/can/events` on its existing listener. It is
mounted by the module's app only when `CAN_API_URL` is set, is authenticated by the signature, and is not
listed in the module's OpenAPI document.

| Header | Meaning |
|---|---|
| `X-Can-Event-Id`, `X-Can-Event-Type`, `X-Can-Attempt` | informational |
| `X-Can-Timestamp` | unix seconds; must be within 5 minutes of the receiver's clock |
| `X-Can-Signature` | `v1=` + hex HMAC-SHA256(`CAN_WEBHOOK_SECRET`, `<timestamp>.<body>`) |

Answers: `204` handled or already seen; `401` bad or stale signature, or no secret configured; `405`
other methods; `413` body over 1 MiB; `500` the handler failed (can-core retries with backoff).

Body: the can-core envelope `{id, type, time, source, data}`.

## Subscriptions declared in manifests

| Module | Event | Endpoint | Effect |
|---|---|---|---|
| people | `candidate.hired.v1` | `people-api:8081/v1/can/events` | person in onboarding + checklist, once per `candidate_id` |
| jobs | `approval.decided.v1` | `jobs:8080/v1/can/events` | requisition approval applied (only `type: requisition`) |
| finance | `approval.decided.v1` | `finance:8080/v1/can/events` | invoice decision recorded (only `type: invoice`) |

`approval.decided.v1` is delivered by can-core only to the module that owns the request
(`PublishTo`). `data` carries `request_id`, `module_id`, `type`, `ref`, `requester`, `outcome`
(`approved`, `rejected`, `cancelled`, `expired`), `approvers`, `decided_at`.

## Notification rule

forms: `{event: lead.created.v1, recipients: ["role:sales"]}`: an inbox item of type `lead.created` for
everyone holding `forms:sales`; only `*_id` members of the event are copied into it.

## Approval types

| Module | Type | Default chain | Ref |
|---|---|---|---|
| jobs | `requisition` | `[admin]` | `job:<id>` |
| finance | `invoice` | `[finance]` | `invoice:<id>` |

## CLI

`can-core admin secrets rotate <module> <key> [--db path]`: `key` is `CAN_WEBHOOK_SECRET`, one of the
module's generated keys, or `keyring`. Exit 1 for an unknown module, an unknown key, or a key that must
not be rotated (`CAN_JOBS_HASH_KEY`). Prints no value.
