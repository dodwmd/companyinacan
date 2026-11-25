# Feature Specification: Server Providers and `make up` (Epic E06)

**Feature Branch**: `epic/E06-providers`

**Created**: 2026-11-25

**Status**: Implemented offline; real-server verification pending (see [measurements.md](measurements.md))

**Input**: Epic E06 in `docs/epics/README.md`: "One command to a live site on a real server. `Provider`
interface; `ssh` provider (bootstrap script: k3s install with pinned version and trimmed flags,
kubelet swap flag, host firewall incl. pod/service CIDR allows, image pre-seeding); `hetzner`
provider; cloud-init path; sslip.io instant URLs with the HTTP fallback; DNS plugin interface with
`manual` (prints records); `make up|down|status`; `can` reaches the k3s API over an SSH tunnel."

This feature covers the part of E06 that can be built and verified without a real server, cloud
account or cluster. Everything that needs one is listed in [measurements.md](measurements.md) as
unverified, and the five-minute timing remains the human-gated E00 measurement.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Bring my own server (Priority: P1)

An operator with any Ubuntu/Debian VPS reachable over SSH writes `provider: {type: ssh, options:
{host: ...}}` in `company.yaml` and runs `make up`. The tool checks the host, installs a pinned k3s
with the footprint-trimming flags, configures swap and the host firewall, retrieves a kubeconfig,
and prints the instant URL and the DNS records to add.

**Why this priority**: `ssh` covers every provider on day one (SPEC.md section 3.5) and is the
basis for the Hetzner path.

**Independent Test**: Run `can up` against a fake `Runner`: preflight, bootstrap upload and
execution, kubeconfig fetch and the printed summary are all asserted without a network.

**Acceptance Scenarios**:

1. **Given** a host that passes preflight, **When** `can up` runs, **Then** the generated bootstrap
   script is uploaded and executed, state records the pinned k3s version, a kubeconfig pointing at
   the local tunnel port is written (mode 0600), and the instant URL and DNS records are printed.
2. **Given** a host with too little RAM or disk, an unsupported OS, no systemd, a non-root user
   without sudo, or busy ports 80/443/6443, **When** `can up` runs, **Then** it exits 1 naming each
   failed check and changes nothing on the host or in state.
3. **Given** a host already bootstrapped at the pinned version, **When** `can up` runs again,
   **Then** no bootstrap is uploaded (idempotent) and the exit code is 0.
4. **Given** `can up --plan`, **Then** the planned actions are printed, the host is not contacted,
   no state is written, and the exit code is 0.

---

### User Story 2 - Create a Hetzner server for me (Priority: P1)

An operator sets `provider: {type: hetzner}` and an API token (environment variable or a file
reference). `make up` uploads their SSH key, creates a firewall and a server with cloud-init that
bootstraps k3s, waits for it, and continues like the `ssh` path.

**Independent Test**: The API client runs against `httptest` servers serving recorded-style fixtures;
SSH is faked.

**Acceptance Scenarios**:

1. **Given** a valid token, **When** `can up` runs, **Then** key, firewall (22/80/443/icmp, never
   6443) and server are created once each, ids are saved in `.can/state.json` after each step, and
   the run waits for the cloud-init completion marker.
2. **Given** a run interrupted after the server was created, **When** `can up` is re-run, **Then**
   the existing server is adopted by name or id and nothing is created twice.
3. **Given** `can down --yes`, **Then** server, firewall and (only if `can` uploaded it) SSH key are
   deleted, a firewall still attached is retried, and a second `down` is a no-op.
4. **Given** rate limiting (HTTP 429) **Then** reads and writes are retried with backoff; a 5xx on
   a POST is **not** retried (it could create a second server).
5. **Given** any failure, **Then** the token never appears in stdout, stderr, JSON output, errors or
   state, even if the API echoes it back.

---

### User Story 3 - Instant URL that does not depend on DNS (Priority: P2)

After `make up` the operator gets a working address before DNS exists: `https://<ip-dashed>.sslip.io`
(plus `admin.` and `help.` names), or, when sslip.io is unreachable or Let's Encrypt limits would be
exceeded, a plain-HTTP URL on the IP serving only the public site, with the admin UI reachable only
through the SSH tunnel.

**Acceptance Scenarios**:

1. **Given** sslip.io resolves the name to the server, **Then** the plan is `https` with one
   hostname per certificate.
