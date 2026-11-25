# Tasks (E11a), all done unless marked

Test-first: each task's tests were written and seen failing before its code.

- [x] T01 Migration 0002 and `CreateAdmin` owner rule
- [x] T02 rbac: roles, assignments, overrides, cache, last-owner guard (`rbac_test.go`)
- [x] T03 events: `PublishTo`, `SubscribeAll`, catalogue (`targeted_test.go`)
- [x] T04 notify: inbox, dedupe, prefs, quiet hours, digests, rate limit, webhook channel, rules, templates (`notify_test.go`)
- [x] T05 approvals: policy validation, create (idempotent), chain, parallel, quorum, thresholds, SoD, delegation, timers, fail-closed, visibility (`approvals_test.go`)
- [x] T06 approvals: directory-backed reporting line (`directory_test.go`)
- [x] T07 module API: check, approvals, notifications (`moduleapi/platform_test.go`)
- [x] T08 admin API: approvals, delegations, notifications, users/roles, gating (`adminapi/platform_test.go`)
- [x] T09 admin UI: approvals, notifications, access; gating of existing pages (`adminui/platform_test.go`)
- [x] T10 schemas, fixtures, OpenAPI, ui-routes, contract tests
- [x] T11 runtime wiring, timers, `can apply` sync, `can-core admin roles`
- [x] T12 docs/approvals.md, docs/notifications.md, RAM measurement
- [ ] T13 real `email.send` provider (needs the provider module)
- [ ] T14 SSO group claims for roles; role override UI; module-facing "who may approve" query
- [ ] T15 cluster verification (no cluster available offline)
