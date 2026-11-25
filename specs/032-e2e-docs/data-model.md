# Data model: E23

The suite adds **no table and no stored field to any product**. What it creates in the stack, and how it stays
repeatable:

| Scenario | Data created | Naming and idempotence |
|---|---|---|
| (a) site, lead | one lead per run | email `lead-<unique>@acme.example`; the foreign-origin post must not be stored |
| (b) admin | audit entries; a rotated TOTP secret (once per process) | the secret is read from `can-core admin reset-password --rotate-totp`; a run therefore rotates the admin's secret |
| (c) work | project `E<5 letters>`, one issue, one comment, one service token | key and names derive from a run-unique suffix |
| (d) people | four people (HR, manager, two employees), a leave type `e2e-vacation`, a 1:1 series, meetings, a leave request, a salary on one person | fixed work emails and `sso_subject`s are **get-or-create** (listed first); the leave request picks a free weekday; the on-leave status is reset to active on cleanup |
| (e) jobs | recruiter and interviewer tokens, one requisition, one application | titles, subjects and candidate email carry the unique suffix |
| (f) finance | a party, an invoice, an approver token, a payment | unique names |
| (g) equity | an entity with its class, plan, stakeholder and grant | a new entity per run |
| (i) restart | one record per module | unique names; read back after `down` and `up` |

## Fixed identities (public, fake, test only)

`dev/compose/config/introstub.json` maps five bearer tokens to subjects and roles. They are accepted by `people` and
`equity` only when the e2e overlay is used.

| Token | Subject | Roles |
|---|---|---|
| `e2e-intro-hr-token-...` | `user:e2e-hr` | hr |
| `e2e-intro-manager-token-...` | `user:e2e-manager` | employee, manager |
| `e2e-intro-ann-token-...` | `user:e2e-ann` | employee |
| `e2e-intro-ben-token-...` | `user:e2e-ben` | employee |
| `e2e-intro-board-token-...` | `user:e2e-board` | board |

## Harness types

`Env` (port, compose command, repository root), `Client` (virtual-host dialer, optional cookie jar), `Req`,
`Response` (status, header, body, `JSON`, `Str`, `Expect`), `API` (client bound to a host and token), `T` (the part of
`testing.TB` the helpers use), `TOTP`, `ParseTOTPSecret`, `CSRF`, `Wait`, `Unique`.
