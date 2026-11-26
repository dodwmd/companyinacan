# Feature Specification: Module routing (Epic E03e)

**Branch**: `epic/E03e-module-routing` | **Date**: 2026-11-25 | **Status**: built; unit and contract
tested; the cluster behaviour is checked only by the advisory `module-routing` CI job

SPEC.md references: Traefik as the only ingress and the platform namespaces (3.1), can-core as the
installer (4), same-origin `/_forms` (6.1, E05). Refines what 016 (module wiring), 023/026/027/031
(web UIs) and 036 (launch tickets) left as "no route". Operator guide: `docs/module-routing.md`.

## What existed before

- work, people and finance had no public route; jobs only `/careers`, `/feeds`, `/robots.txt`.
- forms had a route only when `can up` created one; a forms module enabled any other way had none.
- people, jobs and finance web UIs were off, and nothing switched them on in a cluster.
- The admin **Apps** page stayed empty until an operator routed a UI by hand and set
  `CAN_MODULE_UI_URLS`.
- Every module namespace admitted Traefik on every port, routed or not.
- Nothing in the API server bounded which host a route could claim.

## User scenarios

1. **Reaching a UI.** An operator sets `modules.work.expose.ui: true`. `https://work.<domain>` serves
   the tracker's pages, **Open** on Apps signs the person in, and nothing else was configured.
2. **A website form works.** An operator enables forms; `POST https://<site host>/_forms/contact`
   creates a lead.
3. **Taking it down.** The operator sets `ui: false` or disables the module; the route is gone and the
   ingress controller can no longer reach the pod.
4. **A hostile module or control plane.** A chart, a manifest or a compromised can-core asks for the
   admin host, the apex or another module's host; it is refused.

## Functional requirements

- **FR-001** Exposure MUST be an explicit per-module setting in `company.yaml`
  (`modules.<id>.expose`: `ui`, `api`, `public`, `token_login`). The admin UI MUST NOT edit it.
- **FR-002** Defaults: `ui` off, `api` off, `public` on, `token_login` off on a routed UI (see
  "Defaults").
- **FR-003** With `ui` on, can-core MUST route `<id>.<domain>` to the manifest's UI paths only
  (`runtime.public_ui.exact` and `prefixes`), with the UI switched on in the module. `/v1`, health,
  readiness and metrics paths MUST NOT be routed unless `api` is on (which adds `/v1/` only).
- **FR-004** Routes MUST use `websecure` with the `letsencrypt` resolver, like the rest of the stack.
  A plain-HTTP-only stack MUST NOT route any UI or API.
- **FR-005** UI and API routes MUST add HSTS, `X-Content-Type-Options: nosniff` and
  `X-Frame-Options: DENY`.
- **FR-006** The module namespace MUST admit the ingress controller only on routed ports, and on
  none when nothing is routed.
- **FR-007** The Apps launcher's address of a routed UI MUST be derived by can-core;
  `CAN_MODULE_UI_URLS` still overrides it.
- **FR-008** forms MUST be routed at `/_forms` (prefix stripped, priority above the site) on the site
  hosts, by can-core.
- **FR-009** A module MUST be able to claim only `<id>.<domain>`, its fixed alias (`careers` for
  jobs, `scim` for provisioning, `stats` for analytics) and, for forms, `/_forms` on the site hosts.
  The installer MUST check this and a ValidatingAdmissionPolicy MUST enforce it on can-core's
  requests.
- **FR-010** The domain and site hosts the policy compares against MUST NOT be writable by can-core.
- **FR-011** A change of exposure or of routing facts MUST be applied by the next reconcile without
  any other change, and switching exposure off MUST remove the route even if the rollout then fails.

## Defaults, and why

