# Tasks: Equity and cap table, first slice (E17a)

Order in which the work was actually done. `[x]` done and verified offline; `[ ]` open.
Test-first status is stated per phase rather than assumed. Tool, not legal, tax or securities advice.

## Phase 1: Spec artifacts
- [x] spec.md, plan.md (constitution check), research.md, data-model.md, contracts/equity-api.yaml, contracts/events.md, measurements.md

## Phase 2: Vesting calculator (tests written first, observed failing: undefined symbols)
- [x] Cliff, monthly and quarterly, month-end clamp, leap years, termination cut-off, rounding policies,
      dated and milestone tranches, single- and double-trigger acceleration as data
- [x] Property tests (never negative, never above total, monotone, full at the end) and `FuzzVested`

## Phase 3: Policy and crypto
- [x] `Decide` and `FieldAccess` (roles x actions x self); policy matrix test; managers never see reports' equity
- [x] `sodOK`: one separation-of-duties function, NFKC, invisible characters, case folding, person id
- [x] Keyring parse/load (0400/0600, no symlinks), Seal/Open with AAD binding, rotation (copied design from people)

## Phase 4: Service (implemented, then tests; the first run found a real defect: an exercise cost overflowed int64 for
extreme quantity and strike, so cost now returns an error; and a wrong termination-day boundary in the cap table)
- [x] Entities, share classes (authorised only raised), plans and pools (pool re-checked at approval)
- [x] Stakeholders with sealed identity; audited per-field identity reads
- [x] Grants: draft, edit, submit, board approval with SoD, activate, cancel, terminate (cause and window), amend as a new version
- [x] Milestones and acceleration events (append-only), vesting view, exercisable quantity
- [x] Exercises: request (reservations), approve, payment (exact amount), issue into the ledger; SoD at every step
- [x] Append-only ledger: issuance, transfer, cancellation, repurchase; balances never negative on any date; authorised limit
- [x] Cap table basic and fully diluted as of any date; ownership parts by largest remainder (property + fuzz)
- [x] Convertibles and valuations as sealed data; round model (illustrative, property + fuzz)
- [x] CSV exports with formula-injection protection, audited per record before output
- [x] Retention sweep, legal hold, erasure with tombstones and scrub, purge hook, key rotation with compare-and-swap
- [x] Audit hash chain with verification; outbox with signed, retrying dispatcher; periodic sweep (expiry, vested events)
- [x] Idempotency keys for grants, exercises and ledger entries
- [x] Triggers: append-only tables, frozen grant terms, lifecycle, SoD belt (002_immutability.sql)

## Phase 5: API, config, CLI
- [x] REST handlers, strict decoding, size limit, deadlines, security headers, `Routes()` for the contract test
- [x] Static-token and introspection authenticators (person id carried; fail closed; short cache); auth-first rate limiting
- [x] Config (strict, validated; ledger and audit stay `legal`), secrets from env, `can-equity` commands

## Phase 6: Contracts
- [x] `schemas/events/equity.*.v1.json` (3) with valid and invalid fixtures; `schemas/capabilities/equity.v1.json` with fixtures
- [x] OpenAPI 3.1 document; contract test comparing it with the registered routes
- [x] Contract test: every event emitted by an end-to-end flow validates against its schema
- [x] `modules/equity/module.yaml` (in-house, sensitive classes with retention, approvals, `experimental`), settings schema,
      chart values, events copies; manifest and code data classes agree
- [x] `deploy/equity/Dockerfile` fragment and README; erasure tombstones accepted by the E08 ledger (contract test)

## Phase 7: Docs and measurement
- [x] docs/equity.md, module README, epics and module tables
- [x] Idle RAM measured locally (measurements.md)

## Phase 8: Open (honest gaps)
- [ ] k3d integration test: image build, Secret keyring mount, read-only root filesystem, NetworkPolicy
- [ ] can-core: token introspection returning `person_id`, keyring Secret injection, approval-engine adapter (`ApprovalEngine`), event webhook route
- [ ] Helm templates (values only today); admin UI; employee and investor portals; `board` and `entity` modules
- [ ] Seal grant terms (needs a derived index for pool and dilution queries) and per-subject keys for crypto-shredding
- [ ] SAR cash settlement; partial and net exercise; sub-minor-unit strikes
- [ ] Jurisdiction rules (409A, ISO/NSO limits, tax, 83(b), ESOP schemes): deliberately not implemented; to be handled with counsel
- [ ] Captable import, PDF export, `equity.granted.v1` after signed agreement
