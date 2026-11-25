# Research: can-core MVP (E03 part 1)

Versions were resolved with `go get` on 2026-11-25 (`modernc.org/sqlite` v1.63.0, `golang.org/x/crypto` v0.59.0).

## R1. SQLite driver
- **Decision**: `modernc.org/sqlite`, pure Go, `CGO_ENABLED=0`.
- **Rationale**: SPEC fixes SQLite for can-core state. A pure-Go driver keeps the image a static distroless
  binary and CI free of a C toolchain. Measured cost: the binary is 13 MB stripped; RSS at start is 38 MB.
- **Alternatives**: `mattn/go-sqlite3` (CGO, needs gcc and a libc in the image); `crawshaw`/`zombiezen` (CGO).
- **Settings**: WAL, `busy_timeout=5000`, `foreign_keys=1`, one connection (`SetMaxOpenConns(1)`), so there is no
  writer contention and in-memory test databases behave like files. Migrations are embedded numbered `.sql` files
  applied in a transaction, forward only.

## R2. Password hashing
- **Decision**: argon2id from `golang.org/x/crypto/argon2`, PHC-format strings, parameters m=19 MiB, t=2, p=1
  (OWASP minimum), re-hash on login when policy tightens, constant-time compare, a dummy verification for unknown
  users so timing does not reveal them.
- **Rationale**: spec requirement; the memory parameter is chosen against the RAM budget (R9).
- **Alternatives**: bcrypt (72-byte limit, not memory hard); scrypt (fine, argon2id is the current recommendation).

## R3. TOTP
- **Decision**: own implementation of RFC 6238/4226 (HMAC-SHA1 by default for authenticator apps; SHA256/512
  supported and tested), six digits, 30 s step, one step of skew either side, constant-time comparison,
  single-use per step via `UPDATE ... WHERE totp_last_step < ?` (atomic replay guard).
- **Rationale**: about 60 lines; verified against all RFC 6238 Appendix B vectors for the three hashes.
- **Alternatives**: `pquerna/otp` (unmaintained since 2021), `xlzd/gotp` (small but another dependency in the auth path).

## R4. Sessions and CSRF
- **Decision**: server-side sessions. Cookie value is 256 random bits; only its SHA-256 is stored, so a database
  leak does not yield usable sessions. Cookie is `HttpOnly; SameSite=Strict; Secure` (Secure is configurable only
  for plain-HTTP dev). A per-session CSRF token is returned by login and required in `X-CSRF-Token` on unsafe methods.
  Login itself requires `Content-Type: application/json`, which a cross-site form cannot send.
- **Rationale**: simple, revocable (logout deletes the row), no JWT pitfalls.
- **Alternatives**: signed stateless cookies (cannot revoke); double-submit cookie CSRF (needs a second cookie).

## R5. Throttling
- **Decision**: counters per username (5 failures) and per client address (20 failures) in SQLite, 15-minute window
  and lock, persisted so restarts do not reset them. Locked keys are refused before credentials are evaluated.
  Client address comes from the last `X-Forwarded-For` entry only when `CAN_TRUST_PROXY=true`.
- **Rationale**: stops online guessing even with a leaked password (TOTP), survives restarts, and a distinct address
  limit stops username spraying.
- **Alternatives**: in-memory limiter (lost on restart); exponential lock-out (harder to reason about and to test).

## R6. TokenReview
- **Decision**: `tokenreview.Reviewer` interface; `Fake` for tests and dev; a `net/http` implementation of
  `POST /apis/authentication.k8s.io/v1/tokenreviews` (spec.audiences = `can`, requires the audience in the response
  and a `system:serviceaccount:<ns>:<name>` user). The module record is looked up by (namespace, service account).
- **Rationale**: SPEC 3.1. One endpoint does not justify client-go (about 20 MB of binary). Part 2 adds client-go for
  the installer; this can then be swapped without touching callers.
- **Risk**: only unit-tested against `httptest`; real API server behaviour is verified by the part-2 k3d test.
- **Alternatives**: client-go now (size, and no cluster here to test it).