| Setting | Default | Reason |
|---|---|---|
| `ui` | **off** | A UI is a login surface on the internet in front of HR, money or work data. None of these UIs has been checked in a browser or reviewed on a real cluster. Turning one on is one line and a deliberate act. |
| `api` | **off** | The API accepts long-lived bearer tokens and has more surface than the UI. In-cluster callers do not need a route. |
| `public` | **on** | It is what the module is for (a careers page, a contact form), it is designed for anonymous traffic with its own spam and rate limits, and it is what happened before (`can up` routed `/_forms`; the jobs chart routed `/careers`). |
| `token_login` on a routed UI | **off** | Apps and launch tickets are the way in; a paste-a-token form on a public host invites phishing and token reuse. Set `true` to keep it. |

## Threat model

**Assets**: the admin UI session and host; module data behind UIs and APIs; the company site; TLS
certificates and ACME rate limits.

**Actors**: an anonymous internet client; a signed-in person with few permissions; a hostile or
buggy module chart or manifest; a compromised module pod; a compromised can-core; the operator
(trusted).

| Threat | Mitigation | Residual risk |
|---|---|---|
| A module claims the admin host and harvests admin logins | `MayClaim` refuses `admin`; the admission policy refuses it for can-core's requests; the admin route lives in `can-system`, where the policy refuses can-core any route | A cluster administrator can create any route (by design) |
| A module claims another module's host and receives its launch tickets | Only `<id>.<domain>` or the fixed alias, in Go and in the policy | A ticket also needs the browser's nonce cookie (036); that cookie is sent to every host under the domain, so any code on any such host sees the nonce, but not the ticket |
| A module claims the apex or a site path | Only forms, only `/_forms`, only on hosts listed in `can-routing` | `PathPrefix(/_forms)` also matches `/_formsX`, which forms answers with 404 |
| Loose Traefik rules (`||`, `HostRegexp`, two hosts) | Installer allows one `&&` chain with one Host; the policy requires the rule to start with `Host` and contain only Path, PathPrefix, Method and Headers matchers | The policy matches the rule text with a regular expression, not Traefik's parser |
| A compromised can-core widens its own bounds | `can-routing` and the policies are not writable by it (policy `can-core-routing-config`; no admission verbs in RBAC); plain Ingress is refused | It can still create any Middleware or workload inside `mod-*`, and can rewrite Traefik's HelmChartConfig and CoreDNS through the older kube-system grant (`deploy/base/rbac.yaml`, residual risk 3), which defeats routing bounds |
| A route reaches a Service in another namespace | Installer and policy refuse namespaces and `@provider` names; Traefik does not cross namespaces by default; ExternalName Services are refused | - |
| The API or health endpoints leak through the UI route | UI paths are an allowlist, `/v1`, `healthz`, `readyz`, `metrics`, `public` cannot be listed; contract test | A UI prefix also reaches the module's own handling of that prefix; the module's authentication still applies |
| Session theft or downgrade | TLS only; Secure `__Host-` cookies forced on; HSTS; no UI over plain HTTP | HSTS has no `includeSubDomains`; no HTTP to HTTPS redirect (plain HTTP gets 404) |
| Clickjacking and sniffing | `frameDeny`, `nosniff`; the modules set their own CSP | Chart-made public routes (careers) do not get the Traefik headers; the module's own headers apply |
| Another pod reaches a module through the open Traefik allow | The policy lists routed ports only, none when unrouted | A module pod may still send requests to Traefik with any Host (egress allow, pre-existing) |
| Password guessing and floods on a public UI | Modules throttle failed sign-ins by the client address Traefik appends (`trust_proxy`) | No edge rate limit, no IP allowlist, no WAF |
| Client address spoofing once `trust_proxy` is on | Only Traefik and can-core can reach the pod; modules take the last `X-Forwarded-For` entry | A cluster-internal caller admitted by an operator's own policy could spoof it |
| `api: true` also exposes the signed webhook receiver `/v1/can/events` | It authenticates by HMAC with a replay window | Reachable from the internet when `api` is on |
| Certificate requests for hosts without DNS | Only exposed modules ask; exposure is off by default | ACME failures count against rate limits until the record exists |

## Out of scope

DNS records for module hosts; finance's public invoice link; a toggle in the admin UI; edge rate
limiting and IP allowlists; HTTP to HTTPS redirect; DNS-01 and wildcard certificates.
