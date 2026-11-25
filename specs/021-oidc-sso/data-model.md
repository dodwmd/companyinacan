# Data model: OpenID Connect single sign-on (E11b)

Migration `core/server/store/migrations/0004_oidc_sso.sql`.

## admin_users (changed)

| Column | Meaning |
|---|---|
| `auth_source` | `local` (default; every existing row) or `sso`. An `sso` user has `password_hash = '!'` (never verifies) and an empty `totp_secret`; password login treats it like an unknown user |

## sessions (changed)

| Column | Meaning |
|---|---|
| `source` | `local` (default) or `sso` |
| `revalidate_at` | Unix seconds; 0 = never. An SSO session is refused (and deleted) once reached; the browser then signs in again at the provider |

## sso_identities

| Column | Meaning |
|---|---|
| `id` | row id |
| `issuer_hash`, `subject` | the key: first 16 hex characters of sha256(issuer), and the subject claim. Unique together |
| `user_id` | the admin user; unique (one identity per user), cascade delete |
| `email`, `name` | display only, last values seen; never used to find a user |
| `groups_json`, `amr_json` | groups and authentication methods at the last sign-in (for module introspection) |
| `created_at`, `last_login` | Unix seconds |
| `linked_by` | admin username when an owner bound the identity to a pre-existing local account; empty for provisioned users |

External subject string: `oidc:<issuer_hash>:<subject>`. Provisioned username: `sso-<issuer_hash[:8]>-<sha256(subject)[:16]>`.

## rbac_assignments (use)

Assignments made by the sign-in sync have `granted_by = 'sso'`, `scope = ''`. The sync reads and writes only
those rows. Audit actions: `sso.provision`, `sso.login`, `sso.login.failed` (reason code, address), `rbac.sso_sync`
(`before`, `after`, `added`, `removed`, `skipped`, `kept`), `sso.link`, `sso.unlink`, `sso.test`,
`admin.login.refused_local`.

## login_attempts (use)

Key `sso-ip:<address>`: failed callbacks without a valid transaction.

## company.yaml `sso:`

See [contracts/sso-config.md](contracts/sso-config.md).
