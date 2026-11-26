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
- **Decision** (with `CAN_TRUST_PROXY`): counters per (username, client address) pair (5 failures) and per client address (20 failures) in
  SQLite, 15-minute window and lock, updated atomically in one transaction and persisted so restarts do not reset
  them. Keying the user lock on the address as well means a stranger cannot lock the real admin out by failing
  logins for `admin` from elsewhere (QA finding: lockout DoS). In front of that, every attempt counts against a
  per-address limit of 10 a minute **before** any hashing, usernames over 64 and passwords over 1024 characters never
  reach argon2 or create per-name state, and argon2 itself runs under a semaphore (default 1 slot, 3 s wait, then 503) so
  unauthenticated parallel logins cannot exhaust the 96 MB pod (each verify holds 19 MiB). One audit entry is written
  per lock, not per refused attempt; an audit failure fails a successful login closed (no session) and is logged on
  the failure paths. Expired counters are pruned hourly. Locked keys are refused before credentials are evaluated.
- **Recovery**: `can-core admin unlock [--username]` clears lockouts; `can-core admin reset-password [--rotate-totp]`
  sets a new password (and TOTP seed when the device is lost), ends all sessions and clears lockouts. Both run inside
  the pod (`kubectl exec`) and are audited.
- **QA pass 2 (default deployment)**: behind Traefik without `CAN_TRUST_PROXY` all clients share one address, so any
  refusal keyed on it is a lockout DoS. Throttling therefore has two modes (`auth.Policy.HardLock`, set from
  `CAN_TRUST_PROXY`): with trusted real addresses it refuses as described above; otherwise failures only add a delay
  (250 ms doubling to 2 s, at most 256 waiting logins) and never refuse. The TOTP code is checked before argon2, so
  junk logins cost no memory, and every login takes at least 150 ms so the failing step is not observable.
  Tests simulate all clients on one address while 500 junk logins for the admin's own username run, and require the
  admin to still log in. See [deployment.md](deployment.md).
- **Known limit, open**: in the shared-address mode an attacker who knows the password can guess TOTP codes at about
  128 a second or more, which defeats the second factor within hours. `CAN_TRUST_PROXY=true` plus the Traefik IP
  allowlist (SPEC 3.1) is the control; its locks are per address. Failed sign-ins per account are only reported
  (`admin.login.many_failures`, `auth/throttle.go`).
- **Per-account limit: tried and dropped.** Checking the password first once an account had many failures bounded the
  guesses, but each variant opened something worse: argon2 reachable without a valid code (a flood starves sign-in and
  keeps the owner out), and, to avoid that, a way to confirm a password without a code. A limit here needs something
  that tells the owner from a guesser before the password is checked (a known-device credential, say).
- **Counting before checking** (with the proxy): the (username, address) and address counters are taken before the
  credentials are compared, so requests sent together get no more than the limit, and given back on success, refusal or
  when nothing was compared.
- **Limits of the back-off (documented, accepted)**:
  - `maxSleepers` (256) bounds logins waiting out a back-off, not a burst of concurrent junk logins that all start at zero failure counters; those are bounded only by the HTTP server timeouts and connection limits.
  - `ErrBusy` (503) returns immediately and is not padded to the 150 ms floor; the timing difference reveals only that the server is saturated, a negligible oracle.
  - Failure counters restart when their 15-minute window ends (or an expired lock ends), so a slow attacker below the threshold per window is never delayed; the threshold applies per window, not cumulatively.
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

## R13. Service addresses (SSRF)
- **Decision**: `provides[].service`, `subscribes[].service` (and `runtime.ui.service`) are DNS-1123 labels (at most 63
  characters) in `module.v1`, with invalid fixtures. Independently, `registry.ServiceURL` re-validates service,
  namespace and port, builds the URL with `net/url` and rejects anything that does not round-trip to exactly
  `<service>.<namespace>.svc.cluster.local:<port>`; registration, reconciliation (before the installer runs), the
  event fan-out and capability resolution all go through it. A value such as `169.254.169.254#`, `evil.com/` or
  `x@evil.com` can therefore never redirect a signed webhook or a consumer call.
- **Why twice**: the schema stops it at validation time; the Go check protects can-core when a manifest reaches it
  by any other route (a future registry, a hand-edited database).