## R7. Installer boundary and Helm
- **Decision**: `reconcile.ModuleInstaller{Enable(Spec), Disable(id)}`. `Enable` is idempotent install-or-upgrade
  and receives settings, namespace, ServiceAccount and the webhook HMAC secret; `Disable` scales to zero and keeps
  data. A `FakeInstaller` records calls; `RecordOnly` updates state without a cluster (used by `can apply --record-only`).
  The real implementation (Helm SDK, `--atomic`) is part 2.
- **Rationale**: lets the reconciler, state machine, ordering and failure handling be fully tested now.
- **Alternatives**: shelling out to `helm` (the image has no shell, SPEC 3.1); building the Helm code blind (cannot be tested here).
- **Decision**: `apply` never uninstalls. Removal from `company.yaml` disables; uninstall is an explicit operation
  with keep/purge (SPEC 4.2), a follow-up.

## R8. Webhook signing
- **Decision**: `X-Can-Signature: v1=hex(HMAC-SHA256(secret, "<unix ts>." + body))` with `X-Can-Timestamp`; receivers
  recompute and reject skew over 5 minutes (`webhook.Verify` is the reference implementation). `X-Can-Event-Id` is the
  idempotency key and is stable across retries. Redirects are not followed. Missing secret means no send.
- **Rationale**: the timestamp is covered by the MAC so it cannot be swapped to defeat the replay window; the
  scheme mirrors widely used webhook signing (Stripe-style) so module authors recognise it.
- **Retry policy**: 5 s doubling, capped 1 h, 8 attempts then `dead`; every attempt in `delivery_attempts`.

## R9. Footprint
- **Decision**: stdlib HTTP, `log/slog`, no framework. Soft memory limit 64 MiB by default. Argon2 memory is the
  OWASP minimum. Target 48 MB request / 96 MB limit. Measured 38 MB at start, 55 MB after Argon2 logins
  ([measurements.md](measurements.md)).
- **Open**: the 55 MB figure is the Go heap retaining Argon2's 19 MiB; returning it sooner (`debug.FreeOSMemory`
  after login) is a possible tweak if the CI footprint check (E00) flags it.

## R10. Audit chain
- **Decision**: record hash = SHA-256 over a domain tag and length-prefixed `seq, time, actor, action, target,
  detail, prev_hash`. Appends read the head and insert in one transaction on the single connection, so the chain
  cannot fork. `Verify` walks the chain and reports the first violation with a kind (`hash-mismatch`, `chain-break`,
  `seq-gap`, `head-mismatch`). Tail truncation cannot be seen from the chain alone, so `audit head` prints `seq:hash`
  to record elsewhere and `verify --head` checks it.
- **Honest limit**: someone with write access to the database file can rewrite the whole chain consistently;
  tamper-evidence against that needs the head anchored off the host (backup, email, external log). Documented, and
  the verify command makes anchoring a one-liner.
- **Alternatives**: per-row HMAC with a server key (the key would live next to the data); Merkle tree (more than needed).

## R11. Event bus
- **Decision**: in-process bus whose `Publish` validates the name (same regex as `module.v1`), checks the data is a
  JSON object under 256 KiB, writes event and one pending delivery per enabled subscriber in one transaction, runs
  in-process handlers (panics recovered), then wakes the dispatcher. The catalogue lists the initial SPEC events;
  well-formed unknown names are accepted so module authors are not blocked, and the module API additionally requires
  the type to be declared under the publisher's `publishes`.
- **Alternatives**: external broker (footprint, SPEC says SQLite outbox).

## R12. HTTP layer
- **Decision**: `net/http` `ServeMux` method patterns, RFC 9457 problem responses, a shared middleware (request id,
  panic recovery, security headers, structured access log that never logs `Authorization`/`Cookie`), JSON bodies
  only with unknown fields rejected and a 1 MiB cap. Two `http.Server`s with timeouts on separate listeners.
- **Contract test**: OpenAPI 3.1 files parsed in `test/contract`; served routes and documented routes must be the
  same set, `$ref`s must resolve, webhook headers must match the dispatcher's constants.
