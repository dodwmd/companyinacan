# Feature Specification: can-core MVP (Epic E03)

**Feature Branch**: `epic/E03a-can-core` (part 1 of E03)

**Created**: 2026-11-25

**Status**: Implemented for the platform core. Part 1 delivered stories 1-6 behind interfaces and fakes;
part 2 (merged) delivered stories 7-8 and the cluster lifecycle test, which runs in the advisory k3d CI job.
`email.send`, MariaDB provisioning, upstream-Helm-chart support and the admin enable/disable UI are still pending
(see docs/epics/README.md, E03).

**Input**: Epic E03 in `docs/epics/README.md`: "Go service in `can-system`; SQLite state; admin login
(password + TOTP); admin and module-API listeners/Services; capability registry; TokenReview-based
introspection; HMAC-signed webhooks to modules; `email.send`; audit log (hash-chained); Helm SDK rendering and
`helm upgrade --install --atomic` of module charts with values mapping; database creation and credential
injection; `can apply` reconciliation from the in-cluster `company.yaml`." SPEC.md sections 3.1, 3.3, 3.7, 4.2,
4.3, 4.5.

## Delivery map

| Story | Priority | Delivered by |
|---|---|---|
| US1 Admin signs in securely | P1 | **Part 1 (this PR)** |
| US2 Modules authenticate with their projected token | P1 | **Part 1** (TokenReview behind an interface; a small `net/http` implementation is included but only unit-tested) |
| US3 Capability registry | P1 | **Part 1** |
| US4 Tamper-evident audit log | P1 | **Part 1** |
| US5 Events reach subscribed modules, signed | P1 | **Part 1** |
| US6 `can apply` reconciles company.yaml with recorded state | P1 | **Part 1** (reconciler, plan, record-only mode; installer behind an interface with a fake) |
| US7 Real module deployment (chart install, namespaces, NetworkPolicies, Secrets, DB creation and credential injection) | P1 | **Part 2** (Postgres only; chart rendering is a Helm-compatible subset, research R16) |
| US8 `email.send` (SMTP relay wrapper) and `leads` surface for the admin UI | P2 | Part 2 (`leads` itself is E05) |

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Admin signs in securely (Priority: P1)

The operator logs in to `admin.<domain>` with a password and a TOTP code. TOTP is mandatory: there is no
password-only path. Repeated failures lock the account and the client address out.

**Why this priority**: can-core is internet-facing and highly privileged (SPEC section 3.1); this is its front door.

**Independent Test**: Bootstrap an admin, log in over HTTP with a valid code, then hammer it with bad attempts.

**Acceptance Scenarios**:

1. **Given** a bootstrapped admin, **When** a correct username, password and current TOTP code are posted, **Then**
   a session cookie (HttpOnly, SameSite=Strict, Secure) and a CSRF token are issued and an audit entry is written.
2. **Given** an unknown user, a wrong password, a wrong code or a code already used, **When** login is attempted,
   **Then** the response is the same 401 in every case and no information distinguishes them.
3. **Given** five failures for a (username, address) pair (or twenty from one address) within fifteen minutes, **When**
   the next attempt from that address arrives even with valid credentials, **Then** it is refused with 429 and
   `Retry-After` and the credentials are not evaluated; after the lock expires login works again. Failures from
   another address never lock the real admin out. There is deliberately no limit per account across addresses
   (residual risk: someone with the password can keep guessing codes, bounded only by speed when addresses are
   not trusted; [deployment.md](deployment.md)); twenty failures on one account in a day are reported in the audit log. More than ten attempts a minute from one address are refused before
   any hashing, and password hashing runs under a concurrency cap.
7. **Given** a locked-out admin or a lost authenticator, **Then** `can-core admin unlock` and
   `can-core admin reset-password [--rotate-totp]` recover access from inside the pod.
4. **Given** a logged-in session, **When** an unsafe request lacks the CSRF header, **Then** it is rejected with 403.
5. **Given** a TOTP code that was accepted once, **When** it is replayed within its validity window, **Then** it is rejected.
6. **Given** the stored password hash, **Then** it is argon2id with at least the OWASP minimum parameters.

### User Story 2 - Modules authenticate with their projected token (Priority: P1)

A module calls the module API with its projected ServiceAccount token (audience `can`). can-core validates it with
a TokenReview and maps the ServiceAccount namespace and name to the installed module record. Adapters verify
their callers with `POST /v1/tokens/introspect`.

**Acceptance Scenarios**:

1. **Given** a token for `mod-crm/crm` and an enabled `crm` record, **When** `GET /v1/self` is called, **Then** it
   returns that module.
2. **Given** a missing, forged, wrong-audience or unknown-ServiceAccount token, **Then** 401. **Given** a valid
   token of a disabled module, **Then** 403.
3. **Given** the TokenReview backend is unreachable, **Then** the API answers 502, never 401 (an outage must not
   look like bad credentials).
4. **Given** another module's token, **When** introspected, **Then** `{"active": true, "module_id": ...}`; for any
   token that is not an enabled module's, `{"active": false}` with no reason.

### User Story 3 - Capability registry (Priority: P1)

Providers register the capabilities in their manifest; consumers ask can-core who provides one and then call the
provider directly (can-core never proxies).

**Acceptance Scenarios**:

1. **Given** one enabled provider of an exclusive capability, **Then** it resolves to that provider's in-cluster URL.
2. **Given** two providers of an exclusive capability and no `primary`, **Then** resolution is a 409 conflict; with
   `primary: {contacts: espo}` it resolves to `espo`.
3. **Given** an aggregate capability (`reports`, `portal.view`), **Then** every enabled provider is returned.
4. **Given** a disabled or failed provider, **Then** it provides nothing.
5. **Given** a platform capability (`email.send`, `events`, ...), **Then** the provider is can-core itself.
6. **Given** a caller that did not list the capability under `requires.capabilities` or `consumes`, **Then** 403.

### User Story 4 - Tamper-evident audit log (Priority: P1)

Every administrative action (login, module install, upgrade, disable, failure) is appended to a hash-chained log.
`can-core audit verify` proves the log was not modified, reordered or (given a recorded head) truncated.

**Acceptance Scenarios**:

1. **Given** a log, **When** any record is modified, a record is deleted (with or without renumbering), two records
   are swapped, or a hash or `prev_hash` is forged, **Then** `audit verify` exits 1 and names the record and kind.
2. **Given** a recorded head `seq:hash`, **When** the tail of the log is removed, **Then** verification with
   `--head` fails.
3. **Given** concurrent writers, **Then** the chain never forks.

### User Story 5 - Events reach subscribed modules, signed (Priority: P1)

Modules publish events to can-core; can-core persists them to the SQLite outbox and delivers them at least once to
the modules whose manifest `subscribes` to the type, signed with a per-module HMAC secret.

**Acceptance Scenarios**:

1. **Given** a module that lists `lead.created.v1` under `publishes`, **When** it publishes, **Then** an event with a
   stable id is persisted and one delivery per enabled subscriber is queued. A module that did not declare the type
   gets 403; the `source` is always the authenticated module.
2. **Given** a delivery, **Then** the request carries `X-Can-Event-Id`, `X-Can-Timestamp` and
   `X-Can-Signature: v1=HMAC-SHA256(secret, "<ts>." + body)`, and a receiver can verify it and reject replays older
   than five minutes.
3. **Given** a failing subscriber, **Then** (delivery is at least once; receivers deduplicate on `X-Can-Event-Id`) deliveries are retried with exponential backoff (5 s doubling, capped
   at 1 h) up to 8 attempts, every attempt is recorded in the delivery log, and exhausted deliveries are `dead`.
4. **Given** a module with no signing secret, **Then** nothing is sent unsigned.
5. **Given** a disabled subscriber, **Then** nothing is queued for it.

### User Story 6 - `can apply` reconciles desired and recorded state (Priority: P1)

`can apply` validates `company.yaml` and the manifests of its enabled modules, compares them with the recorded
state and installs, upgrades, enables or disables modules through a `ModuleInstaller`.

**Acceptance Scenarios**:

1. **Given** a stub module enabled in `company.yaml`, **When** applied, **Then** the installer receives its settings,
   namespace, ServiceAccount and a freshly generated webhook secret; the module is recorded, its capabilities are
   registered and an audit entry is written.
2. **Given** nothing changed, **When** applied again, **Then** no installer call is made (idempotent).
3. **Given** the module is disabled or removed from `company.yaml`, **Then** it is disabled (data and signing secret
   kept), never uninstalled; re-enabling reuses the secret.
4. **Given** one module fails to install, **Then** it is recorded as `failed` with the reason, the others proceed,
   the command fails naming it, and the next apply retries it.
5. **Given** `--dry-run`, **Then** the plan is printed and no state, audit entry or installer call is made.
6. **Given** a module that requires a capability another module provides, **Then** the provider is installed first.
7. **Given** an invalid `company.yaml`, **Then** nothing is done and the validation problems are printed.

