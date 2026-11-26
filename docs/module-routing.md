# Reaching modules from the internet

What can-core routes through Traefik, and how to switch it on. Specifications and threat models:
[`specs/040-module-routing/`](../specs/040-module-routing/spec.md) and
[`specs/045-public-access/`](../specs/045-public-access/spec.md).

> **Status: unit and contract tested; on a cluster only the advisory `module-routing` CI job
> exercises it** (work, forms and finance's link path; Traefik's self-signed certificate, no
> browser, no ACME, no real DNS).
>
> **jobs does not yet start when can-core installs it** (it requires `company.name` and
> `company.base_url`, which nothing sets), so the careers pages are not live on a cluster; their
> routing and headers are unit tested only.

## The setting

```yaml
# company.yaml
modules:
  work:
    expose:
      ui: true            # default false: https://work.<domain>
      # api: true         # default false: also /v1 on that host
      # token_login: true # default false: keep the paste-a-token form
      # edge_limit: false # default true: rate-limit sign-in and anonymous routes
  forms: {}               # public intake is on by default: https://<domain>/_forms/<form>
  jobs:
    expose:
      public: false       # default true: take the careers pages off the internet
  finance:
    expose:
      public: true        # invoice links; OFF unless written
```

Then `can apply`. The admin UI does not edit `expose`: putting something on the internet is a change
to the file.

| Module | `ui` host | Public by default |
|---|---|---|
| work | `work.<domain>` (`/ui`) | - |
| files | `files.<domain>` | share-link downloads on `files.<domain>/s/` |
| people | `people.<domain>` | - |
| jobs | `jobs.<domain>` | careers pages and feeds on `careers.<domain>` |
| finance | `finance.<domain>` | signed invoice links on `finance.<domain>/public/invoices/`, only with `public: true` |
| forms | none | `/_forms` on the site hosts |

## What you also need

* **DNS.** An A/AAAA record for each exposed host (`work.<domain>` and so on), or a wildcard. See
  "DNS records" below.
* **A domain with TLS.** UI and API hosts are `<id>.<domain>` from company.yaml, also while you still
  use the instant sslip.io addresses, so they work only once your domain points at the server. On
  the plain-HTTP fallback nothing but `/_forms` and public pages is routed.
* **Company domain on can-core** (`CAN_COMPANY_DOMAIN`, set by `can up`) for the Apps page.

## Warning: `api: true` on files publishes destructive routes

`modules.files.expose.api: true` puts **all of `/v1`** on `files.<domain>`. That includes **`POST /v1/purge`**,
**erasure** (`POST /v1/subjects/{subject}/erasure`), **legal hold** (`PUT /v1/files/{id}/hold`), the
**retention run** (`POST /v1/retention/run`) and the audit log. They still need a token with `files.admin`, and
the service token `CAN_FILES_API_TOKEN` has it: whoever holds one can destroy files from the internet. The
restore-only credential, which ignores legal holds, is refused through any proxy. The web UI (`ui: true`) does
not need `api: true`: leave it off unless a program outside the cluster has to call the API.

## DNS records

`can up` (also `--plan`), `can status` and the setup wizard list one record per exposed host, from
company.yaml and the module manifests. `can status` also says where each name points now (this does
not change its exit code).

With `dns.provider: cloudflare`, `can up` creates them. For module hosts:

* a name that already has an A, AAAA or CNAME record without the `managed-by=can` comment is
  **skipped** and reported, never changed, even with `dns.adopt_existing: true`;
* a record `can` created follows the server to a new address, unless someone has since pointed it
  elsewhere;
* switching exposure off leaves the record (it points at your own server); `can down` deletes the
  records `can` created.

`can apply` and the admin UI never touch DNS: run `can up` after exposing a module. `can up` reads
company.yaml from disk, so a module enabled only in the admin UI is not listed by it (the wizard
and the module page are). A wildcard record of your own is shadowed by the specific records.

## What you get

* Only the web UI's own paths are routed. `/v1`, `/healthz`, `/readyz` and metrics stay inside the
  cluster (unless `api: true`, which adds `/v1/`; that includes the HMAC-signed webhook receiver
  `/v1/can/events`).