2. **Given** sslip.io does not resolve or resolves elsewhere, 4 certificates for the same names in 7
   days (limit 5), or 3 failed issuances in the last hour (limit 5), or a private/loopback address,
   **Then** the plan is the HTTP fallback with the reason, and the admin UI is not advertised.
3. **Given** the custom domain, **Then** the `manual` DNS plugin prints the A/AAAA records (apex,
   `admin`, `help`) and states that propagation is outside the five-minute budget.

---

### User Story 4 - Safe lifecycle and CLI contract (Priority: P2)

`can up|down|status` and `make up|down|status` behave predictably: exit codes 0/1/2/3, `--plan` on
`up` and `down`, `down` refuses without `--yes`, `status` exits 0 only when ready, secrets are
redacted everywhere, concurrent runs cannot corrupt `.can/state.json`.

**Acceptance Scenarios**:

1. **Given** provider types `local`, `static`, `digitalocean`, **Then** `up|down|status` exit 3 naming
   the epic that delivers them and write nothing.
2. **Given** an invalid or missing `company.yaml`, **Then** exit 1 with the validator's problems.
3. **Given** a bad flag or too many arguments, **Then** exit 2.
4. **Given** two processes updating state at once, **Then** no update is lost (file lock).

## Requirements *(mandatory)*

- **FR-001** A `Provider` interface with `Plan`, `Create`, `Destroy`, `Status`, `Address`; `Plan`,
  `Status`, `Address` are read-only; `Create` and `Destroy` are idempotent.
- **FR-002** State lives in `.can/state.json` (mode 0600), updated atomically under an exclusive
  file lock, holds no secrets, and is versioned.
- **FR-003** Provider types: `ssh`, `hetzner` implemented; `local` (E02), `static` (E18),
  `digitalocean` (E12) are explicit stubs returning "not implemented" (exit 3).
- **FR-004** `ssh` preflight checks OS (Ubuntu/Debian), architecture, RAM, free disk, systemd, root
  or sudo, and free ports 80/443/6443; a re-run on a bootstrapped host tolerates busy ports.
- **FR-005** The bootstrap script is generated text: pinned k3s version, config-file flags
  (`disable: metrics-server`, `disable-cloud-controller`, `secrets-encryption`,
  `kubelet-arg: fail-swap-on=false`), swap file on hosts under 4 GB, default-deny host firewall
  allowing 22 (optionally operator-restricted), 80, 443 and the pod/service CIDRs and CNI
  interfaces, never 6443; image tarball pre-seeding with SHA-256 verification in parallel with the
  k3s install; `/etc/can/backup.d` and `/var/lib/can/dumps` placeholders for E08; completion marker.
- **FR-006** Every value interpolated into the script or an SSH command line is allow-list validated;
  injection attempts are rejected.
- **FR-007** The same script is deliverable as `#cloud-config` user-data (base64 `write_files`),
  within the 32 KiB limit.
- **FR-008** Hetzner client over the standard library HTTP client: SSH key, firewall, server
  create/get/find/delete, retry on 429, token only in the `Authorization` header.
- **FR-009** Tokens come from `env:NAME` or `file:PATH` references; every resolved secret is
  registered with a redactor applied to all progress output, errors and JSON.
- **FR-010** `can` reaches the k3s API only through an SSH tunnel bound to 127.0.0.1; the retrieved
  kubeconfig is rewritten to the tunnel port and stored 0600.
- **FR-011** DNS plugin interface; `manual` prints records; `cloudflare` and `provider-native`
  report not implemented (E12).
- **FR-012** `can up [--plan] [--dns]`, `can down [--plan] [--yes]`, `can status`, Makefile targets
  `up|down|status`, exit codes 0/1/2/3, `--output json`.
- **FR-013** `docs/providers.md` and `docs/make-up.md`, including the human-gated timing procedure.

## Out of Scope

Deploying can-core, Traefik ACME configuration and the site (E03/E04); issuing certificates (the
history hook `RecordIssuance` is provided for E03); backups and recovery kit (E08); `can dev` (E02);
Cloudflare DNS, DigitalOcean (E12); pre-baked provider snapshots; the actual five-minute
measurement (E00, human-gated).

## Success Criteria

- **SC-001** `make test` passes with networking disabled; no test contacts a real host or API.
- **SC-002** The generated bootstrap passes `bash -n` and `shellcheck --severity=warning` (when
  shellcheck is installed) and is covered by golden files.
- **SC-003** No test output, error or state file contains a registered secret (asserted through the
  CLI for text and JSON output).
- **SC-004** Re-running `can up` makes zero create calls and uploads no bootstrap.
