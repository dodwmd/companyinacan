# Tasks: Board and entity modules (E17b)

Order in which the work was actually done. `[x]` done and verified offline; `[ ]` open. Test-first status is
stated per phase rather than assumed. Tool, not legal advice.

## Phase 1: Spec artifacts
- [x] spec.md, plan.md (constitution check), research.md, data-model.md, contracts/board-api.yaml (OpenAPI 3.1),
      contracts/events.md, measurements.md

## Phase 2: Policy, quorum and tally (tests written first, observed failing)
- [x] `policy_test.go` (matrix: 8 roles × 12 relationships × every action, field access cases, separation of
      duties with NFKC, case folding, invisible characters and person id) and `tally_test.go` (quorum table, tally
      table, 20 000-case property test against exact arithmetic, fuzz target) written before any implementation;
      the package did not build (`undefined: Decide`, `QuorumRequired`, `TallyPasses`) until `policy.go` and
      `model.go` existed; then green on the first run.
- [x] Keyring, AEAD with AAD, audit chain, outbox, rate limiter, authenticators, config, CLI copied from the equity
      service and adapted (same tests of intent in `security_test.go`)

## Phase 3: Service (implemented, then tests; each test mutation-checked where noted)
- [x] Bodies and members (terms, one chair, history), facts resolved from the store
- [x] Meetings: notice as data with waiver reference, invitees, agenda, attendance (frozen after approved minutes),
      exact quorum, board pack (references only, supplementary after issue); `board.meeting.scheduled.v1`
- [x] Conflicts and recusal; subjects recused automatically; voter list fixed when voting opens
- [x] Resolutions: versions, propose, open vote with quorum, roll call, written consent with signature references,
      secret ballot (receipts and ballots), close, withdraw; tallies immutable (service and triggers);
      `board.resolution.passed.v1`
- [x] Proposer never the sole approver (open, consent and secret-ballot variants)
- [x] Minutes: drafts, approval with separation of duties, immutability, corrections as new versions, integrity hash
- [x] Action items sealed, linked to `work` by opaque id
- [x] The `resolutions` seam: request, outcome, `integration` role, external type and reference on the event
- [x] Entity: legal entities with sealed identifiers, effective-dated contacts, three registers as ledgers with
      replay validation, corrections, as-of and known-at views, attestation with separation of duties
- [x] Filings calendar: obligations, instances, reminder sweep with deduplication, month-end clamp, `entity.filing.due.v1`
- [x] CSV exports with formula-injection protection, audited before the first byte
- [x] Retention sweep, legal hold, erasure with tombstones and scrub, purge hook, key rotation with compare-and-swap
- [x] Mutation checks: removing the audit call from a read, and loosening the inclusive tally, each made a test fail

## Phase 4: API and contracts
- [x] REST surface (52 paths), 404 for the invisible (byte-identical to a missing id), strict JSON, 64 KiB bodies,
      valid credentials never throttled by failed ones, idempotency on resolution creation
- [x] `module.yaml` for `board` and `entity`, settings schemas, chart values, Dockerfile fragment, release wiring
      (images, binaries, dependabot), `docs/board.md`, `docs/entity.md`, catalogue rows
- [x] Capability schemas `board.v1`, `resolutions.v1`, `entity.v1`; event schemas with fixtures; contract tests
      (manifests, fixtures, emitted events validated, OpenAPI against routes, tombstones accepted by the E08 ledger)
- [x] RAM measured locally: 14.4 MiB idle (target 20)

## Open
- [ ] k3d integration tests (module enable, NetworkPolicy, keyring Secret, health)
- [ ] Footprint measured in a container on a cluster
- [ ] can-core: route token introspection, inject the keyring Secret, deploy `board` and `entity` as one release,
      read `approvals` from the manifest
- [ ] Equity side of the approval adapter (map `ApprovalEngine.Request/Status` onto `resolutions`); equity is not
      modified in this slice
- [ ] Helm templates, admin UI, director and observer portal, investor portal
- [ ] Document storage and e-signature integration (references and signature references only today)
- [ ] Reconcile the members register with the equity share ledger
- [ ] Trademark and domain calendars as dedicated records (obligations can stand in)
- [ ] A way for a body to vote by proxy, casting votes or weighted votes (jurisdiction-specific, not implemented)
- [ ] Persist who requested an external resolution with a person id, not only a subject
