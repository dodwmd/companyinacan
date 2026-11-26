# Plan: Outbound email (E03d)

## Shape

| Piece | Where |
|---|---|
| Relay resolution, rules, outbox, worker, SMTP, notify adapter | `core/server/email` |
| Address rules and `email:` of company.yaml | `core/config/address.go`, `email.go`, `schemas/company.v1.schema.json` |
| Table | `core/server/store/migrations/0010_email_outbox.sql` |
| Module API | `core/server/moduleapi/email.go`, contract `specs/005-can-core-mvp/contracts/module-api.yaml` |
| Admin page | `core/server/adminui/email.go` |
| Wiring, dev module identity | `core/server/runtime/email.go` |
| Forms | `core/forms/notify.go`, `modclient.SendEmail`, chart and `notify_email` setting |
| Deploy | `deploy/can-core`, `deploy/base/networkpolicies.yaml`, `core/launch/smtp.go`, `core/cli/up_email.go` |
| Dev | `dev/compose/smtpsink`, compose files, `test/e2e/email_test.go` |

Standard library only (`net/smtp`, `crypto/tls`, `mime`). No new dependency.

## Constitution check

- Spec is the source of truth: refines SPEC 4.3/5; no contradiction. SPEC 5 mentions relay
  credential injection for SMTP-speaking apps; that part stays open and is listed as not built.
- Footprint: one goroutine and one table in can-core; no new pod in a cluster.
- Secure and private: see FR-002 to FR-011.
