# Implementation Plan: Board and entity modules (E17b)

**Branch**: `epic/E17b-board-entity` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

> Tool, not legal advice. Quorum, notice and voting law, register content and filing rules are not implemented
> (data fields and documented limits only).

## Summary

One in-house Go service, `cmd/can-board` over library `core/board`, hosting two modules (`board`, `entity`),
built like `can-equity`: SQLite with embedded migrations, an `Authenticator` seam, a REST JSON API on a
ClusterIP-only listener, an outbox for events, two manifests, values-only charts, one Dockerfile fragment.
Authorisation is one pure function (`Decide`/`FieldAccess`) over role × relationship facts that the service
resolves from the store; separation of duties is one function (`sodOK`) repeated as triggers where the database
can hold the rule; quorum and tally are pure integer functions with property and fuzz tests.

## Technical Context

**Language/Version**: Go 1.27. **Dependencies**: stdlib, `modernc.org/sqlite`, `golang.org/x/crypto`,
`golang.org/x/text` (NFKC, case folding), `go.yaml.in/yaml/v3`; all already in `go.mod`; no new ones.
**Storage**: SQLite (WAL, `secure_delete`, one connection). **Testing**: `go test` with `-race`, a fuzz target
for quorum, property tests for tallies, contract tests in `test/contract`. **Target**: Linux container, distroless
static. **Constraints**: idle RAM 20 MB target (measured 14.4 MiB), limit 64 MB; no cgo; fail closed.

## Constitution Check (before research)

Checked against `.specify/memory/constitution.md`.

| Principle | Assessment |
|---|---|
| I Spec-first | Refines SPEC.md 3.7, 4.3 and the *Board & governance* / *Entity & legal calendar* rows. Divergences: the catalogue names `resolution.passed.v1`; this slice publishes `board.resolution.passed.v1` (namespaced like `equity.*`), plus `board.meeting.scheduled.v1` and `entity.filing.due.v1` (the reminder SPEC.md asks of `entity`); trademarks and domains are not given dedicated records (they can be obligations). The manifest approval type is `resolution` because the schema pattern forbids a dot; its qualified name is `board.resolution`. |
| II Footprint | 20 MB / 64 MB declared in both manifests and the chart; measured idle 14.4 MiB (`measurements.md`); one process serves both modules; no sidecar. |
| III Test-first | The policy matrix and the quorum/tally tests were written first and observed failing (the package did not compile: undefined `Decide`, `QuorumRequired`, `TallyPasses`) before `policy.go` and `model.go` existed. The service tests followed each slice. Schemas have valid and invalid fixtures; routes are checked against OpenAPI. k3d integration tests are pending. |
| IV Stable contracts | Capabilities `board.v1`, `resolutions.v1`, `entity.v1` and three event schemas with fixtures; modules consume ids, never internals. |
| V Secure and private | In-house only; board materials are confidential: 404 for the invisible, per-field access, audited reads (fail closed), sealed columns with AAD and rotation, retention giving way to the legal record, legal hold and erasure delivered with the first module that stores them; ClusterIP only; restricted security context; disclaimer present. |
| VI Open-source hygiene | Apache-2.0; no new dependencies; fake data only. |
| VII Simplicity and measured claims | Boring stack; RAM measured, not asserted; quorum and thresholds are simple documented formulas on data; complexity tracked below. |

## Project Structure

```text
cmd/can-board/main.go             entry point
core/board/
  policy.go                       the single authorisation layer (pure) and sodOK
  model.go                        errors, QuorumRequired, TallyPasses (exact integers), validators, canonSubject
  bodies.go meetings.go           bodies and members, facts; meetings, agenda, attendance, quorum, pack
  resolutions.go                  resolutions, versions, conflicts, voting, consent, secret ballot, the seam, ApplyDue
  minutes.go                      minutes versions, action items
  entity.go filings.go            legal entities, contacts, registers, CSV; obligations, filings, reminders
  retention.go                    classes, retention, erasure, legal hold, purge hook, key rotation
  service.go crypto.go audit.go events.go ratelimit.go   Service, idempotency; keyring and AEAD; audit hook and chain; outbox
  api.go routes.go auth.go config.go cmd.go              REST surface, authenticators, config, CLI
  migrations/001_board.sql 002_entity.sql 003_immutability.sql
modules/board/ modules/entity/    module.yaml, settings schema, chart values, events copies, README
deploy/board/                     Dockerfile fragment, README
schemas/capabilities/{board,resolutions,entity}.v1.json
schemas/events/{board.meeting.scheduled,board.resolution.passed,entity.filing.due}.v1.json   + testdata/ fixtures
specs/029-board-entity/           this directory
docs/board.md docs/entity.md
test/contract/board_test.go
```

