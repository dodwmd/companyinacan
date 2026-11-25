# Company in a Can Constitution

## Core Principles

### I. Spec-First; SPEC.md Is the Source of Truth
Observable behaviour, architecture and scope are defined in `SPEC.md` and in the per-feature
artifacts under `specs/`. A change in behaviour MUST start with a change to the spec, reviewed
before code. Code that contradicts the spec MUST be fixed or the spec amended in the same PR; the
two MUST NOT drift. Rationale: the project is built from a long, carefully reviewed spec, and
spec-driven development only works if the spec stays authoritative.

### II. Footprint Is a Feature
Every component (can-core, each module, each shared service) MUST declare a RAM request and limit
in its manifest/chart. Container images MUST be pinned by digest and SHOULD be minimal
(alpine/distroless). CI MUST measure idle RAM on k3d and MUST fail when a component exceeds its
declared `ram_mb` by more than 20%. No component may add a default sidecar or always-on
dependency without a footprint justification in its plan. Rationale: small footprint is the
project's core promise (`SPEC.md` §3.2).

### III. Test-First (NON-NEGOTIABLE)
Tests MUST be written and observed to fail before the implementation that satisfies them
(red-green-refactor). Schemas (`company.yaml`, `module.yaml`, capability and event schemas) MUST
have contract tests. Behaviour that depends on Kubernetes (NetworkPolicies, token review, Helm
upgrades, restore) MUST have integration tests on a k3d cluster. The same charts and manifests
MUST run in local development and production; environment differences live only in provider and
DNS plugins. Rationale: infrastructure that cannot be tested locally cannot be trusted remotely.

### IV. Stable Module Contracts
Modules MUST interact only through versioned capabilities and events defined in
`schemas/capabilities/`, never through each other's internals, databases or private APIs. Every
module MUST declare, in `module.yaml`: provider and licence, resources, required and provided
capabilities, published and subscribed events, permissions, approval types and `data_classes`.
Breaking a capability or event schema MUST bump its version and keep the old version supported for
at least one minor release. Rationale: OSS apps are replaced by in-house modules over time without
changing the rest of the system.

### V. Secure and Private by Default
All namespaces MUST apply default-deny NetworkPolicies with explicit allows. Secrets MUST be
encrypted at rest and never committed to git. can-core and every module MUST use least-privilege
RBAC; can-core's elevated privileges MUST be documented and minimised. Sensitive data classes
(people records, salary and ID data, equity, performance, payroll) MUST be held only by in-house
modules that can enforce per-field access and audit reads. Retention, erasure and audit logging
MUST be implemented before the first module storing personal data ships. The project is a tool,
not legal, tax or payroll advice; documentation MUST say so. Rationale: the platform holds the
most sensitive data a company has (`SPEC.md` §3.7, §5).

### VI. Open-Source Hygiene
Project code is Apache-2.0. Wrapped third-party apps MUST have OSI-approved licences; anything
source-available MUST be marked `licence_class: source-available`, be opt-in and be flagged in
the UI. A `THIRD_PARTY_LICENSES` file MUST be generated and CI MUST fail when a module's upstream
licence is missing. Contributions MUST carry DCO sign-off. No secrets, credentials or real
personal or company data may be committed; examples MUST use fake data. Rationale: the project is
open source first and must stay safely forkable.

### VII. Simplicity and Measured Claims
Prefer boring technology: single-node k3s, Go, plain Helm, SQLite or the shared Postgres/MariaDB.
Speculative abstractions, multi-node features and unrequested extensibility MUST NOT be added;
complexity MUST be justified in the plan's complexity-tracking table. Performance, size and
timing claims (the five-minute `make up`, the 2 GB target, restore time) MUST be backed by
measurements recorded in the repository, not asserted. Rationale: several headline goals are
unproven until Phase 0 measures them, and the spec says so.

## Technology Constraints

- Core language: Go (CLI and can-core as one module; in-house modules in Go unless justified).
- Runtime: single-node k3s with the SQLite datastore; local development on k3d; Helm charts per
  module; Traefik (bundled) for ingress and ACME TLS.
- Data: SQLite for can-core and small in-house modules; shared Postgres and MariaDB only when an
  enabled module requires them (one database and credential per module).
- Backups: restic to S3-compatible storage, run by a host systemd timer after can-core produces
  consistent dumps.
- Providers are plugins behind the `Provider` interface (`local`, `ssh`, `hetzner` first).
- Any deviation requires a spec amendment (Principle I).

## Development Workflow

- Work is organised in epics (`docs/epics/`). Each epic is one Spec Kit feature developed on its
  own feature branch via: specify -> clarify -> plan -> tasks -> analyze -> implement.
- `plan.md` MUST include a Constitution Check against Principles I-VII before research and again
  after design.
- Pull requests MUST state which principles they touch and MUST verify compliance; reviewers
  reject changes that violate a principle without an approved amendment or a justified entry in
  the complexity-tracking table.
- CI MUST run schema contract tests, unit tests, k3d integration tests, the footprint check and
  the licence check before merge.
- Commit messages describe the change and reference the epic or task.

## Governance

This constitution supersedes other practices. Amendments are made by pull request that updates
this file, states the version bump and rationale, and updates `SPEC.md` where behaviour changes.
Versioning follows semantic versioning: MAJOR for removed or redefined principles, MINOR for new
principles or materially expanded guidance, PATCH for clarifications. Compliance is reviewed on
every pull request and at the end of each epic. Runtime guidance for agents lives in `CLAUDE.md`
and `SPEC.md`.

**Version**: 1.0.0 | **Ratified**: 2026-11-24 | **Last Amended**: 2026-11-24