## R14. Delivery semantics and failed upgrades
- Deliveries to modules that are not enabled are skipped (they stay pending and resume if the module is re-enabled;
  they do not block outbox pruning). Dead deliveries can be requeued from the admin API
  (`POST /api/v1/deliveries/{id}/requeue`) with a fresh attempt budget. Delivery is **at least once**: receivers MUST
  deduplicate on `X-Can-Event-Id`, which is stable across retries and requeues.
- A failed **upgrade** of a module that was working keeps its recorded state, token identity, capabilities and
  subscriptions (Helm `--atomic` rolled the release back, so the previous version is still running); only
  `last_error` is set and the next apply retries. A failed first install is recorded as `failed`, provides nothing and
  is retried.

## R15. Transport and token-review follow-ups (documented, not built)
- The module API listener is plain HTTP inside the cluster. It is reachable only from module namespaces by
  NetworkPolicy and every call carries a bearer token that is valid only for audience `can`; a token leaked on the
  wire is usable against can-core until it rotates. mTLS or a service mesh is out of scope for v1 (SPEC 3.1 relies
  on NetworkPolicy); revisit with the k3d tests.
- Every module-API call performs a TokenReview. A negative cache and a per-source rate limit (so a misbehaving
  module cannot hammer the API server, and an outage degrades gracefully) are part-2 work, together with positive
  caching for the token's remaining lifetime.
