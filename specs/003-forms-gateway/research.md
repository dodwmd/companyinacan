# Research: Forms Gateway (E05)

## R1. SQLite driver
- **Decision**: `modernc.org/sqlite` v1.63.0 (pure Go, no cgo).
- **Rationale**: The image is `distroless/static` and builds must be reproducible without a C
  toolchain (Constitution II, VI); the cgo alternative `mattn/go-sqlite3` needs libc in the image
  and a C compiler in CI. Licence: BSD-3-Clause for modernc packages (transitive deps: MIT,
  BSD-3-Clause, Apache-2.0), all on the accepted list. `can licences` covers wrapped apps in module
  manifests and is unaffected; there is no copyleft dependency.
- **Cost**: about 12 MB of binary growth and slower raw throughput than cgo SQLite. Leads arrive at
  human speed, so throughput is irrelevant; idle RSS was measured below the 15 MB target (R7).
- **Alternatives**: `mattn/go-sqlite3` (cgo, rejected); `ncruces/go-sqlite3` (WASM-based, smaller
  community, would pull wazero); `crawshaw.io/sqlite` (cgo); flat files (no queries, no migrations).
- **Configuration**: one connection (`SetMaxOpenConns(1)`) serialises writes and keeps memory
  small; WAL, `busy_timeout=5000`, `foreign_keys=1`, `synchronous=NORMAL`.

## R2. Routing and prefix stripping
- **Decision**: Hand-rolled router on the public listener that strips an optional configured
  prefix (`/_forms`), then accepts `/<id>`, `/<id>/token`, `/healthz`. The leads API uses the
  Go 1.22+ `http.ServeMux` method patterns on a separate listener.
- **Rationale**: Traefik strips the prefix in the cluster, but the service must also work when
  reached unstripped (local runs, other proxies). No router dependency needed.

## R3. Timing check
- **Decision**: Stateless HMAC token `base64url(unix-ts) "." base64url(HMAC-SHA256(key, form-id
  NUL ts))`, issued by `GET /_forms/<id>/token` and returned as hidden field `_t`. Valid for 24 h,
  rejected if younger than the form's `min_fill_seconds` (default 3).
- **Rationale**: Stateless, so no memory growth and no per-token storage; bound to the form id so
  a token cannot be reused across forms. Replay within the window is accepted by design: rate
  limiting, not the token, bounds volume.
- **Alternatives**: server-side nonce table (state, cleanup), JS-computed proof of work (hurts
  accessibility), no timing check (spec requires it).

## R4. Rate limiter
- **Decision**: Token bucket per client IP, `per_minute` capacity and refill, in a map capped at
  `max_tracked_ips`; on overflow, fully refilled buckets are dropped first, then arbitrary ones.
- **Rationale**: Constant memory regardless of attacker address diversity. Eviction of an active
  bucket only lets one address burst again, which is the safe direction. Client address comes from
  the TCP peer, or from the right-most `X-Forwarded-For` entry only when `trust_proxy` is set
  (left-most entries are client controlled and would let attackers evade or poison limits).

## R5. Captcha
- **Decision**: One `SiteVerify` implementation for both providers (same `siteverify` protocol,
  different URL) behind `CaptchaVerifier`. Verification failure is `400`; provider unreachable is
  `503` (fail closed) so a captcha outage cannot become a spam bypass. Off by default.

## R6. Email and webhook
- **Decision**: `Mailer` interface with an `HTTPMailer` posting JSON to the configured
  `email.send` endpoint with a bearer token. Webhook signature is
  `sha256=hex(HMAC-SHA256(secret, timestamp "." body))` with the timestamp in `X-Can-Timestamp`,
  following the common Stripe/GitHub shape so receivers can use ready-made verification patterns
  and replay protection. Notifications run on two workers behind a 256-entry queue; the lead is
  committed first, so a notification failure never loses data. Outcomes are stored in
  `lead_deliveries`.
- **Open question for E04**: the exact `email.send` request shape. The `Email` struct is the
  adapter point; changing the wire format is confined to `HTTPMailer`.

## R7. Footprint measurement (SPEC.md section 6.1: forms about 15 MB)
Static linux/amd64 build, `-trimpath -ldflags "-s -w"`, Go 1.27, 2026-11-25:

| Measure | Result |
|---|---|
| Binary | 12.2 MB |
| Idle RSS, after start | 12.2 MB |
| RSS after 200 token requests | 14.3 MB |

The manifest requests 15 MB with a 32 MB limit. The CI footprint harness (E00/E03) will
re-measure in a container.

## R8. Authentication
- **Decision**: `Authenticator` interface returning a `Principal` with permissions. Default
  implementation: static bearer tokens compared by SHA-256 digest with `ConstantTimeCompare`
  against every entry. Switching to can-core token introspection means one new implementation.
- **Rationale**: E04 (can-core) is not built yet; the interface keeps the API stable.

## R9. Event schema location
- `schemas/events/lead.created.v1.json` is the source of truth ("next to the capability schemas").
  The manifest's relative `publishes[].schema` path cannot leave the module directory, so
  `modules/forms/events/lead.created.v1.json` is a symlink to it; a contract test asserts the
  bytes match the embedded copy.

## R10. Config format
- **Decision**: YAML (already a project dependency), decoded with `KnownFields(true)`. Secrets
  are environment-only (Constitution V). Defaults applied before validation so errors name final
  values.
