# Tasks (E03c), done unless marked

Test-first where it was done, stated per task: T01-T07 (can-core, `modclient`), the finance approval tests (which exposed a deadlock),
people's hire hand-off (T12 `hire_test.go`)
and the keyring loaders had their tests written and seen failing (compile error or assertion) before
the code. For the module authenticators and event paths (T08-T11, and the jobs approver) the code was
written first and the tests after; those tests then failed on first run in four places (the
replay-window clock, the 422 status, a test that misread the work event payload, a never-queued event)
and led to fixes in the tests or code. T15 passed on its first complete run.

- [x] T01 Migration 0003 and store: tokens, publish de-duplication, rotation requests (`store/wiring_test.go`)
- [x] T02 `pat` service: issue, introspect, revoke, scope, caps, expiry, hashing at rest, audit without the token (`pat/*_test.go`)
- [x] T03 Module API: introspection of tokens with audience binding, no leaks, never throttled; `source_id` (`moduleapi/introspect_pat_test.go`, `publish_once_test.go`)
- [x] T04 Admin API and UI: issue, list, revoke, issuer-holds rule, own-only, CSRF (`adminapi/tokens_test.go`, `adminui/tokens_test.go`); contracts updated
- [x] T05 `modsecrets` and installer: generate-once, rotation, refusal before touching the cluster, never logged, roll on change (`installer/secrets_test.go`, `secretroll_test.go`)
- [x] T06 Reconciler and `can-core admin secrets rotate` (`reconcile/rotation_test.go`, `corecli/secrets_test.go`)
- [x] T07 `modclient`: client, bounded introspection cache, signed receiver, fake can-core (`modclient/modclient_test.go`)
- [x] T08 work: authenticator, events through can-core (`work/canwiring_test.go`)
- [x] T09 forms: authenticator, ids-only events through can-core (`forms/canwiring_test.go`)
- [x] T10 finance: authenticator, events, invoice approvals through the engine, hang found and fixed (`finance/canwiring_test.go`, `canapproval_test.go`)
- [x] T11 jobs: authenticator and role mapping, events, requisition approvals (`jobs/canwiring_test.go`, `canapproval_test.go`)
- [x] T12 people: authenticator and role mapping, events, hire hand-off (`people/canwiring_test.go`, `hire_test.go`); keyring files readable through `fsGroup`
- [x] T13 Manifests: `subscribes`, `approvals`, `notifications`, jobs `subdomain`
- [x] T14 Charts for the five modules, installer allowlist tests (`installer/modulecharts_test.go`), contract tests updated
- [x] T15 End-to-end with the real can-core services (`test/wiring`): lead to notification, hire to onboarding, requisition and invoice approvals, leave masked, token bound to its module
- [x] T16 Advisory k3d job: build the `work` image, install through can-core, token, issue, disable (`.github/workflows/dev-cluster.yml`, `test/integration/work_test.go`). **Written and compiled; not yet seen to pass on a cluster.**
- [x] T17 Docs: `docs/module-wiring.md`, module READMEs, epics table
- [x] T18 RAM measured (spec FR-8)
- [ ] T19 Run the k3d job and fix what it finds (needs CI)
- [ ] T20 forms: durable outbox for events; a route for the public listener (same-origin path or its own host)
- [ ] T21 finance: timesheets, expenses and purchase orders through the engine; apply the approval when it arrives
- [ ] T22 people: fetch the new hire's name and contact details through the `ats` capability
- [ ] T23 Map `company.yaml` `settings` onto each chart's `config:` (`values.tmpl.yaml`)
- [ ] T24 SSO sessions instead of personal access tokens for people's manager roles (a token cannot carry reporting-line facts beyond `performance.write`)
- [ ] T25 Encrypt Kubernetes Secrets at rest (cluster setting, not something can-core can assert)
