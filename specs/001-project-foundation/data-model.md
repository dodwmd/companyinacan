# Data Model: Project Foundation (E01)

Schemas are versioned (`schema: 1`); unknown fields are rejected; all problems are reported
together (FR-003, FR-004). Field names follow `SPEC.md` §3.3, §3.7, §4.1.

## CompanyDescription (`company.yaml`, schema `company.v1`)

| Field | Type | Rules |
|---|---|---|
| `schema` | int | required; must be `1` |
| `name` | string | required; 1–80 chars |
| `domain` | string | required; DNS name; reserved example domains allowed |
| `provider` | object | required; `type` ∈ `local`, `ssh`, `hetzner`, `digitalocean`, `static`; `options` free map restricted per type (E06) |
| `modules` | map id → `ModuleEntry` | optional; ids match `^[a-z][a-z0-9-]{1,30}$` |
| `primary` | map capability → module id | optional; value must be an enabled module providing the capability |
| `site` | object | optional; `tagline`, `template`, `brand` (colours, logo path) |
| `backup` | object | optional; `endpoint`, `bucket`, `region`, `secret_ref` (a reference, never the secret) |
| `email` | object | optional; `relay` {`host`, `port`, `secret_ref`}, `inbound` {`mode`: `imap` \| `parse`} |

`ModuleEntry`: `enabled` (bool, default true), `settings` (map, validated later against the
module's `settings_schema`, E03).

**Validation (semantic)**: unique subdomains across enabled modules; exclusive-capability
conflicts without a `primary`; `primary` consistency; no value that looks like a secret
(`secret_ref` only) — a heuristic warning for keys named `password`, `token`, `secret`, `key`.

## ModuleManifest (`module.yaml`, schema `module.v1`)

| Field | Type | Rules |
|---|---|---|
| `schema` | int | required; `1` |
| `id` | string | required; same pattern as module ids; must match directory name |
| `name`, `description`, `category` | string | `name` and `description` required |
| `version` | string | required; semver |
| `provider` | object | required: `type` ∈ `oss-wrapper`, `inhouse`, `external`; for non-`inhouse`: `name`, `licence` (SPDX expression), `upstream` (URL) required; optional `licence_selected` (one accepted alternative of an `OR` expression), `licence_class` ∈ `osi` (default), `source-available`; `upstream_version` string |
| `runtime` | object | `chart` (relative path), `subdomain` (DNS label), `ui` {`service`, `port`} (all required unless the module has no UI: then `ui` omitted) , `values` (relative path, optional) |
| `requires` | object | `database` ∈ `postgres`, `mariadb`, `sqlite`, `none` (required); `capabilities` list; `sso` ∈ `oidc`, `forward-auth`, `none` |
| `resources` | object | `ram_mb` int >0 (required); `ram_limit_mb` int ≥ `ram_mb` (default 1.5×, computed); `disk_mb` int ≥0 advisory |
| `provides` | list | each: `capability`, `schema` (`name.vN`), `service`, `port` |
| `consumes` | list | capability names |
| `publishes` | list | each: `event` (`…vN`), `schema` (relative path) |
| `subscribes` | list | each: `event`, `service`, `port`, `webhook` (path), `optional` bool |
| `permissions` | list | action names `^[a-z][a-z0-9-]*(\.[a-z0-9-]+)+$` |
| `roles` | map role → permissions | permissions must be declared in `permissions` |
| `approvals` | list | each: `type`, `schema`, `default_chain` list |
| `data_classes` | list | each: `class`, `retention` (duration like `7y`, `12m`, `0` = none, or `legal`), `sensitivity` ∈ `standard` (default), `sensitive` |
| `settings_schema` | path | optional |
| `hooks` | map | keys ∈ `install`, `configure`, `backup`, `restore`, `upgrade`, `export`, `import`, `purge`, `uninstall`, `image`; values relative paths (or image ref for `image`) |
| `healthcheck` | string | URL path |

**Validation (semantic)**:
- `ram_limit_mb ≥ ram_mb`; `roles` reference declared `permissions`; each `subscribes[].service`
  and `provides[].service` is non-empty; event names end `.vN`.
- `data_classes[].sensitivity: sensitive` only when `provider.type: inhouse` (Constitution V).
- `provider.type != inhouse` requires `provider.licence` (the licence check, §4 of the spec,
  enforces acceptance separately).
- Capability names unknown to `schemas/names.yaml` produce a warning, not an error (R6).

## Problem (validation result)

| Field | Meaning |
|---|---|
| `file` | path as given |
| `line`, `column` | 1-based, 0 if unknown |
| `path` | JSON-pointer-like field path (`/resources/ram_mb`) |
| `rule` | stable id, e.g. `schema.required`, `schema.additionalProperties`, `semantic.ram-limit`, `semantic.sensitive-requires-inhouse`, `licence.missing`, `licence.not-accepted` |
| `severity` | `error` or `warning` |
| `message` | human sentence |

Text form: `file:line:col: path: message [rule]`. JSON form: array of the objects above.

## Fixture and Expectation (test data)

`testdata/valid/<name>.yaml` must yield zero errors. `testdata/invalid/<name>.yaml` has a sidecar
`<name>.expect.yaml`: a list of `{rule, path, line?}`; the test asserts the set of reported
problems equals the expectation (no extra errors, none missing).

## AcceptedLicences (`schemas/accepted-licences.yaml`)

`schema: 1`; `accepted`: list of SPDX ids; each entry may carry a `note`. Reviewed like code.

## ThirdPartyNotice (`THIRD_PARTY_LICENSES`)

Generated Markdown: header, one table of accepted modules (module id, wrapped app, licence,
upstream), and a separate "Source-available (opt-in)" table. Sorted by module id; no dates.
