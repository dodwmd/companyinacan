# Company in a Can — Project Spec

> Working name: **Company in a Can** (`can`).

## 1. Vision

A self-hosted "business control panel" — think Webmin, but for business people instead of sysadmins. Given a company idea, you run **`make up`** and about **five minutes later** have a live HTTPS landing page, working contact forms and a support inbox (UI live; mail flow needs a relay, see §3.4), on a single small server at the hosting provider of your choice. Everything else (CRM, wiki, newsletter, analytics, …) is one `make enable <module>` away.

Development happens locally on a k3s cluster (`make dev`), using exactly the same manifests that production uses.

**This is an open-source project**, built first for the author's own use. Commercial offerings (hosted/managed tiers, support) may come later *if the project proves useful*; nothing in v1 is designed around monetisation, tiers or pricing.

Every capability is a **module**. Modules are switched on and off individually. Initially each module wraps an existing open-source app; over time individual modules are replaced by in-house implementations without changing how users or other modules interact with them.

### Goals

1. **Five-minute launch.** `make up` → live site, forms and support inbox UI in ≤ 5 minutes on a fresh server (budget in §3.4). Connecting real email is a separate, documented step.
2. **Small footprint.** Footprint is a first-class constraint: the platform has an explicit RAM budget (§3.2), every module declares and is tested against its own, and the starter stack targets a 2 GB server (tight; 4 GB is the recommended default until Phase 0 proves otherwise).
3. **Provider-portable.** Any provider that can give us a Linux VM with SSH works; a few (Hetzner, DigitalOcean, …) get fully automated provisioning (§3.5).
4. **Local = production.** `make dev` runs the same charts on a local k3s (k3d) cluster.
5. **Modular.** Pick only what you need. Adding or removing a module never breaks the others.
6. **One front door.** One admin UI, one login (SSO from Phase 2; can-core's own local login before that), one domain scheme, one backup, one update flow.
7. **Replaceable internals.** OSS apps today, in-house tomorrow, behind stable module contracts.
8. **Multi-company.** One operator can manage several companies from the same tooling; each company is its own cluster on its own host (§6.5).

### Non-goals (v1)

- Multi-tenant SaaS hosting for third parties, paid tiers, billing for the platform itself (possible later, out of scope now).
- Multi-node / HA Kubernetes. We use k3s as a **single-node** appliance, not as a cluster to operate.
- Building apps from scratch where a suitable lightweight OSS app exists. The people/finance lifecycle modules (`people`, `jobs`, `vendors`, `contracts`, `performance`, `expenses`, `entity`, `board`, `policies`, `invoices`, `time`, `equity` and the lean `work`) are the deliberate exception: they hold the most sensitive data, must share one party model, and have no light OSS fit (or, for `time` and `invoices`, no one that passes the licence/footprint check; they wrap an OSS app where one does). They stay small, and Phase 3 is split to keep the scope honest (§9).
- Replacing accounting/legal professionals — we integrate, we don't certify.

### Target users

- **Primary:** a solo founder / small team with several company ideas who wants to spin up an online presence quickly and repeatedly.
- **Secondary:** non-technical business owners who get a pre-installed instance.

## 2. Principles

- **One command.** The interface is `make up` / `make dev` / `make down`; the Makefile is a thin wrapper over the `can` CLI. A new company needs three values (name, domain, provider); a backup bucket and email-relay credentials are optional extras requested at init and can be added later.
- **Lean by default.** Every component has a RAM budget; CI measures it and fails on regressions. Nothing runs unless it's enabled (databases included: each engine is deployed only when an enabled module needs it).
- **Static first.** The public site is static files served by a tiny static server (or a free CDN host). Dynamic features are separate services reached by forms/embeds.
- **Kubernetes as an implementation detail.** k3s single-node gives declarative deploys, health checks, rollbacks, network policies and secrets, and makes local = production. Users never need to touch `kubectl`.
- **Config as data.** The company is described by one `company.yaml` plus per-module config; the configuration is reproducible and git-friendly (see §3.3).
- **Own your data.** Everything exportable; no vendor lock-in; modules use open formats.
- **Open source first.** Public repo, permissive licence, contribution-friendly docs from day one. Keep the core free of anything that would block a future commercial layer being built *around* it (see §12).
- **Licence hygiene.** Track each module's licence (many OSS apps are AGPL/GPL). Wrapping and running unmodified upstream images is fine; embedding or forking has consequences. Record this per module and ship a generated `THIRD_PARTY_LICENSES` file.

## 3. Architecture

```
  make up ──► can CLI ──► provider API (create VM) ──► cloud-init / SSH
                                                          │
                                       ┌──────────────────▼───────────────────┐
                                       │  single-node k3s (SQLite datastore)  │
                                       │                                      │
   Internet ──► :80/:443 ──► Traefik ──┼─► site (static)   forms   support …  │
                (ACME TLS)             │   admin.<domain> ► can-core          │
                                       │                      │               │
                                       │      can-data: Postgres · MariaDB    │
                                       └──────────────────────────────────────┘
   make dev  ──► same charts on local k3d (k3s in Docker), *.localtest.me
```

### 3.1 Components

| Component | Purpose |
|---|---|
| **`can` CLI / Makefile** | Runs on the operator's machine. Provisions the server through a provider plugin, bootstraps k3s, installs can-core, applies `company.yaml`. Also drives `make dev`. |
| **can-core** | Control plane, runs **in the cluster** (`can-system` namespace) as a small Go service. Admin UI + API, renders module charts, manages lifecycle, health, backups, upgrades, and hosts the capability registry and event bus (§4.3). |
| **Ingress** | Traefik (bundled with k3s) with built-in ACME for TLS. Routes `example.com` → site, `help.example.com` → support, `admin.example.com` → can-core, etc. Only component exposed on host ports 80/443. We avoid cert-manager to save RAM. |
| **Site** | A built-in, can-core-managed component (it still ships a `module.yaml`, but deploys into `can-system` instead of its own namespace). Static HTML rendered from templates + `company.yaml` by a renderer **library** shared by the `can` CLI and can-core (no Node toolchain). can-core writes the output to a small RWO PVC that a ~5 MB static server pod, running in `can-system` next to can-core (PVCs are namespaced), serves. The same output can be deployed to a free static host by the CLI (the `static` provider: site only, with an external form endpoint or webhook instead of the forms gateway). |
| **Forms gateway** | Tiny in-house Go service that accepts form posts from the static site at the same-origin path `/_forms` (a Traefik route with higher priority than the site's catch-all, prefix stripped before it reaches the gateway; the same on sslip.io and custom domains), spam-filters (honeypot, timing check, optional Turnstile/hCaptcha), stores in SQLite (exposed through the `leads` capability), emails, and publishes `lead.created.v1`. Works without Cloudflare. |
| **SSO** | One identity provider so staff log in once. Default: Authelia (OIDC **and** Traefik forward-auth, which covers apps that can't speak OIDC). Pocket ID is OIDC-only and can't provide the forward-auth fallback. Phase 2; until then each module uses its own local login. |
| **Shared databases** | Postgres and/or MariaDB StatefulSets in `can-data`, **each deployed only when an enabled module requires it** (can-core and the forms gateway use SQLite). One database + user per module. |
| **Event bus (light)** | Hosted inside can-core: an event log with webhook delivery (§4.3). |
| **Backup** | `restic` to cheap object storage (R2/B2/S3), run by a **host systemd timer** after can-core has produced consistent dumps (§6.5). |

**Networking and security.**
- Charts (our own plus vendored upstream subcharts) are embedded in the can-core image, so the cluster needs no chart repository at runtime. Hook scripts live in the module directory embedded in the can-core image; can-core mounts them (as a ConfigMap) into a Job running the module's own image, or a `hooks.image` the module declares when the upstream image lacks the needed shell or tools (the can-core image itself has no shell).
- Namespaces: `can-system` (can-core and the site pod), `can-portal` (the portal, Phase 3a; Traefik → portal, portal → `portal.view` providers and the module API only), `can-data` (databases), one `mod-<id>` per module (the forms gateway is a module like any other). Default-deny applies to `can-system`, `can-portal`, `can-data` and `mod-*`, **not** `kube-system` (Traefik, CoreDNS). A default-deny NetworkPolicy (ingress, plus a per-module-namespace egress policy) is applied in each of those namespaces (k3s enforces policies out of the box via its embedded kube-router controller) with explicit allows: Traefik (in `kube-system`, matched by namespace selector) → the site pod, can-core's admin port and module UI/webhook ports; can-core → providers it consumes directly (e.g. the forms gateway's `leads` port); can-core → module webhook/health ports and → `can-data` (to create databases); modules → can-core's module API; each module → its database port; module → module only for declared `provides`/`consumes` pairs; portal → `portal.view` providers and the module API. Module egress policy allows DNS, can-core's module API, the module's database and the internet (SMTP/IMAP, API calls) and declares a deny (an `ipBlock` `except`) for the node IP, the cluster CIDRs and the link-local range `169.254.0.0/16` (the cloud metadata endpoint serves cloud-init user-data on most providers). **That deny is not effective for the node address on k3s** (Phase 0, issue #13): a pod still reached the node's API server and kubelet ports under a pure default-deny. can-core's admin port is protected by the `can-system` ingress policy, which does work. Link-local and RFC1918 egress are not yet tested. Module pods set `automountServiceAccountToken: false` and receive only the projected `can`-audience token volume. Because modules cannot use the loopback-resolved public name (`localtest.me` or the sslip.io address resolves to the node, and the node address is not a route to Traefik for a pod), they reach public hostnames of this cluster (the IdP at `auth.<domain>`, their own public webhook URLs) through a CoreDNS rewrite of only the hostnames modules need (`auth.<domain>` and each module's public webhook host, never `admin.<domain>`) to the Traefik Service; as defence in depth the admin router also has a Traefik middleware that rejects pod-CIDR sources (a `*.override` snippet in the supported `coredns-custom` ConfigMap, since k3s re-applies its own CoreDNS manifest and reverts edits to `coredns`; can-core updates it when the domain moves from sslip.io to a custom one), with an egress allow to Traefik, in production as in dev (§3.6). Phase 0 found that kube-router does NOT enforce pod-to-node egress denial (tested on k3d only, one k3s/kube-router version, no ufw); whether this path works with `externalTrafficPolicy: Local` is still untested. Reachability is not exploitability: with `automountServiceAccountToken: false`, RBAC on the API server and authentication on 6443/10250 (anonymous auth off) the reachable ports still need credentials, and those are the actual barrier until the design decision in §11 is made.
- can-core exposes two Services: an **admin** port (reachable only from Traefik; login + optional IP allowlist via Traefik middleware) and a **module-API** port (reachable only from module namespaces; carries the capability registry, `email.send` and event publish) — extended in Phase 2/3 with `can.check`, approvals, notify and search push.
- Module identity toward can-core and other modules is the pod's **projected ServiceAccount token** (audience `can`), validated with a Kubernetes TokenReview. For the reverse direction (can-core → module event webhooks) can-core signs each request with a per-module HMAC secret injected at enable time; adapters verify the signature header.
- **can-core is highly privileged.** To create namespaces, NetworkPolicies and Helm releases, read module Secrets, exec hooks and perform TokenReview it needs a broad ClusterRole, and it is internet-facing at `admin.<domain>`. This is accepted but mitigated: mandatory TOTP, optional IP allowlist, the narrowest ClusterRole that works, no shell in its image. Its privilege also includes a hostPath mount of the backup dumps directory (§6.5), so `can-system` is exempt from the `restricted` pod-security profile, and it is the first target of security review (§11).
- Secrets are Kubernetes Secrets with k3s secrets-encryption at rest enabled. can-core injects `CAN_API_URL`, database credentials and SMTP relay credentials into each module as environment from those Secrets.
- The bundled Traefik is configured through a `HelmChartConfig` (in `deploy/`) that adds the ACME resolver and **persists `acme.json`** on a PVC, so restarts don't re-issue certificates and hit Let's Encrypt rate limits. It also sets `externalTrafficPolicy: Local` on Traefik's Service so the real client IP survives ServiceLB (needed for the admin IP allowlist).
- Public webhooks (e.g. inbound-email parse) are received by a pod in the owning module, routed by Traefik on a public path and verified by a shared secret or signature. can-core's module API is never exposed publicly.
- The host firewall allows only 22 (restricted to the operator's IP where possible), 80 and 443. The bootstrap configures it on the host (ufw/nftables), since the `ssh` provider has no cloud firewall, and adds the allow rules k3s needs for pod-to-host traffic (pod CIDR 10.42.0.0/16, service CIDR 10.43.0.0/16, the CNI interface) so a default-deny host firewall doesn't break the API server, CoreDNS or kubelet; provider plugins additionally set the provider firewall. **Phase 0 (CI, k3d) found that NetworkPolicy does not cover pod-to-node traffic on k3s: a pod under default-deny still reached the node's API server and kubelet ports (issue #13, `docs/measurements/phase0-ci.md`; k3d only, one kube-router version, no ufw). Note the host firewall rules above allow the pod and service CIDRs in, so they do not close this either. A blanket host drop of pod-CIDR traffic to node:6443 is NOT a solution: system pods (CoreDNS, Traefik, can-core) reach the API through `10.43.0.1:443`, DNAT'd to node:6443 with a pod-CIDR source, and are indistinguishable from module pods by CIDR. How to isolate is an open design decision (§11).** The k3s API server (6443) is not exposed; the CLI reaches it through an SSH tunnel.

### 3.2 Footprint budget

Footprint is a design constraint, not an afterthought.

| Layer | Target (idle) | How |
|---|---|---|
| k3s server + Traefik + CoreDNS + local-path | ≤ ~600 MB | single node, SQLite datastore (no etcd), `--disable metrics-server`, no cloud-controller, no HA. **Measured in CI (2026-11-25, 16 GB runner, not a 2 GB VPS): 510-563 MiB for the whole node (all figures in this section are MiB, binary, taking "MB" as MiB), of which the pods are ~50-55 MiB and the k3s server, containerd and kernel the rest; metrics-server off saves ~26 MiB. Against the ~600 target the headroom is only ~37 MiB at the worst run (6%) and excludes the host OS. See `docs/measurements/phase0-ci.md`. The 2 GB-box figure is still unmeasured and remains the biggest risk to the "2 GB" goal.** |
| can-core + site + forms | ≤ ~100 MB | Go binaries, static server. Measured idle, no traffic, cgroup working set (not RSS), on a 16 GB CI runner: forms 3.2 MiB, can-static 2.2 MiB; can-core does not exist yet. Against forms' declared `ram_mb: 15` the +20% check (18 MB) is loose: it would pass a 5x regression |
| Shared databases | ~150–400 MB | Postgres only if a Postgres module (Listmonk, Umami…) is enabled; MariaDB only if a MariaDB-only module is enabled; tuned small buffer sizes. Phase 0 checks whether FreeScout can use Postgres, which would let the starter stack run a single engine |
| Each module | per §5 | declared `ram_mb` is the idle **request**; the **limit** is `ram_limit_mb` (default 1.5×). CI measures idle RAM and fails if it exceeds `ram_mb` by >20% |

Rules: images must be pinned by digest, preferably alpine/distroless; every chart sets requests **and** limits (§4.1); swap/zram is configured by the installer on small boxes (together with the kubelet `fail-swap-on=false` setting it requires); no module may run a sidecar by default. The admin UI sums declared RAM of enabled modules plus the platform baseline against the server and warns before over-commit.

Peaks (image pulls, Helm renders, restic) are budgeted separately and run one at a time. Rough guide, to be replaced by measured numbers after Phase 0 (the starter stack on 2 GB is **tight** and assumes the lightest picks — GoatCounter (~20 MB, SQLite) instead of Umami, and a single database engine, which depends on FreeScout running on Postgres (Phase 0); otherwise expect two engines and a 4 GB server; verify the smallest plan each provider actually offers — if there is no 2 GB plan, 4 GB is the default):

| Server | Comfortable for |
|---|---|
| 2 GB RAM (tight) | Platform + site, forms, support, newsletter, analytics |
| 4 GB RAM | Above + CRM, wiki, status page, SSO, simple tasks (Vikunja) |
| 8 GB+ RAM | Heavier alternatives (Chatwoot, Twenty, Plane…) and the full lifecycle suite (people, finance, work, equity…); measured in Phase 0 — a dozen in-house Go services plus Documenso, Kimai/Solidtime, Lago and Snipe-IT add up, and MariaDB-only apps (Kimai, Snipe-IT, FreeScout) bring the second database engine |

**Stretch (Phase 3): hibernation.** Rarely used modules (wiki, CRM, booking) can be scaled to zero after idle and woken on the next request by can-core, so a small box can "enable" more modules than it could run at once.

If even the k3s baseline is too heavy for a given idea, a static-only site can be deployed to a free static host with no server at all (`provider: static`).

### 3.3 State & config layout

On the operator's machine / git repo (the "company repo"):

```
my-company/
  company.yaml            # name, domain, provider, enabled modules, primary providers
  config/<module-id>/     # per-module settings
  Makefile                # thin wrapper, generated by `can init`
```

`company.yaml` is the source of truth (including operator choices such as `primary: {contacts: crm}`); only these files are committed. On the server, everything else lives in the cluster:

| What | Where |
|---|---|
| Module data | PVCs (k3s local-path) under `/var/lib/rancher/k3s/storage` |
| Databases | PVCs of the `can-data` StatefulSets |
| can-core state (SQLite: event outbox, admin password hash + TOTP seed) | PVC in `can-system` |
| Secrets | Kubernetes Secrets (encrypted at rest); the operator's local copy lives in `.can/` (git-ignored) |
| Applied `company.yaml` | ConfigMap in `can-system`, pushed by the CLI on `apply`. Changes made in the admin UI (Phase 2) are written to this copy; `can pull` writes them back to the repo and `can apply` warns on drift. Included in backups, so restores don't depend on the operator's repo |
| Rendered manifests | Never stored; rendered from `company.yaml` + charts on every `apply` |

### 3.4 The five-minute budget

`make up` on a fresh account with an API token configured:

| Step | Target | Levers |
|---|---|---|
| Create VM at provider | ~30–60 s | small instance, provider API called directly (no Terraform round-trip) |
| Boot + cloud-init installs k3s | ~60 s | single binary install, minimal flags, no extras |
| Pull images (can-core, Traefik, site, forms, support + DBs) | ~60–90 s | pulled **in parallel with** k3s start (images either pre-seeded as tarballs in k3s's `agent/images` directory — downloaded by cloud-init from a release asset or object-storage bucket, a Phase 0 item since the download itself costs time — or pulled as soon as containerd is up); small images; optional pre-baked provider snapshot with images preloaded (~2 min total) |
| Deploy + TLS | ~30 s | Traefik ACME against an instant hostname (persistent `acme.json`) |
| **Instant URL** | immediate | the site is first served on `<ip-dashed>.sslip.io` (and `admin.<ip-dashed>.sslip.io`, `help.…`, one certificate per host) with real TLS, so it works before DNS exists. sslip.io is a volunteer-run external service and Let's Encrypt rate limits apply to it; if unavailable the fallback serves only the public site over plain HTTP on the IP, and the admin UI is reachable only through an SSH tunnel until DNS/TLS is set (verify in Phase 0) |
| Custom domain | ~1 min with Cloudflare API; manual otherwise | records created automatically when a DNS provider token is configured; propagation time is excluded from the budget |

**Email is not in the budget.** The support inbox UI and forms are live after `make up`, but real mail needs an outbound relay and an inbound path on the real domain (§5 email policy), which can't work on an sslip.io hostname. `make up` takes optional relay credentials from `company.yaml`/`.can/`; without them forms submissions are stored and shown in the admin UI, and `make email` walks through connecting the relay and inbound mail afterwards (excluded from the budget like DNS propagation). Heavier modules are enabled afterwards (`make enable crm`, ~1–2 min, dominated by image pull). Phase 0 validates this budget end to end and adjusts the table.

### 3.5 Providers

A **provider plugin** for server-based targets only has to create a Linux VM and give us SSH. Everything after that is identical on every provider, which is what makes support cheap:

```
type Provider interface {
    Provision(spec ServerSpec) (Server, error) // VM + firewall + SSH key; returns IP
    Destroy(Server) error
}
// Optional capability, implemented by providers that support images:
type Snapshotter interface { Snapshot(Server) (Image, error) } // pre-baked images
```

| Provider | Status |
|---|---|
| `local` | k3d cluster on the operator's machine (`make dev`) |
| `ssh` | Bring-your-own server: any VPS or box reachable over SSH (covers every provider on day one) |
| `hetzner` | Phase 1 (automated provisioning) |
| `digitalocean` | Phase 2 |
| `vultr`, `linode`, `scaleway`, `lightsail` | Later, community-contributed |
| `static` | No server: renders the site locally and deploys it to Cloudflare Pages / GitHub Pages (site only; forms via an external endpoint) |

`local` (k3d) and `static` are target types that don't fit `Provision`/SSH and are handled by dedicated code paths in the CLI. For `ssh`, `Destroy` means "uninstall k3s and can-core, leave the box" (`make down` says so before acting).

DNS is a separate plugin (`cloudflare`, `provider-native`, `manual` — the last prints the records). Bootstrap uses cloud-init where the provider supports it and SSH otherwise.

### 3.6 Local development

- `make dev` creates a k3d cluster (k3s in Docker), installs can-core and applies `company.yaml` with the `local` provider. Hostnames are `*.localtest.me` (resolves to 127.0.0.1), plain HTTP or mkcert TLS.
- k3d adds a CoreDNS rewrite (again via `coredns-custom`) so `*.localtest.me` resolves to the ingress inside pods too (otherwise OIDC and webhook self-references would hit the pod's own loopback).
- Same charts, same can-core, same NetworkPolicies as production. Only the provider and DNS plugins differ.
- In-house modules get a fast loop (`make dev-module M=forms`): build, load the image into k3d, restart the pod. Skaffold/Tilt are optional.
- CI runs the module smoke tests and footprint checks on k3d.
- Prerequisites on the dev machine: Docker, k3d, kubectl (optional), Go, make. `make doctor` checks them.

### 3.7 Shared platform services

The lifecycle modules (people, equity, contracts, invoices, time, jobs, vendors) all need the same cross-cutting machinery. It lives in can-core (or one small `portal` pod next to it), **not** in each module, so there is one implementation, one audit trail and no extra footprint per module. Modules use it through the module API and capabilities (§4.3).

| Service | What it does | Notes |
|---|---|---|
| **Roles & permissions (RBAC)** | Roles (owner, finance, HR, manager, employee, contractor, investor, auditor…) and per-module permissions, defined in each module's manifest (`permissions:`) and assigned in the admin UI. Enforced by can-core for UI routes and checked by modules through a `can.check(user, action, resource)` call on the module API. Field-level hiding for the most sensitive data (salaries, valuations). Manifests also carry default role grants (`roles:`) that the operator can override. **Wrapped OSS apps enforce their own roles**, so RBAC for them is coarse (SSO groups/forward-auth, per-app roles mapped by the adapter); consequently the most sensitive data classes (people records, salary and ID data, equity, performance, payroll data) are held by in-house modules only, where per-field checks and read auditing are possible. Wrapped apps may hold lower-sensitivity per-person data (assignments, logged hours), but documents containing salary or ID data (signed offers, employment contracts) are moved into `files` with per-class rules and purged from the e-sign app's own store after signing. | Phase 2. Until then there is a single admin role. Roles are groups in SSO so modules that speak OIDC get them as claims. |
| **Approval workflows** | One engine for request → approve → record: purchase orders, expenses, timesheets, offers, contract signatures, equity grants, leave. Configurable chains (the requester's manager from `directory` reporting lines, then finance above a threshold), delegation, deadlines, audit entries. Emits `approval.requested.v1` and `approval.decided.v1`. | Modules declare the approval types they use in their manifest (type, payload schema, default chain); they don't implement their own. **Phase 2 chains are static (by role or named user); reporting-line chains switch on when `directory`/`people` arrive in Phase 3a.** |
| **Notifications & tasks** | A per-user inbox of things that need attention (pending approvals, renewal and notice dates, vesting cliffs, overdue invoices), delivered in the admin UI and by email/push via `email.send`. Modules publish reminders with a due date; can-core schedules and de-duplicates them. | Backs the existing dashboard inbox (§6.3). |
| **Portals** | Self-service views for people who aren't staff operators: customers (invoices, contracts, tickets), employees (time off, own timesheets, own option grants and vesting, documents to sign), vendors (POs, bills), investors (read-only cap table, reports). Each audience sees only its permitted slice. External users authenticate with magic link or passkey, not staff SSO. | A small `portal` service in its **own unprivileged namespace (`can-portal`, pod security `restricted`)**, separate from the privileged can-core; it renders views that modules expose through a `portal.view` capability and holds only auth/session state for external users (magic-link/passkey credentials and their email addresses in SQLite), no business data; it has a `backup` dump through the module API and a `data_classes` entry (`external-user-credentials`) with a `purge` path, so restores keep enrolments and erasure covers it. |
| **Unified search** | One search box across CRM, contacts, contracts, invoices, tickets, wiki, directory names and contact details (the non-sensitive subset; sensitive classes are excluded unless the operator opts in, permission-filtered). Modules push searchable records to can-core's `search` capability; backed by SQLite FTS5 or Postgres full-text search, so no Elasticsearch-style RAM. | Phase 3. |
| **Reporting** | Cross-module numbers (revenue vs. billable hours, headcount, open pipeline, runway, cap-table summary) from read-only report views that modules expose through a `reports` capability; scheduled PDF/email digests. | Phase 3. |
| **Import / migration** | Importers per module for common sources (CSV; HiBob/BambooHR people, QuickBooks/Xero invoices, Toggl/Clockify time, Carta/spreadsheet cap tables, HubSpot contacts) mapped through the `import` hook (§4.1). Dry-run with a diff before committing. | Adoption depends on this; importers are community-friendly plugins. |
| **Automation & webhooks** | Generic outbound webhooks for every event type and inbound webhooks, so anything not built can be glued on; an optional wrapped no-code workflow module: Activepieces (MIT core with a commercial `ee/` carve-out; verify) by default, n8n only as an opt-in `source-available` module (its Sustainable Use Licence isn't OSI-approved). | Phase 2 (webhooks); workflow module Phase 3c. |
| **Retention & legal hold** | A policy engine that enforces per-data-class retention ("delete rejected candidates after 12 months", "keep invoices 7 years") with legal-hold overrides, deletion reports, and data-subject export/erase across all modules. Modules declare their data classes in the manifest (`data_classes:`) and implement a `purge(subject, class)` hook (see *Erasure semantics* below). | Phase 2 design, enforced before the first people-data module ships. |
| **Audit log** | Append-only log of reads of sensitive data and all writes (who, what, when, from where), tamper-evident (hash-chained), exportable. Entries reference salted per-subject ids (pseudonymous personal data, erasable by destroying the salt), not raw identifiers. | Phase 1 for admin actions; extended as modules arrive. |

**Erasure semantics.** Erasure and retention interact with append-only stores, so the rules are explicit:
- **Legal retention beats erasure.** Records we must keep (invoices, the equity ledger, payroll exports, audit entries) are retained for their legal period and the data subject is told which; only PII not needed for that purpose is erased.
- **Append-only stores hold party ids, not PII.** The audit log and the equity ledger (the only append-only stores; the event outbox is a derived store with a TTL, below) reference `directory` party ids and hashes; the PII lives in `directory` (and module tables) where it can be erased or pseudonymised, leaving the chain intact. Where PII must be inside a record, it is encrypted with a per-subject key that can be destroyed (crypto-shredding).
- **Derived copies have TTLs.** Event payloads in the outbox, the search index and cached report views carry minimal data, expire on a short TTL, and are purged by subject on request. Sensitive classes (people, equity, performance) are excluded from the search index by default, and report views for them are computed at query time from the source module rather than copied into can-core.
- **Backups.** restic snapshots keep erased data until they expire. The backup retention horizon (default 35 days) is documented, and an erasure request is complete when the last snapshot containing the data has expired; an **erasure tombstone ledger** (subject id, data class, date — no PII) is written as a separate encrypted object in the backup bucket, outside the snapshots it governs (written by can-core, uploaded by the host backup timer, encrypted with a key derived from the restic password), and a restore consults it first and re-applies every erasure before going live. Until a snapshot expires the data is held "beyond use", and the response to the requester says "erased from live systems; backups expire by <date>".

Manifest additions (§4.1): `permissions:` (the actions the module defines), `approvals:` (approval types it uses), `data_classes:` (what personal/financial data it holds, with default retention) and the `purge` hook. Capability additions (§4.3): `approvals`, `notify`, `search`, `reports`, `files`, `portal.view`.

Footprint: all of this runs inside the existing can-core process (plus the small portal pod); state is SQLite/Postgres rows, not new services. The footprint CI (§3.2) covers can-core's idle RAM including these services.

## 4. Module system (the core idea)

A module is a directory with a **manifest** and a **Helm chart**. Wrapping an OSS app and writing an in-house app use the *same* contract. Upstream charts can often be used as a subchart; otherwise the module ships its own small chart.

### 4.1 Manifest (`module.yaml`)

```yaml
schema: 1                    # module.yaml schema version
id: crm
name: CRM
version: 0.1.0               # version of this module wrapper (semver)
category: sales
description: Contacts, companies, deals, pipeline.
provider:
  type: oss-wrapper          # oss-wrapper | inhouse | external
  name: EspoCRM
  licence: AGPL-3.0          # SPDX id; tracked for compliance (verify in Phase 0)
  licence_class: osi         # osi | source-available (opt-in only, flagged in UI)
  upstream: https://github.com/espocrm/espocrm
  upstream_version: "9.x"    # images pinned by digest in the chart
runtime:
  chart: ./chart             # Helm chart (own, or wrapping an upstream chart)
  subdomain: crm             # -> crm.<domain>
  ui: { service: app, port: 8080 }   # what Traefik routes to
  values: ./values.tmpl.yaml # maps injected inputs (DB creds, SMTP, CAN_API_URL, token volume, HMAC secret) onto the chart's values; needed to drive upstream subcharts
requires:
  database: mariadb          # postgres | mariadb | sqlite | none
  capabilities: [email.send] # hard dependencies, from the §4.3 table
  sso: oidc                  # oidc | forward-auth | none (needs auth.oidc once SSO is enabled)
resources:                   # ram_mb = idle request (CI checks idle RAM ≤ 1.2×); disk is advisory
  ram_mb: 400
  ram_limit_mb: 600        # Kubernetes limit (default 1.5× ram_mb)
  disk_mb: 500             # local-path does not enforce capacity: surfaced as a disk-usage alert
provides:                    # capabilities other modules can consume
  - capability: contacts
    schema: contacts.v1      # OpenAPI schema in schemas/capabilities/
    service: adapter         # chart Service that implements it (the app itself, or an adapter)
    port: 8081
consumes:                    # soft integrations: capabilities used if a provider is enabled
  - tickets
publishes:                   # events this module emits, with payload schemas (§4.3 catalogue)
  - { event: contact.created.v1, schema: ./events/contact.created.v1.json }
subscribes:                  # events delivered to this module (§4.3)
  - event: lead.created.v1
    service: adapter         # chart Service receiving the webhook (defaults to the first `provides` service; required if the module provides nothing)
    port: 8081
    webhook: /hooks/lead
    optional: true           # module still works if no one publishes this event
permissions:                 # actions this module defines for RBAC (§3.7)
  - contacts.read
  - contacts.write
approvals: []                # approval types it defines/uses: [{type: expense, schema: ./expense.schema.json, default_chain: [manager, finance]}]
roles:                       # default role grants (operator can override)
  sales: [contacts.read, contacts.write]
data_classes:                # what sensitive data it holds, with default retention (§3.7)
  - { class: customer-contact, retention: "7y" }
settings_schema: ./settings.schema.json   # drives the admin UI form
hooks:                       # run by can-core as Jobs (module image, or `image:` below)
  image: ghcr.io/example/crm-hooks:0.1.0   # optional; defaults to the module's main image
  install:   ./hooks/install.sh
  configure: ./hooks/configure.sh
  backup:    ./hooks/backup.sh
  restore:   ./hooks/restore.sh
  upgrade:   ./hooks/upgrade.sh
  export:    ./hooks/export.sh     # portable data export (open formats)
  import:    ./hooks/import.sh     # load an export, possibly from a different provider
  purge:     ./hooks/purge.sh      # delete a data subject or data class (retention, erasure requests)
  uninstall: ./hooks/uninstall.sh
healthcheck: /health         # becomes the readiness/liveness probe
```

### 4.2 Lifecycle

`install → configure → enable → (upgrade)* → disable → uninstall (keep|purge data)`

- `install` runs as a pre-install Job that seeds data and config (can-core has already created the database and credentials, §4.5); `enable` = `helm upgrade --install --atomic` in the module's namespace; `disable` = scale to zero (data kept), no hook needed. Uninstall runs `export`, then asks keep/purge.
- All lifecycle steps are idempotent and run by `can apply`. Upgrades are atomic Helm releases, so a failed readiness check rolls back the chart (Helm does not undo database migrations, hence `can upgrade` snapshots first).

### 4.3 Contracts between modules (so they stay swappable)

Modules must not call each other's internals. They talk through **capabilities** (each defined with the verbs its schema allows; write verbs are listed explicitly, e.g. `tickets` can be reassigned, not just created). Capabilities provided by can-core itself (`approvals`, `notify`, `email.send`, `events`, `search`, `identity.provision`, backed by the SSO user store) are consumed by listing them under `requires.capabilities`:

| Capability | Meaning | Example swap |
|---|---|---|
| `contacts` | Create/find/update/reassign-owner a person/company and its deals; holds the `directory` party id on each record (`external_ids`) | EspoCRM → in-house CRM |
| `tickets` | Create/read/update/reassign support tickets | FreeScout → in-house support |
| `files` | Store, fetch and export files (object storage or PVC) with per-class access rules | any |
| `portal.view` | Render a read-only/self-service view for a given audience (customer, employee, vendor, investor) (`aggregate: true`, pull) | any |
| `email.send` | Send via the shared SMTP relay | any |
| `events` | Publish/subscribe webhook events | any |
| `leads` | List/read captured form submissions (the admin UI's leads view reads this) | forms gateway → in-house CRM |
| `newsletter` | Lists and subscribers; subscribing requires recorded consent (double opt-in by default) (`subscriber.added.v1`) | Listmonk → in-house |
| `analytics` | Privacy-friendly site statistics, read-only aggregates plus the tracking snippet | GoatCounter → Umami |
| `auth.oidc` | Identity | Authelia → in-house |
| `content` | Public pages/articles | wiki → CMS |
| `directory` | The company's **people**: employees, candidates, contacts, vendors as typed parties with one identity each | in-house `people` → HR suite |
| `jobs` | Open roles, applications, candidate pipeline; board syndication (§5 Recruiting) | in-house ATS → OpenCATS/Frappe HR |
| `vendors` | Suppliers, purchase orders, bills | in-house → Akaunting/ERPNext |
| `work` | Issues/stories/epics/sprints with priority, status, assignee and links; create/update by API | Plane/Redmine → in-house `work` |
| `projects` | Read-only project directory of the work tracker (keys, names, workflows), so `time`, `invoices` and `reports` can refer to a project | in-house `work` |
| `performance` | 1:1 series, agendas, notes, action items, goals/OKRs, feedback and review cycles | in-house → other |
| `documents` | Request/store/sign documents (offers, contracts, NDAs, POs) | Documenso → other e-sign |
| `contracts` | Contract registry: parties, terms, dates, renewals, obligations, linked signed documents | in-house → other CLM |
| `invoices` | Issue and track receivable invoices and payable bills, payments, credit notes | Invoice Ninja/Akaunting → in-house |
| `time` | Time entries and timesheets per person/project/client, with approval and billable flags | Kimai/Solidtime → in-house |
| `timesheets` | Weekly timesheets, approval, locked periods and billable time to draft invoice lines; the in-house finance module's name for the timesheet half of `time` (E15a, `specs/014-finance-basics/`) | in-house `finance` → Kimai/Solidtime |
| `approvals` | Request/approve/reject with configurable chains (provided by can-core) | in-house engine → any |
| `notify` | Schedule reminders and per-user attention items (provided by can-core) | any |
| `search` | Query across modules, permission-filtered. Provided by can-core; modules **push** records to it (sensitive classes excluded by default) | FTS → dedicated search |
| `reports` | Read-only report views for cross-module reporting (`aggregate: true`, pull) | any |
| `equity` | Cap table ledger: share classes, issuances, option grants, vesting, exercises, convertibles | in-house (Captable import supported) |
| `identity.provision` | Create/disable staff accounts across modules (SCIM-style). With Authelia this is backed by the SSO user store (an LDAP service, lldap, shipped as part of the `sso` module, or can-core managing the users file); `people` links a `directory` party id to the account id. The capability is usable for staff accounts from Phase 2 (SSO) and gains the hire/leave automation in 3a; apps without SCIM (Vaultwarden) are handled by adapters or manual steps | Authelia+lldap → in-house |

Where an OSS app has no matching API, the module ships a small **adapter** that translates to the capability. Replacing a module = new module implementing the same capabilities; the rest of the system is unchanged.

**Adapters.** An adapter is an extra Deployment/container in the module's own chart (so adapters can be written in any language and stay part of the module), or, for small glue needing cluster access, a can-core plugin. Either way it is listed under the module's `provides`.

**Lead flow (reference example).** The forms gateway publishes `lead.created.v1` to `events`. can-core delivers it, authenticated, to the webhook the CRM module declares under `subscribes`; the CRM's adapter creates the person/company in EspoCRM. Other modules (e.g. support, to open a ticket) subscribe the same way. Consumers that need to look contacts up call the `contacts` capability through can-core's registry. Event types and their schemas live next to the capability schemas.

**Event catalogue (initial).** Event types are versioned, defined next to the capability schemas, and carry a `party_id` where a person is involved. Producers: forms `lead.created.v1`; CRM `deal.won.v1`; invoices `invoice.issued.v1`, `invoice.paid.v1`, `bill.received.v1`, `bill.approved.v1`, `bill.paid.v1`; vendors `vendor.created.v1`, `po.approved.v1`; jobs `job.published.v1`, `application.received.v1`, `offer.accepted.v1`; people `person.hired.v1` (emitted by `people` on a manual hire action in Phase 3a; once `jobs` exists in 3b, `offer.accepted.v1` is an additional trigger), `person.left.v1` (with an `effective_date`, since vesting and access end on that date, not on event time); the in-house `people` module (E13a) publishes the finer-grained `person.created.v1`, `person.status_changed.v1` and `person.offboarded.v1` (which carries the `effective_date`, replacing `person.left.v1`; a hire is a status change to `active`) `person.erased.v1` (an erasure request purged the person; consumers drop their copies by `party_id`) and `checklist.task_completed.v1` (onboarding and offboarding tasks with a machine `action`, for later `identity.provision`); documents `document.signed.v1`; contracts `contract.requested.v1`, `contract.signed.v1`, `contract.expiring.v1`; time `time.approved.v1`; board `resolution.passed.v1`; equity `equity.proposed.v1`, `equity.approved.v1`, `equity.granted.v1` (only after the grant agreement is signed); work `issue.created/updated/transitioned/priority_changed.v1`; performance `oneonone.scheduled.v1`, `goal.updated.v1`, `one_on_one.action_item.created.v1`, `review.cycle.opened.v1`; can-core `approval.requested.v1`, `approval.decided.v1` (one terminal event with an `outcome`: approved, rejected, cancelled or expired; E11a, replacing the separate approved/rejected events) and `notification.sent.v1`. Modules list what they publish and subscribe to in their manifest, and the catalogue grows as modules are written.

**Protocols.**
- Each capability is defined by an OpenAPI schema in `schemas/capabilities/` (REST + JSON). A provider exposes it as a ClusterIP Service only.
- can-core is the registry: consumers ask it for the current provider of a capability and then call that provider **directly** (a NetworkPolicy allows module-to-module traffic only for declared `provides`/`consumes` pairs). Callers present their projected ServiceAccount token; adapters verify it with `POST /v1/tokens/introspect` on the module API (which performs a TokenReview). can-core does not proxy capability calls. Consumers never hard-code a provider.
- `email.send` is provided by can-core itself, wrapping the configured SMTP relay, and is the REST path for adapters and in-house modules. SMTP-speaking OSS apps (FreeScout, Listmonk, Authelia) can't call it, so declaring `email.send` in `requires.capabilities` makes can-core inject SMTP relay credentials into their environment (the shared transactional relay, or a separate bulk relay configured for the newsletter module). `auth.oidc` is provided by the SSO module.
- **Events** are JSON envelopes (`id`, `type`, `time`, `source`, `data`) published to can-core, persisted in its SQLite outbox, and delivered to subscribers' webhooks **at least once** with exponential-backoff retry and an `id` for idempotent handling. Event types are versioned (`lead.created.v1`).

### 4.4 Module registry

- v1: modules live in this repo under `modules/`.
- Later (Phase 3): third-party modules from git URLs or OCI registries; manifest signature/digest pinning; `can module add <ref>`.

### 4.5 Dependency & conflict rules

- Enabling a module auto-enables hard `requires` (e.g. database) and offers soft `consumes` integrations.
- Capabilities flagged `aggregate: true` (`reports`, `portal.view`) are multi-provider by design with a **pull** model: every module may provide one without conflict, and the consuming service (can-core's reporting, the portal) fans out to all providers at query time. `search` is the opposite, a **push** model: can-core provides it and modules push records into it. All other capabilities are exclusive.
- Two modules providing the same exclusive capability (e.g. two CRMs) conflict unless the operator designates one as the primary provider for that capability in `company.yaml` (the other then serves no consumers).
- Shared databases: one Postgres and one MariaDB instance max (a module with bundled private datastores, e.g. Plane, may run them inside its own `mod-<id>` namespace as a counted exception, with no access to `can-data`; modules with `sqlite` or `none` use neither); each module gets its own DB + user. can-core creates them on enable from `requires.database` by connecting to the in-cluster database Service with admin credentials, and injects the module's credentials as a Secret.

## 5. Module catalogue

Picks favour **light footprint**, **active maintenance** and **licences compatible with an OSS project**. Heavier alternatives are listed for larger servers. Modules must use an OSI-approved licence; anything source-available is `licence_class: source-available`, opt-in and flagged in the UI. These are starting points — each choice should be validated in a spike (see §9).

| Module | Initial OSS pick | ~RAM | Lighter / heavier alt. | Notes |
|---|---|---|---|---|
| **Landing / site** | In-house static renderer + templates | ~5 MB (static server) | Astro, Hugo, 11ty (advanced) | Rendered by the shared renderer library (CLI/can-core) in <1 s; sections driven by `company.yaml` |
| **Forms** | In-house forms gateway (Go) | ~15 MB | Formbricks (heavy) | Contact/waitlist/quote forms, honeypot, timing check, optional Turnstile/hCaptcha, webhooks |
| **Support / helpdesk** | FreeScout (PHP) | ~250 MB | Chatwoot, Zammad (heavy) | Shared inbox, email-in. FreeScout's KB is a paid add-on, so the public help centre comes from the wiki module |
| **Wiki / knowledge base** | BookStack or Wiki.js | ~200 MB | Docmost (Outline is BSL-licensed, so excluded) | Internal wiki + optional public help centre |
| **CRM** | EspoCRM | ~350 MB | Twenty, SuiteCRM (heavy) | Contacts, deals, pipeline; fed by forms gateway |
| **Newsletter / email marketing** | Listmonk (Go) | ~50 MB | Mautic (heavy) | Lists, campaigns, double opt-in; via SES/Brevo relay |
| **Analytics** | Umami or GoatCounter | ~100 / 20 MB | Plausible (needs ClickHouse) | Privacy-friendly, no cookie banner |
| **Work tracking (Jira-like)** | **Plane** (AGPL-3.0, closest to Jira; heavy, **8 GB+ servers**) as the first wrapper; **in-house `work`** (lean Go service) as the target | Plane ~1.5–2 GB (multi-service, private datastores; measure) / in-house ~40 MB (target) | Redmine (lighter, dated UI, REST API), Taiga, OpenProject (heavy), Kanboard (light, not Jira-like) | API-first issue tracker with epics, stories, P0–P3 priority, sprints/boards, workflows, labels, comments, links and webhooks; see *Managing people and work* below. The adapter maps P0–P3 onto the tracker's own priorities (Plane: urgent/high/medium/low); features a wrapped tracker lacks (custom workflows, components, saved filters — verify per tracker) are optional capability flags that only the in-house `work` fully provides |
| **Projects / tasks (simple)** | Vikunja (Go) | ~50 MB | — | Personal/team to-do lists and light Kanban for small teams that don't need an issue tracker. From Phase 3a, when `work.v1` and its adapter exist, it provides `work` at a reduced feature level; before that it is a standalone app |
| **1:1s, goals & reviews** | In-house `performance` (Go) | ~30 MB (target) | OrangeHRM/Frappe HR performance (heavy) | Manager 1:1 series with shared agenda and private notes, action items that become `work` issues, goals/OKRs, feedback, review cycles; see below |
| **Status page / uptime** | Uptime Kuma | ~100 MB | Gatus (lighter) | Public status + alerts (should be monitored from outside the box too) |
| **Scheduling / booking** | Easy!Appointments | ~150 MB | Cal.com (heavy) | Discovery calls, demos |
| **Invoicing / billing** | **Licence-checked in Phase 0.** Invoice Ninja (v5 is, I believe, Elastic License 2.0) and Akaunting (source-available terms) probably fail the OSI rule; candidates that should pass: InvoiceShelf (AGPL), ERPNext (GPL-3, heavy). Until verified, **the in-house `invoices` module is the default plan** | ~300 MB wrapped / ~40 MB in-house (target) | Lago (usage billing; multi-service, heavier than it looks) | Pair with Stripe/PayPal links. Immutability of issued invoices (credit notes only) is enforced by the in-house module, or by an adapter guard that blocks edit calls on a wrapped one |
| **E-sign / documents** | Documenso (some features are under a commercial licence — verify what the open core covers) | ~400 MB | DocuSeal, OpenSign (AGPL; verify) | Contracts, NDAs, offer letters, purchase orders |
| **Jobs / recruiting (ATS)** | In-house `jobs` module (Go) | ~30 MB (target) | OpenCATS (dated), Frappe HR recruitment (heavy) | Careers page, pipeline, **job-board syndication built in** — see *Recruiting & job-board syndication* below |
| **People / HR core** | In-house `people` (directory, onboarding checklists, time off) to start | ~30 MB (target) | OrangeHRM, Frappe HR (heavy) — licences/footprint to verify | The `directory` provider; drives hire/leave workflows below |
| **Vendors / procurement** | In-house `vendors` (supplier registry, POs, approvals) | ~30 MB (target) | ERPNext, Odoo (heavy) | Pairs with invoicing/accounting for bills (accounts payable) |
| **Accounting / expenses** | Export/sync to the operator's accounting system (Xero, QuickBooks, local equivalents) through adapters; ERPNext only for operators who want a full ERP | — / heavy | ERPNext | We integrate rather than reinvent accounting; licences of any wrapped app are verified first (see Invoicing) |
| **Timesheets / time tracking** | Kimai or Solidtime (both AGPL-3.0; verify) | ~150–300 MB (Solidtime is Laravel + Postgres + a PDF service; Kimai needs MariaDB; measure) | Traggo (lighter), in-house `time` | Billable vs. internal hours per project/client. Their own approval features vary (Kimai's is, I believe, a paid plugin), so approval runs through can-core's approval engine via the adapter; feeds invoicing, payroll export and project reports |
| **Contracts (lifecycle)** | In-house `contracts` registry on top of `documents` | ~20 MB (target) | OpenSign/DocuSeal for signing; no mature OSS CLM exists | Templates and clauses, counterparties from `directory`, renewal/notice-date reminders, obligations, status; signed PDFs live in `files`, referenced from `documents` |
| **Equity / cap table** | **In-house `equity` from the start** (an append-only ledger with per-field access and read auditing can't be enforced on a wrapped app). Captable (open source; verify licence) is a reference/import-format spike only | ~30 MB (target) | Spreadsheet export | Shares, options, vesting, SAFEs/convertible notes, option pool, fully-diluted reports, round modelling, read-only investor view; see below |
| **Payroll** | **Not built** — integrate (Gusto, Deel, Rippling, local providers) through adapters | — | — | Payroll and tax are jurisdiction-specific and regulated; we sync people/contracts to the provider, we don't compute pay |
| **Expenses & receipts** | In-house `expenses` (claims, receipt capture, approval, reimbursement export) | ~30 MB (target) | Akaunting/ERPNext expenses | Sits between timesheets, accounting and payroll; uses the approval engine |
| **Bank feeds / reconciliation** | Adapter to an open-banking aggregator available in the operator's region (e.g. GoCardless Bank Account Data, Plaid, TrueLayer, Tink — availability and pricing change; verify) | — | — | Matches payments to invoices and bills; the operator holds the bank connection |
| **Subscriptions / recurring billing** | Built on `invoices` + Stripe first; Lago (verify licence) only on large servers | ~40 MB in-house / Lago ≫300 MB (Rails, workers, Redis) | — | Plans, dunning, usage billing |
| **Entity & legal calendar** | In-house `entity` (registers, filings calendar, trademarks, domains) | ~20 MB (target) | — | Directors/shareholder registers read from `equity`; reminders via `notify` |
| **Board & governance** | In-house `board` (meetings, minutes, resolutions, consents) | ~20 MB (target) | — | Resolutions feed `equity` grants and `documents` |
| **Assets & equipment** | Snipe-IT (verify licence/footprint; MariaDB) or in-house | ~150 MB | — | Assigned to a person; returned at offboarding via the leave workflow |
| **Policies & training** | In-house `policies` (handbook acknowledgements, required training, reviews/OKRs) | ~20 MB (target) | — | Acknowledgements tracked per person |
| **Secrets / password manager** | Vaultwarden (no SCIM/directory provisioning; its SSO support is recent — verify) | ~20 MB | — | Team credentials; access reviews are an exported report of who has access, not automated provisioning |
| **Customer feedback** | Surveys/NPS (Formbricks or in-house forms) + public roadmap | varies | — | Later |
| **Compliance** | Policy/privacy/DPA templates, DSAR (data-subject request) tooling, audit log | small | — | Cross-cutting; see lifecycle below |
| **SSO** | Authelia (default) | ~30 MB | Pocket ID (OIDC only), Authentik, Keycloak (heavy) | Phase 2; recommended once ≥2 staff-facing modules are enabled |
| **Files (`files` capability)** | In-house thin `files` service over S3-compatible storage or a PVC, with per-data-class access rules, encryption and read auditing (Phase 3a) | ~20 MB (target) | Nextcloud (~400 MB, optional human-facing file sharing; can't enforce per-class rules), Filebrowser (maintenance mode) | Holds resumes, signed documents, exports. The `documents` adapter returns `files` references for sensitive classes, and signed PDFs are purged from the e-sign app's own store after signing |
| **Chat widget / live chat** | Chatwoot widget or in-house | varies | — | Phase 3a |
| **Email hosting (mailboxes)** | *Not self-hosted* | — | — | Deliverability is a trap; use the inbound/outbound services in the email policy below |

**Email policy:** do not self-host a full mail server.
- **Inbound:** forwarding services (Cloudflare Email Routing, ImprovMX) only forward to an existing address, so they aren't enough alone. The support module reads mail either over IMAP from a small hosted mailbox that `support@domain` forwards to, or via an inbound-parse service (Postmark, Mailgun, SES→webhook) that posts to a public webhook on an adapter pod in the support module, which creates the ticket. The chosen mechanism is a setting of the support module.
- **Outbound:** a transactional relay (SES, Resend, Brevo). Support replies are sent through the relay with `From: support@domain`, with SPF/DKIM/DMARC aligned to the company domain; `can` prints or sets the required DNS records. Many providers block outbound port 25, so the relay is used over 587/443.

### Business lifecycle coverage

The catalogue is organised around what a company actually does from day zero to wind-down, so modules compose into workflows instead of being a pile of apps:

| Stage | Needs | Modules (OSS wrapper → in-house over time) |
|---|---|---|
| **Launch** | Web presence, domain, email, legal basics | site, forms, DNS plugin, email policy, policy templates (privacy, terms, DPA) |
| **Sell** | Leads, deals, quotes, contracts, invoices, payments, subscriptions, billable time | forms → CRM → contracts/e-sign → invoicing + Stripe/PayPal links; timesheets; booking; newsletter; analytics |
| **Support & retain** | Tickets, help centre, status, feedback | support, wiki (public help centre), status page |
| **Buy** (suppliers) | Vendor onboarding, contracts and renewals, purchase orders, bills, approvals | `vendors`, `contracts`, documents/e-sign, invoicing/accounting (payables) |
| **Hire** | Job openings, applicants, interviews, offers | `jobs` (ATS) + board syndication, documents/e-sign (offers), forms intake |
| **Onboard / manage people** | Records, accounts, checklists, time off, timesheets, **equity grants**, policies, performance | `people`, `performance` (1:1s, goals, reviews), `equity`, timesheets, SSO/provisioning, wiki (handbook), `work` (onboarding tasks), payroll adapter |
| **Operate** | Work tracking, priorities, projects, knowledge, files, internal comms | `work` (issues, boards, sprints), wiki, file storage |
| **Finance & compliance** | Bookkeeping, expenses, tax, **cap table and investor reporting**, audit trail, GDPR/CCPA requests | accounting adapter, `equity`, compliance tooling, can-core audit log |
| **Infrastructure** | Hosting, TLS, backups, monitoring, updates | can-core platform (§3, §6.5) |
| **Wind-down / exit** | Export everything, offboard, close accounts | `can export`, `make down`, provisioning disable flows |

**Cross-module workflows** run on the event bus and capabilities (§4.3), so each is a configuration of existing modules rather than bespoke code:

- **Customer:** `lead.created.v1` → CRM contact → `deal.won.v1` → invoice + payment link → `invoice.paid.v1` → support entitlement / welcome email.
- **Supplier:** `vendor.created.v1` → (explicit action) `contract.requested.v1` → signature (`documents`) → `po.approved.v1` → `bill.received.v1` → approval → paid; the vendor is a typed party in `directory`.
- **Hire:** `job.published.v1` → board syndication → `application.received.v1` → pipeline → offer via `documents` → `offer.accepted.v1` → `people` emits `person.status_changed.v1` (`onboarding` to `active`, consumers filter on `to`/`from`) → `people` onboarding checklist + `identity.provision` creates accounts + projects/wiki onboarding tasks + payroll adapter sync (once the adapter exists, Phase 3b).
- **Contract:** `deal.won.v1` (customer) or a vendor onboarding action (supplier) → `contract.requested.v1` → contract from template → signature via `documents` → `document.signed.v1` / `contract.signed.v1` → `contracts` records term and renewal/notice dates → reminders → `contract.expiring.v1` raises a dashboard item; customer contracts also activate invoicing schedules.
- **Time to money:** timesheets → `time.approved.v1` → billable entries become invoice lines (`invoices`); the same approved hours export to the payroll adapter and appear in project reports.
- **Equity:** `person.status_changed.v1` to `active` (consumers filter on `to: active`; `from: onboarding` for a first hire) or `resolution.passed.v1` → `equity.proposed.v1` → approval (`approvals`) → `equity.approved.v1` → grant agreement via `documents` → signed → `equity.granted.v1` → vesting schedule runs in `equity` → `person.offboarded.v1` stops vesting at its `effective_date` and starts the exercise-window clock → investor/board reports read the same ledger.
- **Leave:** `person.offboarded.v1` → `identity.provision` disables every account, revokes sessions, reassigns tickets, deals and issues through the `tickets`/`contacts`/`work` write verbs, exports files through `files` → retention timers start; access ends at the event's `effective_date`.

**Design rule — one party model.** Employees, candidates, customers and vendors are all *parties* in `directory`; modules reference a party id instead of keeping private copies (a candidate who is hired becomes an employee without re-entry; a customer who is also a vendor is one record). `directory` is the source of truth for identity (names, contact details, party id) **once enabled**. Before that (Phase 2) the CRM owns contacts and `external_ids` is nullable; enabling `directory` runs an import that backfills parties from existing `contacts`, and from then on `directory` creates the party when a lead arrives and the CRM stores its id on each contact (`external_ids`); the CRM keeps owning deal and pipeline data, and the EspoCRM adapter syncs both ways. `people`/`jobs`/`vendors` (Phase 3) use the model natively; the in-house CRM (Phase 4) will too.

**Sensitive data.** People data (CVs, contracts, salaries, ID documents) is the most sensitive data we hold. Rules: separate database and storage prefix per module, encrypted backups only, per-role access in the admin UI, retention policies with automatic deletion (e.g. rejected candidates after N months), data-subject export/erase, and an audit log of reads and writes. We are a tool, not legal advice; compliance is the operator's responsibility and the docs say so.

**Managing people and work.** The manager's weekly loop — *what is each person working on, how urgent is it, how are they doing* — ties three modules together through `directory` (who reports to whom), `work` and `performance`:

- **Org structure.** `directory` carries `reports_to` (manager), team, role and location, with an org chart view. Everything below, and the approval engine's chains (§3.7), follow these reporting lines. Roles (RBAC) grant a manager visibility into their reports' work and 1:1s only.
- **Work tracking (`work`).** A self-hosted, Jira-style tracker: projects/spaces, epics → stories → tasks/sub-tasks, bugs, configurable workflows and boards, sprints/backlog, labels, components, assignees and watchers, comments/attachments, issue links (blocks, relates), and saved filters. **Priority is a first-class field: P0 (drop everything), P1 (this sprint), P2 (planned), P3 (backlog)**, with definitions and optional response targets configurable per company; changing a P0/P1 notifies the manager and the owner (`notify`). Views managers need: per-person workload (counts by priority, WIP, blocked), "what is everyone on" by team, P0/P1 across the company, and ageing of unresolved P0/P1s. Estimates and logged time connect to timesheets (`time`) so billable and capacity views use real hours. Source-control adapters (GitHub, GitLab, Gitea) link commits and pull requests to issue keys and can move issues through the workflow.
- **API (required, not optional).** Everything the UI does is available over a documented REST API (OpenAPI in `schemas/capabilities/work.v1`): create/update/transition/comment/link/search issues, bulk create (e.g. a manager's list of stories from a planning meeting), idempotency keys so retries don't duplicate, and webhooks/events for every change (`issue.created.v1`, `issue.updated.v1`, `issue.transitioned.v1`, `issue.priority_changed.v1`). Credentials are scoped API tokens for people and for **service accounts** (bots, integrations, an email-to-issue gateway, chat commands), subject to RBAC and recorded in the audit log. An optional MCP server exposes the same API so AI assistants can create and update stories on a manager's behalf under that manager's permissions. Because the wrapped OSS trackers have different APIs, the `work` adapter normalises them to this one contract; the in-house `work` module implements it natively.
- **1:1s and performance (`performance`).** Recurring 1:1 series between a manager and each direct report (cadence from the calendar, ICS invites via adapters): a **shared agenda** both sides add to before the meeting, **shared notes**, **manager-private notes** (hidden from every other role in the UI, including HR unless policy says so; the operator and database administrators can still technically read them, so application-level encryption of these notes is a planned hardening), mood/check-in prompts (optional), and **action items that become `work` issues** assigned to either party, then shown in the next 1:1 as follow-ups. Also: goals/OKRs linked to work items, continuous feedback and praise, review cycles (self, manager, peer, 360) with templates, growth plans, skip-level meetings, and a per-person timeline combining 1:1 history, goals, feedback and delivery signals (completed issues) as *context*, not as an automatic score. Everything here is `performance` data class with the strictest access rules (see *Sensitive data*).
- **Manager workflows on the event bus:** `one_on_one.action_item.created.v1` → `work` issue; `issue.priority_changed.v1` to P0 → manager notification; `person.offboarded.v1` → open issues reassigned or flagged; `review.cycle.opened.v1` → tasks and reminders for everyone involved.

**Integrate, don't build** (large, well served elsewhere, and heavy on footprint): team chat, video meetings, mailboxes and calendars, e-commerce checkout, full accounting engines, payroll and tax filing, and HRIS features beyond records, onboarding and time off. Each gets an adapter or a documented integration, not a module of ours.

### Money, time and paper

- **Invoices.** Receivable *and* payable (bills) in one module, recurring schedules, credit notes, multi-currency, tax rates (VAT/GST/sales tax) per jurisdiction, payment links (Stripe/PayPal), AR ageing and dunning reminders, PDF output, and — where regulation requires it — structured e-invoice formats (Factur-X/Peppol). Numbering and immutability rules (no editing issued invoices, only credit notes) matter for audits and are part of the module contract. The in-house `invoices` module is the default plan; an OSS invoicer is wrapped only if one passes the licence check (§5), and the `invoices` capability keeps either replaceable.
- **Timesheets.** Time entries per person/project/client with billable and internal flags, weekly submission and manager approval, rates, and export. Wrapped from Kimai or Solidtime first, with approval run by the can-core engine. They matter because they link three modules: projects (where time goes), invoices (what is billed) and payroll (what is paid).
- **Contracts.** Two layers: `documents` signs and stores PDFs; the `contracts` registry knows what each contract *means* — counterparty (a `directory` party), type, value, start/end, auto-renewal and notice period, owner, obligations/milestones, and links to the signed document and related deal, vendor or employee. Reminders before notice dates are the main value (missed renewals are the common, expensive failure).
- **Stock and options (cap table).** An append-only ledger of share classes, issuances, transfers, option grants (strike, vesting schedule with cliff, acceleration), exercises, repurchases and convertible instruments (SAFEs, notes) with conversion modelling; derived views: current and fully-diluted cap table, option-pool usage, per-person vesting status, pro-forma "what if we raise a round", and exports (CSV/PDF) for lawyers, auditors and investors, plus read-only investor and employee portals (an employee sees only their own grants, vesting schedule, exercise window and strike price; exercise requests flow through the approval engine, §3.7). Stock here means **employee stock options and equity** (not physical inventory). Equity law differs by jurisdiction (securities rules, tax treatment, valuations such as 409A in the US) and mistakes are expensive, so `equity` is a **record-keeping and reporting tool, not legal or tax advice**: it computes vesting and dilution deterministically, flags issues where it can, and documents are produced from lawyer-reviewable templates. Cap table data is among the most sensitive we hold (shareholder identities, valuations), so it gets the strictest access roles and audit logging (see *Sensitive data*).

### Recruiting & job-board syndication

Companies currently pay tools like HiBob or Breezy mostly for **"post once, appear everywhere, collect applicants in one place"**. We handle this in-app, in layers that are honest about what the boards allow:

1. **Own careers page (day one, free).** The `jobs` module publishes `/careers` and one page per role on the company site with schema.org `JobPosting` JSON-LD. **Google for Jobs** indexes these directly with no partner agreement, and many aggregators crawl them.
2. **XML/JSON job feeds (access varies per board — verify each).** `jobs` generates standard feeds (Indeed-style XML, plus formats used by Adzuna, Jooble, Talent.com, ZipRecruiter-type aggregators) at a stable URL that boards pull or that we push where the board accepts uploads. Some aggregators accept feeds openly; others (Indeed, and Glassdoor which is operated through Indeed) restrict feeds and APIs to approved partners/ATS vendors and their programmes change, so each board's status is shown in the UI and none is assumed to work without approval. Candidate consent and retention are applied at intake.
3. **API connectors (partner approval required).** LinkedIn, Indeed, Glassdoor and similar only expose posting and apply APIs to **approved partners**; there is no open self-serve API for third parties. We ship connector plugins and the OAuth/credential flow, but the operator (or, later, this project as a registered ATS partner) must hold the access. This is the main external dependency and is not fully in our control.
4. **Paid multiposting bridges (optional).** For boards that stay closed, an adapter to a multiposting/distribution aggregator, using the operator's own account.
5. **Inbound applications.** All routes land in one pipeline: apply-on-site form (via the forms gateway, with resume upload), email-to-apply (inbound-email policy above), and board apply-webhooks where a connector provides them. De-duplicated by party id.

Connectors are plugins behind a small interface, like providers (§3.5):

```go
type JobBoard interface {
    Publish(job Job) (ExternalRef, error)
    Update(ExternalRef, Job) error
    Close(ExternalRef) error
    Applications(since time.Time) ([]Application, error)   // pull model; webhooks optional
    Capabilities() BoardCaps                                 // e.g. feed-only, supports apply, supports sponsored
}
```

Each board has a status in the UI (`feed`, `connected`, `needs partner access`). Pipeline stages, interview scheduling (via the booking module), scorecards, offer letters (via `documents`) and the hire hand-off to `people` are in-app. Footprint target for the whole `jobs` module is tens of MB: it is a small Go service on SQLite/Postgres plus file storage on object storage or a PVC.

### Priority order for v1 modules

1. Landing site + Forms gateway
2. Support inbox
3. Newsletter
4. Analytics
5. CRM
6. Wiki
7. Status page and SSO (Phase 2)
8. Booking, invoicing, e-sign, the work tracker and the rest of the lifecycle modules (Phase 3a/3b, as demanded)

## 6. Core features

### 6.1 `make` interface and onboarding

```
make init        # asks for name, domain, provider; writes company.yaml (or edit it by hand)
make up          # provision + bootstrap + deploy (≈5 min); prints the instant URL and admin login
make dev         # local k3d cluster, same charts
make enable crm  # / make disable crm
make status      # health, RAM vs budget, versions, URLs
make backup | restore
make email       # connect the outbound relay and inbound mail (post-`up`)
make down        # destroy the server (asks first; offers a final backup)
make doctor      # check local prerequisites and provider credentials
```

`make up` ends by printing: the live URL, the admin URL and one-time credentials, the DNS records still to add (if no DNS plugin), and the **recovery kit** (§6.5). Phase 2 adds a browser wizard on `admin.<domain>` for everything after `make up` (templates, modules, DNS), so the day-to-day flow needs no terminal.

### 6.2 Landing & SPA builder

- Section-based templates (hero, features, pricing, FAQ, waitlist/contact CTA), content in `company.yaml`/Markdown, brand colours/logo/fonts as tokens.
- Rendering is done by the Go renderer library shared by the `can` CLI and can-core: no Node on the server, no build step, milliseconds to publish. Advanced sites can use Astro/Hugo built in CI and pushed.
- Output is static HTML/CSS/JS; optional SPA island for interactive bits (pricing calculators, signup flow).
- Built-in SEO basics: sitemap, OG tags, robots, structured data, fast by default.
- Forms are plain `<form>` posts to the gateway (works without JS); enhanced progressively.
- Future: visual/AI-assisted editor ("describe your company → draft site").

### 6.3 Admin dashboard (`admin.<domain>`)

- Module store: enable/disable, configure from `settings.schema.json`, see status/health/RAM against budget.
- Single login; deep links into each module (SSO where possible).
- Unified inbox of "things needing attention": new leads, open tickets, failing health checks, expiring certs/domains, failed backups.
- Backup status, restore button, update-available badges.

### 6.4 CLI (`can`)

```
can init                 # create company.yaml
can up | down | status   # provision/bootstrap/deploy, destroy, health
can dev                  # local k3d cluster
can module list|enable|disable|uninstall <id>   # uninstall: export, then keep|purge
can module add <ref>                            # Phase 3
can apply                # reconcile company.yaml -> cluster
can pull                 # write UI-made changes back to company.yaml
can backup | backup verify | restore <snapshot> | restore --kit <file>
can upgrade [<id>]       # pinned, Helm rollback on failed readiness
can export | import <file>   # portable data export / import (per module or whole company)
can doctor               # check local prerequisites and provider credentials
can email                # connect outbound relay + inbound mail
```

The Makefile targets in §6.1 call these.

### 6.5 Operations

- **Backups:** a bucket is requested by `make init`; **without one, backups are off and `make status` warns loudly**. Nightly, can-core runs the per-module `backup` hooks (consistent dumps, plus a SQLite `.backup` of its own state and the applied `company.yaml`) into a dumps directory on the host, which is a **hostPath mount on can-core only** (see its privilege list in §3.1). Backup hooks run as Jobs in the module's namespace and HTTP-PUT their dump to a module-API endpoint on can-core, authenticated with the projected token; can-core writes the dump files itself, so module Jobs never get hostPath (logs are line-oriented and rotated, so they aren't used as a transport). At the start of each run can-core deletes the completion marker and at the end writes it with a run id and timestamp; the timer refuses a marker older than ~24 h. SQLite files on PVCs (forms gateway, Uptime Kuma, …) are excluded from the raw PVC backup and covered by their dumps. A **host systemd timer** (installed at bootstrap, which also installs `restic` and `sqlite3` on the host) then runs `restic` (encrypted) over that dumps directory, the non-database PVC data under `/var/lib/rancher/k3s/storage` (databases are covered by their dumps, not raw files), Secrets exported by can-core, and host-level state: a `.backup` copy of the k3s SQLite datastore (`server/db/state.db`; `etcd-snapshot` doesn't apply to SQLite), the node token and the secrets-encryption key (only needed for a raw datastore restore; a normal restore re-creates Secrets through the API). The cluster itself is declarative, so a restore is, in order: (1) provision → k3s → can-core started in **restore mode**; (2) restic restores the **dumps directory first** (applied `company.yaml`, can-core state dump, Secrets export, database dumps) onto the host, and can-core imports its own state from it (admin credentials and TOTP seed are therefore preserved, not regenerated) and re-creates the exported Secrets (module app keys, HMAC secrets), creating the `can-data` and `mod-<id>` namespaces for them (labelled as can-core-managed; `can apply` and Helm adopt existing namespaces and Secrets), then leaves restore mode — `can apply` reuses existing Secrets and never regenerates them; (3) `can apply` runs from that restored `company.yaml` (so a kit-only restore from a new laptop works; `can restore --kit` also writes `company.yaml` back into a new company repo directory) and recreates the modules and their empty PVCs; (4) in a restore apply the `install` seeding hook is skipped, and each module's `restore` hook is **replace-all**: it drops and recreates the module's database, loads the dump, and leaves the module scaled to 0 until step (6) (SQLite-on-PVC modules are restored from their dumps the same way); (5) restic restores the PVC directories (workloads scaled to 0 for the copy, after the hooks have finished); (6) with workloads still scaled to 0, the erasure tombstone ledger is fetched (it is encrypted with a key derived from the restic password, so it is readable at this point) and the `purge(subject, class)` hooks re-run against the now-restored data; can-core also re-applies the tombstones to its own restored state (audit-log salts and per-subject encryption keys stay destroyed; the search index and portal credential store are purged for erased subjects); only then are workloads scaled up. The ledger is uploaded by the host timer after each backup run (and immediately on an erasure when bucket credentials are reachable), from a source file written by can-core; it lives outside the snapshot prefix, so `restic forget`/`prune` never touch it. local-path directories are named `pvc-<uid>_<namespace>_<claim>` and the UIDs differ on a fresh cluster (and are only created once a pod is scheduled), so the host-side `can restore` step, run over SSH, scales each module's workloads to 0, copies each backed-up directory into the new volume matched on its `<namespace>_<claim>` suffix, and leaves them at 0 for can-core to scale up after step (6); no module Job needs hostPath. Database dumps are restored by the module's `restore` hook, which pulls the dump with an authenticated GET from a module-API endpoint on can-core (the mirror of the backup PUT). `can backup verify` performs a test restore.
- **Recovery kit:** the restic password and bucket credentials are all that is needed to restore and would be lost with the server; everything else (including the k3s secrets-encryption key, for the rare raw-datastore restore) is inside the restic snapshot, encrypted by that password. `make up` prints a **recovery kit** once, which the operator must store outside the server (e.g. a password manager). `can restore --kit <file>` runs against a bare provider account or a fresh SSH host: it provisions, bootstraps, then pulls the latest snapshot.
- **Updates:** images pinned by digest; `can upgrade` snapshots first, uses Helm atomic upgrades and rolls back on failed readiness. k3s itself is upgraded by re-running the pinned install script (single node, brief downtime; no extra upgrade controller to pay RAM for).
- **Monitoring:** health surfaced in the dashboard; optional Uptime Kuma module; alert by email/push.
- **Security:** auto-TLS, firewall defaults (22/80/443; k3s API not exposed), NetworkPolicies default-deny (pod-to-node traffic is not covered on k3s, §3.1), secrets encrypted at rest, fail2ban/CrowdSec option, per-module database credentials, non-root containers where possible, pod security `restricted` for module namespaces where the upstream image allows (can-system is exempt, §3.1).
- **Multi-company:** each company is its own repo (`company.yaml`) and its own server/cluster; `can` selects it by directory or `--company`. Several companies on one server would need namespacing and a shared ingress and is deferred to Phase 3c.

### 6.6 Clients: web and Android

Supported client platforms are **web** and **Android**. iOS and macOS clients are out of scope for now (no Mac build hosts; Apple tooling requires them); revisit if that changes. The server-side CLI and CI are Linux-only for the same reason.

- **Web (primary).** The admin UI (§6.3) and the portals (§3.7) are responsive web apps served by can-core and the portal pod, usable on phones, installable as a PWA (manifest, offline shell, web push where supported). No separate Node runtime in production (§7).
- **Android (companion app).** A native app (Kotlin, Jetpack Compose) for the on-the-go slice of the product: the attention inbox and push notifications, approvals (leave, expenses, purchase orders, offers), timesheets, 1:1 agendas and action items, work-tracker issues assigned to me (including quick create/update of stories by managers), and the employee/investor views from the portals. It talks only to the same documented HTTPS APIs as the web UI (no private back door), authenticates with passkeys or magic links plus per-device tokens that an admin can revoke, and keeps sensitive data off the device where possible (no offline copies of salary, equity or performance data; screen-capture blocked on those screens). Push uses UnifiedPush or FCM, chosen per company, because FCM routes metadata through Google.
- **API first.** Every client feature is an API feature first, with an OpenAPI description; the Android app, the web UI and integrations are all consumers. Build and test for Android happen on Linux CI (Gradle, emulator or Robolectric/unit tests), so no extra build hosts are needed.

## 7. Tech choices (proposed)

| Area | Choice | Why |
|---|---|---|
| Runtime | k3s, single node, SQLite datastore | Smallest conformant Kubernetes; declarative; same locally and in prod |
| Local dev | k3d (k3s in Docker) | Fast, disposable, same k3s version |
| Ingress / TLS | Traefik (bundled) + built-in ACME | No extra component; no cert-manager RAM |
| Packaging | Helm charts, rendered by can-core via the Helm Go SDK | Reuse upstream charts; atomic upgrades/rollback |
| Storage | k3s local-path provisioner | Zero overhead; single node |
| Core language | **Go** (single static binary for CLI + can-core) | `client-go` and the Helm SDK are Go; low RAM; same language for in-house modules |
| Admin UI | Server-rendered + HTMX, embedded in the can-core binary | No separate Node runtime in production |
| Site rendering | Go templates in a renderer library (CLI + can-core) | No build toolchain; instant publish |
| Databases | Postgres for in-house and Postgres-native modules; MariaDB for the PHP apps that require it; SQLite where supported | One DB server per engine, one DB per module |
| Backups | restic → R2/B2/S3 | Cheap, encrypted, dedup |
| Providers | Direct provider APIs/SDKs behind the `Provider` interface | No Terraform dependency; fastest path to a VM |
| DNS | Cloudflare API (free), provider-native, or manual | Instant records; `sslip.io` hostname until DNS exists |
| Config | YAML + JSON Schema | Human-editable, UI generated from schema |

## 8. Repository layout (proposed)

```
companyinacan/
  SPEC.md  README.md  LICENSE
  Makefile              # make up / dev / enable / … (wraps `can`)
  core/                 # can CLI + can-core (Go): providers/, dns/, render/, api/, ui/
  modules/
    site/  forms/       # in-house
    support-freescout/  wiki-bookstack/  crm-espocrm/
    newsletter-listmonk/  analytics-goatcounter/  analytics-umami/  sso-authelia/  ...
                        # each: module.yaml, chart/, hooks/, settings.schema.json
  templates/            # landing page templates
  schemas/              # module.yaml, company.yaml, capability + event schemas
  deploy/               # cloud-init, k3s config/flags, base manifests, network policies
  dev/                  # k3d config, local-dev helpers
  docs/
  examples/             # sample company repos with fake data
```

## 9. Roadmap

**Phase 0 — Spikes (1–2 weeks).** The risk-burning phase:
- Measure the real idle RAM of k3s + Traefik + CoreDNS on a 2 GB VPS, with and without trimming flags. If it exceeds the §3.2 target, decide: raise the minimum to 4 GB, or evaluate alternatives to full k3s for the smallest tier.
- Time `make up` end to end on Hetzner (and DigitalOcean); tune against the §3.4 budget, including pre-baked snapshots.
- Validate module picks together on 2 GB and 4 GB: RAM with their databases, available/usable Helm charts, SSO feasibility per app, and licences.
- Prove the NetworkPolicy + TokenReview module-auth design with two stub modules.
- Measure the lifecycle-suite candidates (Plane, Kimai/Solidtime, Snipe-IT, Documenso, Lago, Captable) for idle RAM, database engines, private datastores and licences, and re-check can-core's own idle budget with the §3.7 services; confirm which wrapped apps pass the OSI rule.
- Output: updated §3.2, §3.4 and §5 with measured numbers and final picks.

**Phase 1 — Skeleton (MVP, dogfooded on the author's own companies).**
- `company.yaml` + `module.yaml` schemas; `can` CLI and Makefile
- `local` (k3d) and `ssh` providers, then `hetzner`; sslip.io instant URL
- can-core with local admin login (password + TOTP), minimal admin UI (module enable/disable, status, leads) and the module API: capability registry, token introspection, backup/restore dump endpoints, `email.send` and the `leads` capability, so the admin UI can show form submissions. The event bus and event subscriptions follow in Phase 2; until then the forms gateway delivers via a direct configured webhook/email
- Site renderer + forms gateway (email + webhook)
- Modules: support, newsletter, analytics; per-module local logins until SSO arrives
- Backups to object storage, recovery kit, `can restore --kit`
- Footprint CI (idle RAM per module)

**Phase 2 — Business suite.**
- CRM, wiki, status page, simple tasks (Vikunja), SSO (OIDC/forward-auth)
- Shared platform services (§3.7): RBAC with roles as SSO groups, approval engine, notifications/tasks, webhooks, audit-log extensions and the retention/legal-hold engine — all before the first sensitive people/finance module ships in Phase 3
- Admin dashboard (module store, resource meter, attention inbox) and onboarding wizard
- Capability/event wiring (form → CRM lead → support/notification)
- DigitalOcean provider, Cloudflare DNS automation

**Phase 3a — People and work.** `files` (per-class rules, read auditing); the `directory` model and `people` module (reporting lines switch the approval engine to manager chains); `identity.provision` and the hire/leave workflows; `work` (Jira-like tracker; Plane wrapper first on 8 GB+ servers, in-house lean version next — API, service accounts and webhooks from day one) and `performance` (1:1s, goals, reviews); timesheets; booking, e-sign, live chat; employee portal (its equity view is added with `equity` in 3b).

**Phase 3b — Money, paper and hiring.** `invoices` (receivable and payable, e-invoicing), `contracts` registry, expenses, accounting and payroll adapters, bank feeds, subscriptions, `vendors`, customer and vendor portals; `jobs` careers page + feeds first (Google for Jobs needs nothing but the site), then ATS pipeline and board connectors; `equity` (in-house; Captable import) then `board` and `entity` (which read and feed `equity`), investor portal; assets, policies, secrets; unified search, reporting and import/migration tooling.

**Phase 3c — Platform breadth.** The no-code workflow module (Activepieces); module registry from git/OCI; more providers; module hibernation (§3.2); several companies on one server.

**Phase 4 — Go in-house.** Replace modules one at a time where OSS is heavy, awkward, or restrictive: starting with forms (already in-house), then a lightweight CRM and helpdesk sharing one contacts/tickets data model, then wiki/CMS. Unified UI/design system across in-house modules. Smaller footprint is a primary motivation.

**Phase 5 — Experience.** Visual site editor, AI-assisted company bootstrap (copy, site, FAQ, wiki seeds), template marketplace.

## 10. Success criteria

- `make up` on a fresh provider account → live HTTPS site, working contact form (stored and visible in admin) and support inbox UI in **≤ 5 minutes** (excluding custom-domain DNS propagation and email-relay setup), measured in CI-style timing runs on Hetzner.
- `make dev` → working local company in ≤ 2 minutes after prerequisites.
- The starter stack (platform + site, forms, support, newsletter, analytics) idles under 1.5 GB total used RAM (OS, k3s and database engines included) (so it fits a 2 GB server tightly and a 4 GB server comfortably); to be validated in Phase 0 — if not met, the recommended server size is revised, not the budget hidden.
- Enabling/disabling any non-core module never requires editing files by hand and never takes down others.
- Full restore from the recovery kit onto a blank server in ≤ 15 minutes, verified by automated test.
- Swapping a module's provider (e.g. FreeScout → in-house) preserves data via export/import and keeps integrations working.
- A new provider plugin can be written against the `Provider` interface in under a day.

## 11. Risks & open questions

| Risk / question | Mitigation / decision needed |
|---|---|
| **k3s baseline eats the footprint goal** (~0.6 GB before any app; provider minimum plans may be 4 GB anyway) | Phase 0 measures it; trimming flags, SQLite datastore, 4 GB as honest minimum if needed; `static` provider for server-less launches |
| **Pod-to-node reachability** (NetworkPolicy does not cover it on k3s, issue #13) | Open design decision, not yet made: a CNI with host-endpoint policy (Cilium or Calico) at a footprint cost; an ipset/nft set of module-namespace pod IPs maintained by can-core and dropped at the host; or accept reachability and rely on RBAC, `automountServiceAccountToken: false`, authentication on 6443/10250 and `anonymous-auth=false`. Follow-up issue #16 tracks it |
| **Five minutes is aggressive** (VM boot + image pulls dominate) | Parallel pulls, small images, pre-baked snapshots, instant sslip.io URL; the budget is re-baselined from measurements |
| **Provider API drift / many providers** | Narrow `Provider` interface; `ssh` provider covers everything; community-contributed plugins |
| **can-core is a privileged, internet-facing component** | Mandatory TOTP, IP allowlist option, narrow ClusterRole, minimal image; first target of security review |
| **Disk full on local-path** (no quotas) | Disk-usage alerts in the dashboard; `disk_mb` advisory |
| **Single-node means no HA** | Accepted for v1; backups + fast restore are the safety net |
| **Licence obligations** (AGPL/GPL apps; some "open core" with paid features) | Track per-module; keep wrappers separate from upstream code; prefer permissive where equal |
| **Upstream maintenance burden** (breaking upgrades across ~10 apps) | Pin by digest, test upgrades in CI on k3d, Helm atomic rollback |
| **Upstream Helm charts are uneven** (some missing, some heavy) | Ship our own minimal charts where needed; footprint CI rejects bloat |
| **SSO inconsistency** across apps | Accept forward-auth fallback; some apps keep local logins in v1 |
| **Email deliverability** | Never self-host mail; relay + guided DNS (SPF/DKIM/DMARC); outbound via 587/443 |
| **Security surface** (many apps = many CVEs) | Auto-update policy, default-deny NetworkPolicies, per-module DB credentials, k3s API not exposed, security-update notifications |
| **Scope creep** | Strict module boundaries; ship Phase 1 before breadth |
| Open: **Android app scope and stack** — native Kotlin as proposed, or PWA-only first? | Web/PWA ships first; the native app starts once the approvals, notifications and `work` APIs exist (Phase 3a); decide stack and push provider before starting |
| Open: **Branding/naming** — "Company in a Can" final? | Check domain/trademark availability |
| **Job-board access is gated by the boards** (LinkedIn/Indeed/Glassdoor partner programmes) | Layered approach (§5): careers page + JSON-LD and feeds work with no approval; API connectors are plugins the operator enables with their own access; apply for partner status later if the project grows |
| **People data liability** (PII, salaries, candidate data; GDPR/CCPA, local employment law) | Isolation, retention timers, audit log, export/erase, encrypted backups; payroll integrated, never computed; docs state the operator owns compliance |
| **Equity and tax mistakes are costly and jurisdiction-specific** | `equity` is record-keeping and reporting only; deterministic calculations with tests against published examples; lawyer-reviewed document templates; clear disclaimers; strict access control |
| **Cross-cutting services done late are painful to retrofit** (RBAC, approvals, retention) | Designed in Phase 2 *before* any sensitive module; manifest fields (`permissions`, `approvals`, `data_classes`) required from the first people/finance module |
| **Jira-like OSS trackers are heavy** (Plane ≈ multi-service, ~1.5–2 GB) | Define the `work` capability and API first; wrap Plane only for ≥8 GB servers (it brings private datastores — Postgres, a Redis-type cache, a message queue, object storage; verify — which a module may run inside its own namespace as a counted exception to the shared-database rule); small servers use Vikunja or the in-house lean `work`; measure in Phase 0 |
| **Performance data is highly sensitive and easy to misuse** | Manager-private notes, strict RBAC, retention rules, audit log; delivery signals shown as context, never auto-scored |
| **Portals and search/report copies widen the attack surface** | Portal in its own unprivileged `can-portal` namespace holding no business data (only external-user auth state); sensitive classes excluded from the search index and computed at query time; inbound webhooks verified; see *Erasure semantics* |
| **Wrapped apps can't enforce fine-grained RBAC or audit reads** | Sensitive classes (people, equity, performance, payroll) are in-house only; wrappers get coarse SSO-group roles |
| **Scope explosion** (HR/finance suites are huge) | Lifecycle map guides priority; in-house modules stay small and share the party model; integrate (payroll, accounting) where regulation makes building unwise |
| Open: **Payments** — Stripe links only, or a billing module in v1? | Defaults to Stripe/PayPal links in v1 |
| Decided: **one company per server** in v1 | Shared server deferred to Phase 3c (§6.5) |
| Decided: **single-node k3s**, Go core | Revisit only if Phase 0 shows the baseline is untenable |

## 12. Open-source plan

- **Licence (decided):** Apache-2.0 for `core/`, schemas, templates and in-house modules (see `LICENSE`). It maximises adoption and leaves the door open to a later commercial layer (hosting, support, premium modules) without relicensing. AGPL-3.0 was considered and rejected: more protection against closed hosted forks, but it deters contributors and users.
- **Contributions:** not accepted until the repo is public and `CONTRIBUTING` exists. Then DCO sign-off (not a CLA) by default. If relicensing flexibility is wanted for a future commercial edition, adopt a CLA *before* accepting the first external PR.
- **Third-party modules:** upstream apps keep their own licences; wrapper manifests/charts/hooks in this repo are Apache-2.0. Module manifests record the upstream licence; CI fails if one is missing.
- **Repo hygiene from day one:** `README`, `LICENSE`, `CONTRIBUTING`, `CODE_OF_CONDUCT`, `SECURITY.md`, issue/PR templates, CI (lint, schema validation, module smoke tests and footprint checks on k3d), no secrets or personal company data in git (use `examples/` with fake data).
- **Later (only if it proves good):** managed hosting, paid support, premium/enterprise modules, a hosted module registry. These must live outside the open core and never degrade the self-hosted experience.