### Edge Cases

- The SQLite pool is one connection; handlers never hold a transaction while calling out.
- Clock changes: TOTP step replay is tracked by step number, session expiry by absolute time.
- `X-Forwarded-For` is trusted only when configured and only its last entry is used.
- A corrupted or foreign password hash fails closed with an error, never a successful login.
- `audit verify` never creates a missing database.

## Requirements *(mandatory)*

- **FR-001** can-core MUST run as one Go binary with two HTTP listeners on separate ports (admin, module API),
  `/healthz` on both, structured JSON logs that never contain credentials, configuration from `CAN_*` environment
  variables, and graceful shutdown on SIGTERM.
- **FR-002** State MUST be stored in SQLite with forward-only embedded migrations.
- **FR-003** Admin authentication MUST be argon2id password plus mandatory TOTP (RFC 6238, verified against the
  RFC test vectors), server-side sessions identified by a hashed random token, CSRF tokens on unsafe methods,
  throttling and lockout as in US1.
- **FR-004** Module identity MUST come from TokenReview of the projected token, behind a `Reviewer` interface with an
  in-memory fake.
- **FR-005** The registry MUST implement exclusive/aggregate/platform resolution and `primary:` as in SPEC 4.5.
- **FR-006** The audit log MUST be append-only by construction, hash-chained, and verifiable offline from the
  database file (`can-core audit verify|head|export`). Entries MUST hold ids, not personal data.
- **FR-013** Service names taken from manifests MUST be DNS-1123 labels and URLs built from them MUST round-trip (SSRF).
- **FR-007** Webhooks MUST be signed with HMAC-SHA256 over timestamp and body, delivered at least once with
  backoff and a delivery log, and never follow redirects.
- **FR-008** The event bus MUST validate versioned event names, persist before delivering, and match subscribers on
  manifest `subscribes`.
- **FR-009** Both HTTP APIs MUST be described by OpenAPI 3.1 files in `contracts/`, and a test MUST fail when a
  served route is undocumented or a documented route is not served.
- **FR-010** All Kubernetes and Helm work MUST be behind interfaces (`tokenreview.Reviewer`,
  `reconcile.ModuleInstaller`) so the whole behaviour is testable without a cluster (Constitution III).
- **FR-011** can-core's idle RAM MUST be documented as a target and measured (plan.md, measurements.md).
- **FR-012** Part 2 (`epic/E03b-k8s-integration`, tasks.md): real installer, namespaces/NetworkPolicies/Secrets,
  database creation, credential injection, in-cluster `company.yaml` ConfigMap, k3d integration test. Still open:
  `email.send`, admin enable/disable UI, uninstall (keep/purge), MariaDB.
- **FR-014** The installer MUST create `mod-<id>` from `deploy/templates`, give every module pod its own
  ServiceAccount with `automountServiceAccountToken: false` and only the projected `can`-audience token, inject
  `CAN_API_URL`, the webhook secret and database credentials from Secrets, refuse chart objects outside the module
  namespace or of kinds it owns, wait for readiness and roll back (or remove a failed first install) on failure, and
  MUST NOT contain any code path that deletes a PersistentVolumeClaim, Secret or Namespace.
- **FR-015** Disable MUST scale workloads to zero and suspend CronJobs, keep all data and Secrets, and a later enable
  MUST restore them.
- **FR-016** Database provisioning MUST be idempotent, give each module its own database and role, and revoke
  `CONNECT` from `PUBLIC` so one module's credentials do not open another's database.
- **FR-017** can-core's RoleBinding and Namespace creation MUST be constrained by a ValidatingAdmissionPolicy
  (`mod-*` only, `can-core-module` only, `restricted` pod security), proven on a real cluster.
- **FR-018** Token reviews MUST be cached (positive for at most 30 s and never past the token's `exp`, negative for
  10 s) and rate limited globally and per source; an overloaded limiter answers 429, an API outage 502.
- **FR-019** `can apply` MUST write the validated `company.yaml` to ConfigMap `can-system/company`; can-core MUST
  reconcile the mounted file on a timer and never apply an invalid file.

## Success Criteria

- **SC-001** `make test` is green with no cluster, network or container runtime.
- **SC-002** The stub-module lifecycle test (enable, injected config, token call, signed event, disable) passes
  in-process.
- **SC-003** Every tampering case in the audit tests is detected.
- **SC-004** Measured idle RSS of the stripped binary stays under the declared target (see measurements.md).
