# Tasks: E03d

`[x]` = done and covered by a test run offline.

- [x] T01 company.yaml `email.relay` (tls, from) and `email.modules`; address and rule validation
- [x] T02 Relay resolution from Secret and company.yaml, credentials bound to host
- [x] T03 SMTP transport: required TLS, dial-time address guard, sanitised classified errors
- [x] T04 Message building without header injection
- [x] T05 Outbox, quotas, idempotency, retries, dead letters, requeue, retention, audit
- [x] T06 Module API and OpenAPI contract
- [x] T07 Notification email channel over the outbox
- [x] T08 Admin delivery page
- [x] T09 Forms: send through can-core, retries with `source_id`, safe content; `notify_email` setting in the chart
- [x] T10 Secret mount, egress policy, `can up --smtp-credentials-file`, `make up SMTP_CREDENTIALS=`
- [x] T11 Compose SMTP sink, wiring, smoke check, e2e test
- [x] T12 Docs
- [ ] T13 Verify against a real relay and on a real cluster
- [ ] T14 `can-smtp` Secret for SMTP-speaking apps; `make email`; DNS records
