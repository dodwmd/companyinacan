# Feature Specification: Phase 0 Measurements, CI-runnable part (Epic E00)

**Feature Branch**: `epic/E00-measurements`

**Created**: 2026-11-25

**Status**: Harness implemented; measurements run in GitHub Actions only (no cluster was available to the author). Results are in `docs/measurements/phase0-ci.md`. Real-server items are blocked, see "Blocked" below.

**Input**: Epic E00 in `docs/epics/README.md`: "Replace the guessed numbers in SPEC.md (§3.2, §3.4, §5) with measured ones, and settle the open verification items." SPEC.md §3.2 (footprint budget), §3.1 (NetworkPolicy, TokenReview, CoreDNS), §3.4 (five-minute budget), §5 sizing claims, §11 risks. Constitution II (footprint is a feature, +20% tolerance) and VII (measured claims).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A reusable footprint harness (Priority: P1)

A module author or CI job deploys a manifest onto a throw-away k3d cluster, waits for Ready, lets it
idle, samples memory over a window and gets a JSON report. `can footprint check` compares the
workload with the module's declared `ram_mb` and fails above +20% (Constitution II).

**Independent Test**: `go test ./core/footprint ./core/cli` feeds canned kubectl JSON and canned
cgroup samples through the parser, aggregator and checker with no cluster. `shellcheck` passes on
`dev/footprint/*.sh`.

**Acceptance Scenarios**:

1. **Given** raw samples and `kubectl get pods -A -o json`, **When** `can footprint report` runs, **Then** it prints per-pod median and max working set, the node total, the pod sum, the remainder (k3s, containerd, kernel) and the k3s server process RSS, as JSON.
2. **Given** a report and a module with `ram_mb: 15`, **When** `can footprint check --module ... --namespace ...` runs, **Then** 18.0 MB passes, anything above fails with exit 1, and an empty selection fails (it measured nothing).
3. **Given** malformed sample lines, **Then** parsing fails rather than producing a partial number.

### User Story 2 - Measured k3s baseline, with and without trimming (Priority: P1)

A non-required workflow `footprint.yml` creates k3s clusters in three variants (as shipped,
`make dev` flags, and lean) and records idle RAM of k3s + Traefik + CoreDNS + local-path, plus the
`deploy/base` footprint, can-static and the forms gateway.

**Independent Test**: the workflow itself. Artifacts and the step summary hold the JSON reports.

**Acceptance Scenarios**:

1. **Given** a GitHub ubuntu runner, **When** the workflow runs, **Then** each variant yields a report with a node total, pod sum, k3s RSS and per-pod figures, recorded in `docs/measurements/phase0-ci.md` with date, runner spec and run URL.
2. **Given** the forms image built from `deploy/forms/Dockerfile`, **Then** its idle RAM is compared with `modules/forms/module.yaml` `ram_mb`.

### User Story 3 - Network and module-auth proofs on a real k3s (Priority: P2)

Integration tests (build tag `integration`) instantiate the module-namespace template and check, on
a real cluster: PodSecurity `restricted` admission, pod-to-node and API-server egress denial,
the CoreDNS rewrite from a module namespace, module-to-module isolation, the module API port
versus the admin port, TokenReview of a module ServiceAccount token (valid, wrong audience,
tampered), and can-core's RBAC scope. Each has a positive control so a denial cannot be a broken
probe.

**Acceptance Scenarios**: pass or fail on the cluster; failures are findings, filed as issues
and recorded in the measurements doc, never silenced.

### User Story 4 - Honest SPEC numbers (Priority: P2)

`SPEC.md` is changed only where a number was measured, with the CI-runner caveat; everything else
stays marked as an estimate. Items needing a real 2 GB/4 GB VPS or a provider account are listed
as blocked.

## Requirements

- **FR-001**: `core/footprint` parses `kubectl get pods -o json`, the raw cgroup sample format and `kubectl top pods` output, aggregates a window into medians and maxima, and checks against `ram_mb` x (1 + tolerance).
- **FR-002**: The memory figure is the cgroup v2 working set (`memory.current` minus `inactive_file`), in MiB; `kubectl top` is not used by default because `dev/k3d.yaml` disables metrics-server.
- **FR-003**: `dev/footprint/measure.sh` creates the cluster, imports images, applies manifests, waits for all pods Running and Deployments Available, idles, samples, reports, and always deletes the cluster.
- **FR-004**: The workflow uses SHA-pinned actions, `contents: read`, the checksum-verified k3d binary of `dev-cluster.yml`, and is non-required (`continue-on-error`, separate file).
- **FR-005**: Integration proofs run under the `integration` build tag only; `make test` never needs a cluster.
- **FR-006**: No SPEC number changes without a CI measurement behind it.

## Success Criteria

- **SC-001**: `make test` passes with the new package and CLI tests.
- **SC-002**: A `footprint` workflow run on GitHub completes its measurement jobs and the recorded numbers match the run logs (URLs in the measurements doc).
- **SC-003**: Every proof that fails is reported as a finding with the observed output.

## Blocked (human-gated, not guessed)

- Idle RAM on a real 2 GB and 4 GB VPS (needs a provider account; a 16 GB runner cannot show swap, OOM or kernel-memory behaviour of a small box).
- `make up` end to end in five minutes (needs a provider token and E06 provider code run against a real account).
- Image-pull time on a real network, sslip.io and Let's Encrypt behaviour, firewall rules (ufw) with k3s on a real host.
- Licence and footprint checks of wrapped candidates other than forms (FreeScout on Postgres, Listmonk, GoatCounter/Umami, Plane, Kimai/Solidtime, Documenso, Snipe-IT, Invoice Ninja/Akaunting): upstream images can be pulled in CI later, but each needs a configured database, so they are out of this slice.