## Design decisions

1. **One policy function, one SoD function.** Facts (member, chair, attended, invited, observer, recused,
   subject, proposer, drafter, self) are resolved from the store; the policy only decides. The `*.read` actions
   double as the visibility test, so 404 vs 403 follows from the same table.
2. **404 before 403** for every record type; lists are filtered; a malformed id is a 404.
3. **Audit before data**, per record, atomically (`RecordBatch`); a failing auditor means no data; writes audit in
   the same transaction (`txAudit`).
4. **Database-level guarantees** in `003_immutability.sql`: append-only audit, registers, votes, ballots, receipts,
   versions; frozen decided resolutions (the tally is final), approved minutes, terms and contacts; forward-only
   status; the voter list fixed when voting opens; votes only while voting.
5. **Exact integers** for quorum (`ceil(eligible × num / den)`, at least the minimum, at least one) and tallies
   (`for × den` against `num × (for + against)`, strict or inclusive); abstentions never change the outcome.
6. **Voter list snapshot** at open: members in term minus subjects and the recused; eligible count and quorum
   figures are stored on the resolution.
7. **Secret ballot** = receipts (who voted, as a hash) + ballots (random id, sealed choice), no link, no time, no
   insertion order; per-voter reads refused for every role; a pass needs two "for" ballots (conservative: the
   proposer cannot be told apart).
8. **Integrity hash** for resolution versions and minutes (sha256 over id and content, frozen by trigger), checked
   on open, so a swapped or edited ciphertext is detected; key rotation may rewrite only the ciphertext.
9. **Registers replay** on every write: validation by re-running the register with the new entry, so backdated
   entries are checked against every later date; as-of by effective date, known-at by record date.
10. **Erasure pseudonymises, never deletes the legal record**: the board stores opaque person ids; erasure clears
    sealed interest text, addresses and dates of birth; terms, votes, attendance, minutes and register entries
    stay.
11. **The approval seam is a capability, not a code change in equity**: `resolutions` (request, outcome) plus the
    event; the `integration` role holds exactly those verbs.
12. **One SQLite connection** serialises writes and keeps RAM low.

## Complexity Tracking

| Addition | Why | Simpler alternative rejected because |
|---|---|---|
| Triggers duplicating service rules | Votes, tallies, minutes and registers are the legal record; a service bug must not break them | Service-only checks leave a single point of failure |
| Hash-chained audit table in the module | The hook must work before can-core's audit accepts module events | A plain log is easier to edit silently; the hook lets can-core replace it |
| Receipts/ballots split for secret ballots | A vote table with a voter column cannot be anonymous | Storing the voter beside a sealed choice still lets the key holder link them |
| Content hashes next to sealed text | Detect swapped ciphertext and enforce immutability by trigger without reading plaintext | AAD alone does not stop a valid ciphertext of the same row being replaced by an older one |
| `integration` role | The service account must ask for a resolution and read its outcome without reading any board content | Giving can-core the secretary role would hand it every board paper |
| Two manifests, one binary | Operators enable governance and company records separately; roles and data classes differ | One manifest would force `entity` data classes and roles onto operators who only want board packs |

## Constitution Check (after design)

No principle is violated; no new dependency. Remaining risks: Principle III integration tests on k3d, the
footprint on a cluster, and the gaps in `tasks.md` (notably the unwired equity adapter and the single-release
deployment of two manifests).
