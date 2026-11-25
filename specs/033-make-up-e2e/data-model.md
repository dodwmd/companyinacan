# Data model (E24)

## `.can/state.json` (additive; `version` stays 1)

```json
{
  "launch": {
    "phases": {"base": "2026-11-25T12:00:01Z", "images": "...", "company": "...", "core": "...",
               "admin": "...", "site": "...", "routes": "...", "backup": "...", "ready": "..."},
    "images_fingerprint": "<sha256 of name|ref|tarball-sha256 lines>",
    "admin_user": "admin",
    "admin_created": true,
    "admin_creds_shown": false,
    "site_sha256": "<sha256 of the published site.tar.gz>",
    "backup_installed": true,
    "kit_shown": false,
    "mode": "https"
  }
}
```

Never present: passwords, TOTP secrets, restic passwords, bucket credentials, tokens.

## Checkpoint semantics

| Field | Written when | Used for |
|---|---|---|
| `phases.<p>` | a phase finishes | `can status`; `ready` is deleted at the start of every run |
| `images_fingerprint` | images were loaded | skip re-upload of the same set |
| `admin_created` | bootstrap/reset succeeded | informational |
| `admin_creds_shown` | the summary displayed them (terminal + confirmation, or `--print-secrets`) | after this the admin is never touched; before it a re-run re-issues credentials |
| `backup_installed` | timer enabled on the host | skip; `can down` removes the timer |
| `kit_shown` | kit displayed and acknowledged, or written with `--kit-out` | never show twice; before it, a re-run rebuilds the kit from the host's password |

## Release manifest `deploy/release-images.json`

```json
{"schema":1,"version":"v0.1.0","images":{
  "can-core":  {"ref":"ghcr.io/dodwmd/can-core@sha256:<64 hex>"},
  "can-static":{"ref":"ghcr.io/dodwmd/can-static@sha256:<64 hex>"},
  "can-forms": {"ref":"ghcr.io/dodwmd/can-forms@sha256:<64 hex>"}}}
```

## Local images index `build/images/images.json`

```json
{"schema":1,"images":{
  "can-core":  {"ref":"can-core:dev","file":"can-core.tar","sha256":"<64 hex>"},
  "can-static":{"ref":"can-static:dev","file":"can-static.tar","sha256":"<64 hex>"},
  "can-forms": {"ref":"ghcr.io/dodwmd/can-forms:0.1.0","file":"can-forms.tar","sha256":"<64 hex>"}}}
```

## Cluster objects created by `can up`

| Object | Namespace | Phase |
|---|---|---|
| deploy/base (namespaces, quotas, netpols, RBAC, Traefik config, coredns-custom, admission policy) | various | base |
| ConfigMap `company` (key `company.yaml`) | can-system | company |
| PVC `can-core-data`, Service + Deployment `can-core` | can-system | core |
| ConfigMap `site-content` (binaryData `site.tar.gz`), Service + Deployment `site` | can-system | site |
| IngressRoute `site`, `admin` | can-system | routes |
| Middleware `forms-strip`, IngressRoute `forms-public` | mod-forms | routes |
| Namespaces/Deployments of the forms module | mod-forms | created by can-core |