* TLS with the `letsencrypt` resolver. On a `can up` TLS install, a plain `http://` page load
  (GET or HEAD) is redirected to `https://`; anything else gets 404.
* HSTS, `nosniff` and `X-Frame-Options: DENY` on UI and API hosts and on the careers pages. Other
  chart-made routes get HSTS and `nosniff` (wrapped apps may be meant to be framed).
* The invoice link route: `GET`/`HEAD` of `/public/invoices/` only, with `X-Robots-Tag: noindex,
  nofollow`, `Cache-Control: no-store` and `Referrer-Policy: no-referrer`. can-finance's
  `public_base_url` is set to it, so links it issues are absolute.
* The **module page** in the admin UI shows what is on, and the addresses (read-only).
* The module runs with its UI enabled, Secure cookies, the client address from Traefik, and no
  paste-a-token form.
* The module's namespace admits Traefik on the routed port only.
* **Apps** in the admin UI offers the module. `CAN_MODULE_UI_URLS` is no longer needed; it still
  overrides the derived address.

## Rate limits at the edge

Per client address, on each of these routes separately; the answer is 429.

| Routes | Rate | Burst | In flight |
|---|---|---|---|
| Sign-in: module `/login` (work `/ui/login`), admin `/login`, `/api/v1/login`, `/auth/`, `/set-password` | 1/s | 30 | 20 |
| Anonymous: the site, `/_forms`, careers pages, invoice links | 20/s | 100 | 40 |

Signed-in pages and every API (`/v1`, the admin API, SCIM) are not limited here: the services limit
those per credential. The address is the TCP source as Traefik sees it; `X-Forwarded-For` and other
request headers are never read, so a client cannot pick its bucket. Modules take the client address
from the entry Traefik writes (Traefik discards what the client sent).

**Behind your own proxy or CDN** every visitor shares the proxy's addresses. Switch the limits off:
`can up --no-edge-limits` (implied by `dns.proxied: true`) for the site, admin and, through
`can-routing`, all module routes; or `expose.edge_limit: false` for one module. The flag has to be
given on every `can up`, like `--domain-live`.

## What bounds it

A module can claim only `<its id>.<domain>`, a fixed alias (jobs `careers`, provisioning `scim`,
analytics `stats`) and, for forms, `/_forms`. A third-party module with another `runtime.subdomain`
is refused until `core/routing` and the admission policy list it.

The policy `can-core-routes` enforces this in the API server and reads the domain and site hosts from
ConfigMap `can-system/can-routing`:

```sh
kubectl -n can-system get configmap can-routing -o yaml
```

`can up` writes it on every run. `can apply` only writes it when it is missing or names another
domain, and then reads it off the site's route; where there is no site route it writes nothing and
says so. can-core cannot write it. A cluster without it (set up without `can up`) still installs
modules, with no public routes; `expose.ui` and `expose.api` fail there. Changing the domain in the admin UI therefore also needs `can apply` (or `can up`): until
then routed modules fail with a message saying so.

## Upgrading an existing installation

1. Run `can up` (or apply `deploy/base`, then `can apply` on a server that has the site route). Without the new policies can-core
   refuses to install a routed module, and without `can-routing` the API server denies module routes.
2. Every module is reconciled once. jobs and people pods restart: the careers pages and the people
   API now count failures against the client address instead of Traefik's.
3. **Routes you made by hand** to a module UI stop working, because the namespace now admits Traefik
   only on ports can-core routes. Replace them with `expose.ui`.

Servers must run k3s v1.32.2 or later: older releases bundle a Traefik chart that ignores the
certificate resolver. `provider.options.k3s_version` older than that is refused.

## Troubleshooting

| Symptom | Look at |
|---|---|
| 404 on `https://work.<domain>` | `kubectl -n mod-work get ingressroutes.traefik.io`; can-core logs for "not routing" |
| Certificate warning | `can status` (DNS section); Traefik logs (ACME) |
| 429 for many users at once | they share an address (office NAT, proxy): see "Rate limits at the edge" |
| Redirect loop behind Cloudflare | SSL mode "Flexible" talks plain HTTP to the server: use "Full (strict)" |
| Module failed: "run `can apply`" | `can-routing` names another domain |
| Denied by `can-core-routes` | the host in the message against the table above |