- **Part 2 status**: the negative cache, positive cache (30 s, capped by the token's `exp`) and a global plus per-source
  rate limit are built (`tokenreview.Cached`); the module API maps a refused review to 429. Trade-off stated in the
  code: a token revoked by deleting its pod keeps working for up to the positive TTL. mTLS stays out of scope.

## R16. Helm SDK versus a small client (part 2 decision)
- **Measured** on the author's workstation (Go 1.27.3, CGO off, `-trimpath -s -w`), a program that only imports the
  package and reads its own RSS at start:

  | Imports | Binary | RSS at start |
  |---|---|---|
  | nothing (baseline) | 2.7 MB | about 4 MB |
  | `client-go/rest` + `client-go/dynamic` | 8.2 MB | 8 MB |
  | `helm.sh/helm/v3/pkg/engine` + `chart/loader` + client-go dynamic | 26.5 MB | 23.6 MB |
  | `helm.sh/helm/v3/pkg/action` + client-go rest | 28.7 MB | 27 MB |

  can-core today is 13.3 MB and 38 MB idle (measurements.md) against a declared 48 MB request / 96 MB limit. The
  Helm template engine alone would add about 15 MB of resident memory at start-up (scheme and sprig registration),
  the action package about 19 MB: that is the whole headroom, paid by every install forever, to save a few hundred
  lines.
- **Decision**: no Helm SDK and no client-go. `core/server/kube` is a net/http client for the 18 kinds the installer
  needs (server-side apply with a field manager, create, get, list, delete, merge-patch). `installer/chart.go` renders
  `chart/templates` with `text/template` and a Helm-compatible subset (`.Values`, `.Release`, `.Chart`, `include`,
  `define`, `toYaml`, `toJson`, `nindent`, `default`, `required`, `quote`, `trunc`, `b64enc`, ...). Atomic semantics
  are rebuilt: apply, wait for Deployment/StatefulSet readiness, and on failure re-apply the previous rendering
  (stored gzip+base64 in Secret `can-release`) or, for a first install, delete the new workloads and keep the data.
  Measured result: binary 16.96 MB (+3.7 MB), idle RSS 41.5 MB (+3.5 MB), both including the Postgres driver.
- **What this gives up, plainly**: charts that use `lookup`, `tpl`, `.Files`, `.Capabilities`, subcharts, hooks or
  library charts fail with an "undefined function" error instead of rendering. SPEC 3.1 expects upstream charts as
  subcharts, so wrapping an upstream chart needs either the full engine (open task T030f; the options are a
  build-tagged `can-core-helm` variant or rendering with `helm template` at image build time and baking the result in)
  or a module-owned minimal chart, which SPEC 4.1 already allows. The seam is `chart.RenderChart`.
- **Guard rails** (tests in `installer_test.go`): chart objects may not target another namespace, be a Namespace,
  RoleBinding or Pod, or reuse names can-core owns (`can-module`, `can-db`, `can-release`, the module ServiceAccount,
  the four policies, quota, limit range); pod templates may not use host namespaces, privileged containers, hostPath
  or another ServiceAccount; every pod template is rewritten to the module's ServiceAccount, no token automount and
  the projected `can` token. A chart can still add its own NetworkPolicies (additive); modules are reviewed repository
  code, and a hostile chart is out of the threat model for now.
- **Disable/enable**: disable merge-patches replicas to 0 and `suspend: true` and deletes running Jobs; the renderer
  always writes explicit `replicas` and `suspend`, so the next server-side apply (forced, same field manager)
  restores them. No code path deletes a PVC, Secret or Namespace (tested by scanning the fake's call log).
- **Alternatives**: Helm SDK in-process (rejected by the numbers); shelling out to `helm` (no shell in the image);
  client-go dynamic client alone (+4.5 MB binary, +4 MB RSS for what 300 lines do; reconsider with the Helm engine).

## R17. Admission policy for can-core's RoleBinding and Namespace creation
- **Decision**: two ValidatingAdmissionPolicies with Deny bindings (`deploy/base/admission-policy.yaml`), matched on
  `request.userInfo.username == system:serviceaccount:can-system:can-core` so administrators and k3s are untouched.
  RoleBindings: namespace `mod-*`, roleRef the ClusterRole `can-core-module`, one subject (can-core itself).
  Namespaces: name `mod-*`, `pod-security.kubernetes.io/enforce=restricted`, `can.dev/namespace-kind=module`, no
  `exempt` label. `failurePolicy: Fail`. Needs Kubernetes 1.30 (k3s v1.31.5 is pinned).
- **Why it is real**: RBAC grants can-core `create` on rolebindings cluster-wide plus `bind` on one ClusterRole, which
  without the policy lets a compromised can-core bind into `kube-system`. The k3d test impersonates can-core,
  attempts exactly that and requires the denial message to come from the policy (RBAC alone would have allowed it).
- **Still open**: the kube-system Role (`coredns-custom`, `HelmChartConfig/traefik`) is not covered; nor is a
  compromised can-core creating arbitrary workloads in `mod-*` (pod security `restricted` bounds them).

## R18. Hardening after QA (renderer sandbox, chart rules, integrity)
- **This is not Helm.** `chart.go` is a Go `text/template` renderer with a Helm-flavoured function set. Supported:
  `.Values`, `.Release.{Name,Namespace,Service}`, `.Chart.{Name,Version}`, `define`/`template`/`include`, `toYaml`,
  `toJson`, `indent`, `nindent`, `quote`, `squote`, `default`, `empty`, `required`, `fail`, `trim*`, `trunc`, `lower`,
  `upper`, `replace`, `contains`, `has{Prefix,Suffix,Key}`, `b64enc`, `toString`, `list`, `join`, `ternary`. Not supported
  (render fails): sprig beyond that list, `lookup`, `tpl`, `.Files`, `.Capabilities`, subcharts and dependencies, hooks,
  `helm test`, library charts, `NOTES.txt`, CRDs, JSON-schema validation of values. Release history is a single record
  (`can-release`), not Helm's revision list.
- **Sandbox limits** (tests in `hostile_test.go`): `include` nesting 20; any single function result 1 MiB; `indent` and
  `nindent` 0..1024 columns and a result bound; `replace` growth bound; one template file 1 MiB of output, a chart 8 MiB;
  chart files over 1 MiB are not read; 10 s wall-clock checked at each `include`; a panic while rendering is recovered.
  Reads refuse symlinks and anything that resolves outside the chart directory (`values.yaml`, `Chart.yaml`, the templates
  directory and every template); YAML errors never echo file contents. Not bounded: CPU spent in `range` over large
  values (values come from reviewed repository files and `company.yaml` settings, not from the chart).
- **Object rules.** Allowed kinds: ConfigMap, Secret (Opaque only, no `kubernetes.io/*` annotations), PersistentVolumeClaim,
  Service (ClusterIP only; no NodePort, LoadBalancer, ExternalName, externalIPs), Deployment, StatefulSet, Job, CronJob,
  NetworkPolicy, IngressRoute. Names must be DNS-1123. A NetworkPolicy may only select specific pods and allow
  same-namespace bare `podSelector` peers (the shape the starter charts use); allow-all, `ipBlock`, `namespaceSelector`,
  port-only and empty-peer rules are refused. An IngressRoute may only match the module's own host
  (`<runtime.subdomain>.<company domain>`, which can-core also injects as `.Values.ingress.host` last, so a values file
  cannot override it), may not use `HostRegexp`, and may not reference services or middlewares in other namespaces;
  Ingress and Middleware are can-core's. The starter charts (support, newsletter, analytics) are installed through the
  installer in `TestStarterModuleChartsStillInstall`.
- **Deletion and integrity.** Deletes use `propagationPolicy=Background`. Prune and rollback delete an object only after
  reading it back and finding `can.dev/module` and `managed-by=can-core`; PVCs, Secrets and Namespaces are never deleted.
  The release record carries a SHA-256 that is verified on load (a mismatch refuses the enable instead of replaying it),
  and every replayed object passes the same checks as a fresh render. A pre-existing namespace or RoleBinding (create
  returned a conflict) is adopted only if the labels (`restricted`, `module`, managed-by, module id) or roleRef and single
  subject are exactly what can-core would have created.
- **Admission.** The `exempt` check was removed (that label is not a pod-security input, so it checked nothing); the
  policy now requires a pinned `pod-security.kubernetes.io/enforce-version` (`v1.x`; the template pins `v1.31`, the k3s
  minor in `dev/k3d.yaml`). With `CAN_INSTALLER=kubernetes` the installer refuses to create anything until it can read
  both policies and sees a `Deny` binding for each (can-core has `get` only on those two names), so a cluster without
  `deploy/base` fails loudly instead of running with the standing RBAC alone.
- **TokenReview cache**: LRU eviction and singleflight, so a burst of identical tokens is one API call and junk tokens
  evict junk first.
- **MariaDB** is accepted by `module.v1` (`requires.database: mariadb`) but not provisioned: Enable fails with
  "database kind is not configured" before any workload is created.
- **In-cluster RSS, unexplained gap.** The CI run printed VmHWM 45.4 MB and VmRSS 25.1 MB for the can-core process after the
  whole integration run; the local idle figure is 41.5 MB (3 s after start). The HWM is the comparable number (peak, which
  includes startup and the Postgres/Argon2-free request paths); the 25 MB RSS was sampled minutes after the load had
  ended, so the Go runtime had very likely returned memory to the OS, but this is not verified. Neither figure is a
  steady-state-under-load measurement.
- **Evidence status.** The k3d lifecycle test runs in the `dev-cluster` workflow, which is `continue-on-error` and not a
  required check: it is advisory evidence, not a merge gate.

### R18 addendum (QA round 2)
- **Routing rules** are parsed, not pattern-matched (`installer/match.go`): one `&&` chain of plain matcher calls with
  backtick arguments and exactly one `Host(`<subdomain>.<domain>`)`; matcher names are case-insensitive and
  allowlisted (Host, PathPrefix, Path, Method, Headers); `||`, `!`, grouping parentheses, HostRegexp, HostSNI,
  HeaderRegexp, ClientIP, Query and anything else are refused. entryPoints are limited to `web` and `websecure`;
  service and middleware names may not contain `@` (other providers) and may not name another namespace.
- **Render CPU bound**: Go templates cannot be interrupted, so every `range` body (also inside `define`d templates and
  `values.tmpl.yaml`) is instrumented with a tick that counts iterations (200000 per render, `include` counts too) and
  checks the 10 s deadline and the caller's context; Enable additionally renders in a goroutine under a 30 s timeout.
  Test: a nested range over two 200k lists stops with an error, also when hidden behind `include`.
- **Pod templates** (`installer/pod.go`): volume types limited to configMap, secret, emptyDir, persistentVolumeClaim and
  the `can` projected token; Secret, ConfigMap and PVC references (volumes, `env.valueFrom`, `envFrom`) must name objects
  the chart defines, the Secrets can-core creates (`can-module`, `can-db`), or Secret names the chart declares under
  `.Values.secrets` (the starter charts' contract); the release record can never be read. hostPort, ephemeralContainers,
  shareProcessNamespace and the `serviceAccount` alias are refused; booleans are read robustly (`"true"`, `"True"`, 1).
  A chart's own `can-token` volume and mount (the starter charts declare one) is replaced by can-core's, never duplicated.
- **Claims and metadata**: PVCs and volumeClaimTemplates may not set `volumeName`, `selector`, `dataSource`,
  `dataSourceRef`, and `storageClassName` is empty or `local-path`; `apiVersion` must be the canonical one for the kind;
  `ownerReferences`, `finalizers`, `generateName` and `deletionTimestamp` are refused.
- **Release record** is authenticated with HMAC-SHA256 under the module's signing secret (kept in can-core's database,
  not in the cluster), so write access to Secrets in the namespace does not allow forging a record that rollback replays.
- **What the advisory CI run covers.** The `dev-cluster` workflow `integration` job (continue-on-error) runs the E02
  tests that are not gated, the admission tests, the stub-module lifecycle and the Postgres test. These tests are
  *skipped* there (`t.Skip`, gated by `CAN_PHASE0_PROOFS=1`): TestModuleTemplateApplies,
  TestModuleEgressDeniedToNodeAndAPI, TestModuleDNSRewriteFromModuleNamespace, TestModulesIsolatedFromEachOther,
  TestModuleReachesOnlyTheModuleAPIPort, TestTokenReviewAuthenticatesModule, TestCanCoreRBACScope,
  TestDiagnosePodToNodeEgress. They are slow and one fails on purpose while issue #13 is open, so the `footprint`
  workflow's `proofs-networkpolicy-tokenreview` job (also continue-on-error) runs them with the variable set; it passed on
  this PR's head. So module isolation, module-egress denial, TokenReview and RBAC scope are covered by that job, not by the
  `integration` job, and neither is a required check. Those proofs run against the E02 templates and not against
  namespaces created by the new installer; the lifecycle test is what exercises the installer's own namespaces.

### R18 addendum (QA round 3)
- `{{ template "x" }}` calls are instrumented like `range` and `include`, so an exponential chain of defines
  (45 levels, two calls each) hits the 200000-step budget in well under a second, and the abandoned render goroutine
  finishes because the budget, not a timer, ends it. At most one render per module runs at a time; a reconcile that finds
  one still running fails instead of starting another. Tests assert the time and that the goroutine count returns to baseline.
- Secret references in a pod template may only name chart-defined Secrets, `can-module`, `can-db`, and the two names
  can-core provides for the starter charts (`can-database`, `can-smtp`); names are no longer taken from chart values.
  A chart volume called `can-token` must be the projected `can` token. IngressRoute `tls` may only set
  `certResolver: letsencrypt` (no `domains`, `secretName`, `options`).

### R18 addendum (QA round 4): charts render in a child process
- **Why.** In-process limits cannot stop everything a template can do: a pointer-shared list DAG
  (`$l = list $l $l` forty times) serialises to 2^40 nodes through any printing path, and Go's
  "fatal error: out of memory" is not recoverable, so it would kill can-core. Budgets and caps stay as defence in depth
  (tick budget, string and output caps, a node cap in `toYaml`, `toJson`, `join`, `toString`, `quote`, `squote`, `print`,
  `println`, `printf`), but the boundary is a process.
- **How.** The installer re-executes its own binary (`os.Executable()`, so the image needs nothing extra) as the hidden
  `can-core render-chart` subcommand: job JSON on stdin (module directory, chart path, values file, settings, release),
  result JSON on stdout. Limits: clean environment (only `GOMAXPROCS`, `GOMEMLIMIT`), working directory a fresh temp
  directory, own process group killed on timeout or cancel, 15 s wall clock (the child also stops itself at 10 s), stdout
  capped at 16 MiB and stderr at 4 KiB, `RLIMIT_AS` set to the start-up address space plus 256 MiB, and a heap watchdog that
  exits the child above 32 MiB of live heap within about 5 ms. Any crash, kill, limit or missing output is a render error
  in the parent, which keeps running. If the child cannot be started the enable fails with "refusing to render in-process";
  there is no in-process fallback. The one-render-per-module guard stays.
- **Linux only** for `RLIMIT_AS` and the process group (`child_linux.go`); other platforms compile but have only the heap
  watchdog and the wall-clock kill. A fixed absolute address-space limit does not work for a Go binary (measured: the
  static binary fails to start at 512 MiB, starts at 1 GiB, and a limit set below the current size after start makes the
  next runtime allocation fatal), hence start-up size plus headroom.
- **Cost, measured** (workstation, stub and `support` charts, static binary): 6 ms wall per render and 15.6 MB peak RSS in
  the child, transient. The child's memory counts against the same container limit as can-core (96 MiB), so a render adds
  up to about 16 MB to can-core's 41 to 55 MB for a few milliseconds, and at most about 32 MB of heap before the watchdog
  fires; keep the limit at or above 96 MiB.
- **Tests** (`round4_test.go`, `limits_test.go`): the list DAG through toYaml, toJson, join, toString, printf, print, quote
  and squote is a render error (guard); through `{{ $l }}`, `html` and `urlquery`, which have no guard, the child dies at
  the heap limit (exit 5) and the test process, which stands in for can-core, survives with bounded peak RSS and still
  installs a normal chart afterwards; a missing renderer binary fails clearly before the cluster is touched; goroutines
  return to baseline; the 45-define chain still fails in under 2 s; the starter charts still install; the production limits
  work both with and without the race detector.
