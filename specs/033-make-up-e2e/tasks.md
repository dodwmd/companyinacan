# Tasks: `can up` end to end (E24)

Test-first: each implementation task is preceded by its failing test (red), then made green.

- [x] T001 `core/staticsrv/extract_test.go` (red) then `extract.go`: safe tar.gz unpack (links, devices, `..`, absolute, size cap)
- [x] T002 `cmd/can-static` `-extract` test (red) then flag
- [x] T003 `deploy/site/site.yaml` (site pod: init container + can-static), `deploy/stack.go` embed
- [x] T004 `core/launch/images_test.go` (red) then `images.go`: release manifest (digest-only), local dir (sha256), resolution, fail closed
- [x] T005 `core/launch/manifests_test.go` (red) then `manifests.go`: base, can-core patch, site patch, routes, forms route, default bundle, deterministic site archive
- [x] T006 `core/launch/stack_test.go` (red) then `stack.go`, `kube.go`, `loader.go`, `backup.go`: phase machine, checkpoints, admin without argv, resume, backup + kit, status
- [x] T007 `core/provider/state.go`: `LaunchState` (additive)
- [x] T008 `core/cli/up.go`, `up_stack.go`: flags, offline preflight, routes decision, summary with confirmation, `--print-secrets`, `--kit-out`, status stack checks, down removes the timer
- [x] T009 `core/cli/up_stack_test.go`; legacy tests moved to `--provision-only`
- [x] T010 `scripts/build-images.sh`, `make images`, `make up IMAGES_FROM=`; `scripts/gen-release-images.sh` + self-test
- [x] T011 `test/integration/makeup_test.go` and `.github/workflows/make-up-e2e.yml` (advisory, SHA-pinned, `contents: read`)
- [x] T012 docs: `docs/make-up.md` (rewritten), `docs/providers.md` notes, SPEC.md section 6 note
- [ ] T013 (human-gated) time `make up` on a fresh Hetzner server three times; record in `specs/000-*/measurements.md`
- [ ] T014 (follow-up) mount the host dumps directory into can-core so the backup marker is produced; wire `gen-release-images.sh` into `release.yml`
