# Feature Specification: Public access (Epic E03f)

**Branch**: `epic/E03f-public-access` | **Date**: 2026-11-26 | **Status**: built; unit tested with
fakes; cluster behaviour checked only by the advisory `module-routing` CI job

Closes the gaps listed by [`040-module-routing`](../040-module-routing/spec.md). Operator guide:
`docs/module-routing.md`. SPEC.md references: 3.1 (Traefik, platform base), 3.4 (instant URL, DNS).

## User scenarios

1. **DNS follows exposure.** An operator sets `modules.work.expose.ui: true` and runs `can up`. The
   record for `work.<domain>` is listed, and created when Cloudflare is configured. `can status` says
   whether it resolves.
2. **Typing the address.** `http://acme.example` lands on `https://acme.example`.
3. **Guessing and floods.** One address cannot try passwords or hammer anonymous pages without
   limit; people signed in and API clients notice nothing.
4. **Sending an invoice.** With `modules.finance.expose.public: true`, a link finance issues opens
   for the customer; nothing else of finance is reachable.
5. **Seeing what is public.** The module page shows what is exposed and where.

## Functional requirements

- **FR-001** `can up` (and `--plan`), `can status` and the setup wizard MUST list a record for every
  module host company.yaml and the manifests put on the internet.
- **FR-002** A DNS plugin that creates records MUST NOT change, adopt or add to a module name that
  has an address or CNAME record it did not create, and MUST delete only records it created. A
  conflict on a module name MUST NOT fail the run. A record can created that someone since pointed
  elsewhere MUST be left.
- **FR-003** On a TLS install, plain-HTTP `GET` and `HEAD` MUST redirect to HTTPS. Other methods MUST
  NOT. The redirect MUST NOT affect TLS-ALPN issuance and MUST be absent in the plain-HTTP fallback,
  including after an earlier TLS run.
- **FR-004** Sign-in and anonymous routes MUST have a per-address rate limit and in-flight limit.
  Signed-in pages and APIs MUST NOT be limited at the edge.
- **FR-005** The address used for any limit MUST NOT be choosable by an internet client: the edge
  uses the TCP source; modules use the last `X-Forwarded-For` value, which Traefik writes. Traefik
  MUST NOT be configured to trust forwarded headers.
- **FR-006** Services behind a routed path MUST count failures per client, not per ingress pod
  (including the people API and the careers application limit).
- **FR-007** finance's signed links MUST be routed only on explicit `expose.public: true`, only over
  TLS, only `GET`/`HEAD` of `/public/invoices/`, with HSTS, nosniff, frame denial, no-referrer,
  no-store and no-index headers.
- **FR-008** Chart-made public routes MUST carry HSTS and nosniff; in-house ones (careers) also
  `X-Frame-Options: DENY`.
- **FR-009** The module page MUST show each public surface, whether it is on and its address, and
  MUST NOT change it.
- **FR-010** The limits MUST be switchable off for installs behind a proxy (`--no-edge-limits`,
  `dns.proxied`, `expose.edge_limit: false`).

## Decisions, and why

| Decision | Reason |
|---|---|
| Invoice links are off unless `public: true` is written | `public` defaults to on elsewhere because that is what those modules already did; finance did not, and an upgrade must not publish it |
| The redirect is a lowest-priority route, not an entry point setting | The plain-HTTP fallback and dev serve on `web`; removing a route restores them |
| Temporary redirect, `GET`/`HEAD` only | A browser must not remember it if the install falls back; a client posting secrets over HTTP should fail, not be quietly forwarded |
| Limits only on sign-in and anonymous routes | Everything signed in is limited per credential by the service; a per-address edge limit there would punish offices and integrations behind one address |
| SCIM and wrapped apps get headers but no limit | SCIM is authenticated API traffic; wrapped apps' traffic cannot be guessed |
| Records are made by `can up` only | The DNS token and the record of what can created live on the operator's machine; can-core gets neither |
| Records are not removed when exposure is switched off | They point at the operator's own server; `can down` removes them |

## Threat model

**Assets**: DNS records of the company's zone (mail and other services included); sign-in surfaces;
invoice data behind a signed link; availability for legitimate users.

**Actors**: anonymous internet client; a customer holding a link; a signed-in user; a hostile module
manifest; an operator behind a proxy (trusted, may misconfigure).

| Threat | Mitigation | Residual risk |
|---|---|---|
| `can` overwrites a record the company uses (`jobs.` pointing at a vendor) | Module names with a foreign A, AAAA or CNAME are skipped; `adopt_existing` does not apply to them | Ownership is the `managed-by=can` comment: a record carrying it (copied, or from another can install on the same zone) counts as can's. A wildcard record is shadowed. `adopt_existing` still applies to the domain, `admin` and `help`, as before |
| A hostile manifest makes can create `www` or `mail` records | Hosts come only from `MayClaim` labels (own id or fixed alias) | - |
| Credentials sent over plain HTTP | `POST` and other methods are not redirected; session cookies are `Secure` | A `GET` carrying a token or a signed link is already on the wire when it is redirected. The site and admin routes send no HSTS |
| Redirect used as an open redirect | Same host, path and query; scheme only | - |
| Redirect breaks certificate issuance | TLS-ALPN on 443; nothing on 443 changes | Not exercised with a real CA |
| Password guessing | Edge 1/s per address on sign-in, plus the services' own limits | Distributed guessing; IPv6 clients can rotate addresses within their prefix |
| Sign-in limit bypass by path tricks | Services limit failures themselves; Traefik normalises paths | An unusual encoding that reaches the handler past the login rule is limited only by the service |
| Client spoofs `X-Forwarded-For` | Edge ignores headers; Traefik replaces what clients send; services take the last value of the last header line; contract test that Traefik trusts none | A cluster-internal caller admitted by an operator's own policy can still set it |
| All clients seen as one address (proxy, CDN, a changed ServiceLB behaviour) | Switches (FR-010); documented; `externalTrafficPolicy: Local` pinned by test | If k3s did not deliver client addresses, the limits would be shared by everyone: not verified on a real server |
| One client locks others out | Buckets are per address; services never refuse a valid credential because of failures | Users behind one NAT share a sign-in bucket (burst 30) |
| Flood from many addresses | Out of scope: no WAF, no global limit | Yes |
| Invoice link leaks (search engine, referrer, cache, frame) | no-index, no-referrer, no-store, frame denial; signed, expiring, revocable | Whoever holds the URL can read the invoice until it expires. Traefik access logs, off by default, would record it |
| The link route exposes more of finance | One path prefix, two methods, can-finance's own mux; k3d probe of `/`, `/v1`, `/login`, traversal | The NetworkPolicy admits Traefik on the port that also serves the API: the route is the only bound |
| A module publishes an arbitrary path with `public_link` | Prefix must equal `routing.LinkPrefixes[id]`; schema enum | - |
| Headers break a wrapped app | No frame denial for wrapped apps | HSTS applies to their hosts once visited |
| The module page leaks configuration | Shows only addresses that are public by definition, to signed-in users with `platform.view` | - |

## Out of scope

A DNS token in can-core or record creation from the admin UI; editing `expose` in the admin UI; a
WAF, IP allowlists, global (not per-address) limits; HSTS on the site and admin routes; DNS-01 and
wildcard certificates; per-route limit tuning.
