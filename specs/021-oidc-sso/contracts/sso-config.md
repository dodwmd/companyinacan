# Contract: `sso:` in company.yaml (E11b)

JSON Schema: `schemas/company.v1.schema.json` (`sso`, no additional properties, so a `client_secret:` value
is rejected as an unknown field). Semantic rules: `core/config/sso.go` (`config.ValidateSSO`), reported by
`can validate` as `semantic.sso-*`. Fixtures: `testdata/valid/company-sso.yaml`,
`testdata/invalid/company-sso-*.yaml`.

| Rule | Meaning |
|---|---|
| `semantic.sso-issuer` | https URL, no credentials, query or fragment; no IP literal in a private, loopback, link-local or other non-public range, no `localhost`, `.local`, `.internal` (unless `allow_insecure_dev`) |
| `semantic.sso-client-id` | 1 to 256 characters, no whitespace |
| `semantic.sso-secret-ref` | `name` or `name/key`, a DNS-style name not starting `can-` or `kubernetes.io` |
| `semantic.sso-redirect` | `redirect_base_url` an origin (https, no path), or a company domain to derive `https://admin.<domain>` from |
| `semantic.sso-scopes`, `-claim`, `-email-domain` | token shapes |
| `semantic.sso-mapping`, `-role` | at most 200 groups, 20 roles each; roles built in or `module:role` |
| `semantic.sso-owner-confirm` | a group maps to `owner` without `confirm_owner_mapping: true` |
| `semantic.sso-local-login` | unknown mode; `disabled` without `confirm_disable_local_login: true`; `disabled` without an owner mapping |
| `semantic.sso-open-access` | none of `allowed_email_domains`, `group_roles`, `require_mapped_role` is set and `allow_any_account: true` is not |
| `semantic.sso-revalidate` | duration between 5m and 24h |

can-core re-validates the block when it reads the applied file; an invalid block disables sign-in (the status
page shows the first problem) and leaves local login as it was.
