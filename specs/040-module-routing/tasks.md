# Tasks: Module routing (E03e)

- [x] T001 `expose` in company.yaml, `public_ui` and `site_route` in module manifests, validation
- [x] T002 `core/routing`: claimable hosts and paths, routing facts
- [x] T003 Installer: routes, headers, NetworkPolicy by routed port, disable and toggle-off
- [x] T004 Reconcile fingerprint; Apps address derived from the route
- [x] T005 forms `/_forms` by can-core; `can up` and `can apply` write `can-routing`
- [x] T006 Admission policies `can-core-routes` and `can-core-routing-config`
- [x] T007 Unit and contract tests (installer, reconcile, routing, manifest/UI path coverage,
  policy tables)
- [x] T008 Advisory k3d job `module-routing`: policy refusals and work + forms end to end
- [x] T009 Docs
- [ ] T010 DNS records for `<module>.<domain>` in `can up` and the setup wizard
- [ ] T011 people, jobs and finance UIs on a cluster (their images are not built in CI)
- [ ] T012 A real certificate and a real browser
- [ ] T013 finance public invoice link route
- [ ] T014 Admin UI: show what is exposed
