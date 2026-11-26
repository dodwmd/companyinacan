# Reaching modules from the internet

What can-core routes through Traefik, and how to switch it on. Specification and threat model:
[`specs/040-module-routing/`](../specs/040-module-routing/spec.md).

> **Status: unit and contract tested; on a cluster only the advisory `module-routing` CI job
> exercises it** (work and forms, Traefik's self-signed certificate, no browser).

## The setting

```yaml
# company.yaml
modules:
  work:
    expose:
      ui: true            # default false: https://work.<domain>
      # api: true         # default false: also /v1 on that host
      # token_login: true # default false: keep the paste-a-token form
  forms: {}               # public intake is on by default: https://<domain>/_forms/<form>
  jobs:
    expose:
      public: false       # default true: take the careers pages off the internet
```

Then `can apply`. The admin UI does not edit `expose`: putting something on the internet is a change
to the file.

| Module | `ui` host | Public by default |
|---|---|---|
| work | `work.<domain>` (`/ui`) | - |
| people | `people.<domain>` | - |
| jobs | `jobs.<domain>` | careers pages and feeds on `careers.<domain>` |
| finance | `finance.<domain>` | - (the public invoice link is not routed) |
| forms | none | `/_forms` on the site hosts |

## What you also need

* **DNS.** Add an A/AAAA record for each exposed host (`work.<domain>` and so on), or a wildcard.
  `can up` and the setup wizard list only the site and admin records.
* **A domain with TLS.** UI and API hosts are `<id>.<domain>` from company.yaml, also while you still
  use the instant sslip.io addresses, so they work only once your domain points at the server. On
  the plain-HTTP fallback nothing but `/_forms` and public pages is routed.
* **Company domain on can-core** (`CAN_COMPANY_DOMAIN`, set by `can up`) for the Apps page.

## What you get

* Only the web UI's own paths are routed. `/v1`, `/healthz`, `/readyz` and metrics stay inside the
  cluster (unless `api: true`, which adds `/v1/`; that includes the HMAC-signed webhook receiver
  `/v1/can/events`).
* TLS with the `letsencrypt` resolver; plain `http://` gets 404.
* HSTS, `nosniff` and `X-Frame-Options: DENY` on UI and API hosts.
* The module runs with its UI enabled, Secure cookies, the client address from Traefik, and no
  paste-a-token form.
* The module's namespace admits Traefik on the routed port only.
* **Apps** in the admin UI offers the module. `CAN_MODULE_UI_URLS` is no longer needed; it still
  overrides the derived address.

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
2. Every module is reconciled once.
3. **Routes you made by hand** to a module UI stop working, because the namespace now admits Traefik
   only on ports can-core routes. Replace them with `expose.ui`.

Servers must run k3s v1.32.2 or later: older releases bundle a Traefik chart that ignores the
certificate resolver. `provider.options.k3s_version` older than that is refused.

## Troubleshooting

| Symptom | Look at |
|---|---|
| 404 on `https://work.<domain>` | `kubectl -n mod-work get ingressroutes.traefik.io`; can-core logs for "not routing" |
| Certificate warning | the DNS record; Traefik logs (ACME) |
| Module failed: "run `can apply`" | `can-routing` names another domain |
| Denied by `can-core-routes` | the host in the message against the table above |
