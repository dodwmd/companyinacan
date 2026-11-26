# Tasks

## Done

- [x] T001 Verify upstream images exist, run unprivileged; record licences and digests (research.md)
- [x] T002 Managed Postgres in `can-data` (`dbprov.Managed`, `deploy/templates/postgres.yaml`)
- [x] T003 Generated admin passwords for analytics, support, newsletter
- [x] T004 SMTP relay to support (`can-smtp`)
- [x] T005 Charts: pinned images, real Secret names, admin bootstrap, probes, resources
- [x] T006 Offline tests (installer, dbprov, modsecrets, contract)
- [x] T007 Advisory k3d job (passed on PR #56; figures in research.md R4)

## Open

- [ ] T020 Backups (first: until then, enabling any of these modules stops all backups): publish `can-hooks`, run the hooks (with PR #49), restore each app once
- [ ] T021 Capability adapters (`tickets`, `newsletter`, `analytics`)
- [ ] T022 Re-apply a module when only the relay Secret changes (company.yaml changes already do)
- [ ] T023 Configure FreeScout mailboxes and Listmonk SMTP from the relay
- [ ] T024 Deliver a message through a real relay; verify certificate checking of each app
- [ ] T025 Verify on a real server and domain (certificate issuance, client IPs, memory after ten minutes)
- [ ] T026 Upgrade path between app versions, and a Postgres major upgrade
- [ ] T027 support on arm64 (own image build, or another publisher)
- [ ] T028 `analytics` settings `site_domain` and `retention_days` are accepted but not applied
- [ ] T029 Watch the pinned images for security releases (no automation today)
- [ ] T030 Put `support` back into the starter template once it is verified on a real server (its backups run since specs/046)
