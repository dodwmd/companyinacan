# Tasks: Pod-to-node isolation

- [x] T001 `core/provider/hostfw/can-firewall.sh`: rules, set sync, install with rollback, check, disable
- [x] T002 `test/hostfw/netns.sh`: controls, enforcement, SSH, boot, sync loop, bad ruleset, rollback, check
- [x] T003 Bootstrap step, uninstall, plan text, golden files; static tests (`core/provider/hostfw_test.go`)
- [x] T004 `can doctor --server`, `can diagnose --server`, tests
- [x] T005 k3d test `test/integration/z_hostfw_k3d_test.go`; Phase 0 egress proof fails if installed and reachable
- [x] T006 Workflow `node-isolation.yml`: shellcheck, namespaces, real systemd units, k3d
- [x] T007 SPEC.md, security model, providers, node-isolation doc, template comment
- [ ] T008 Verify on a real server: ufw, reboot, current k3s, metadata, footprint (needs a provider account)
