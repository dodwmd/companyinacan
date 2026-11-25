# Single sign-on (OpenID Connect)

can-core can let staff sign in to the admin UI with your identity provider (IdP) instead of a local
password: any standards-following OpenID Connect provider (Keycloak, Authentik, Authelia, Google
Workspace, Entra ID, ...). Epic E11b; design and decisions in [`specs/021-oidc-sso/`](../specs/021-oidc-sso/).

> **Status: built and tested against an in-process fake provider only.** No real IdP has been used.
> See [what is not verified](#not-verified).

The local login (password + TOTP) stays: it is the break-glass path and the only way in when the IdP is
down or misconfigured. SSO is off unless `company.yaml` has `sso.enabled: true`.

## Setup

1. **Register a client at the IdP.** Authorization code flow, confidential client, PKCE allowed.
   Redirect URI: `https://admin.<your domain>/auth/oidc/callback` (the admin UI shows the exact value
   under *Single sign-on*). Scopes: `openid email profile` and, for group mapping, whatever your IdP needs to put
   groups in the ID token (often `groups`).
2. **Put the client secret in a Secret**, never in `company.yaml`:

   ```sh
   kubectl -n can-system create secret generic sso-client --from-literal=client-secret='...'
   ```

   `deploy/can-core` mounts the optional Secret `sso-client` at `/etc/can/sso/sso-client`
   (`CAN_SSO_SECRET_DIR`, default `/etc/can/sso`). A reference `name` means the file `<name>/client-secret`;
   `name/key` means `<name>/<key>`. Names starting `can-` or `kubernetes.io` are refused. (To use another
   Secret, mount it under `/etc/can/sso/<name>`. can-core has no Kubernetes permission to read Secrets of
   `can-system` itself, and none was added.)
3. **Add the block to `company.yaml`** and apply it (`can apply`; `can validate` checks it offline):

   ```yaml
   sso:
     enabled: true
     issuer: https://idp.example.org/realms/acme
     client_id: can-admin
     client_secret_ref: sso-client
     scopes: [openid, email, profile, groups]
     allowed_email_domains: [acme.example]
     group_roles:
       acme-admins: [owner]
       acme-ops: [operator]
       acme-finance: ["finance:approver"]
     confirm_owner_mapping: true      # required because a group maps to owner
     local_login: owner_only          # all (default) | owner_only | disabled
   ```
4. Open **Single sign-on** in the admin UI (needs `rbac.manage`) and press **Test discovery**. It fetches
   the discovery document and the signing keys exactly as sign-in does and says what is wrong.
5. The login page now shows **Sign in with single sign-on** above the password form.

The SSO block is read from the applied `company.yaml` (the mounted ConfigMap) and cached for 30 seconds;
changes need no restart. A secret change is picked up the same way. It is **not** editable from the admin UI
(view and test only): changing who can become owner is a deployment decision.

### Reference

| Key | Meaning |
|---|---|
| `enabled` | Off unless true. |
| `issuer` | Issuer URL. Discovery is `<issuer>/.well-known/openid-configuration` and its `issuer` must equal this string exactly. https only. |
| `client_id`, `client_secret_ref` | The client. The secret is a Secret reference (see above), validated like module `secret-ref` settings. |
| `scopes` | Default `openid email profile`; `openid` is always sent. |
| `redirect_base_url` | Origin of the admin UI. Default `https://admin.<domain>`. |
| `claims.{subject,email,groups,name}` | Claim names (default `sub`, `email`, `groups`, `name`). Use another subject claim only if you know it is stable and unique per person at this issuer. |
| `allow_any_account` | Must be `true` when none of `allowed_email_domains`, `group_roles` or `require_mapped_role` is set: with none of them, **every** account the IdP authenticates may sign in (with no role). `can validate` refuses the block otherwise. |
| `allowed_email_domains` | If set, the email's domain (exact, lowercase, no subdomains) must be listed. Needs a verified email unless `require_email_verified: false`. |
| `require_email_verified` | Default **true**. |
| `require_amr` | Any one of these `amr` values must be in the token (for example `mfa`). Empty: not checked (the IdP's MFA is assumed). |
| `group_roles` | IdP group -> roles. Roles are built-in (`operator`, `auditor`, `owner`) or module-namespaced (`finance:approver`). Not mapped means no role (least privilege). |
| `confirm_owner_mapping` | Must be `true` if any group maps to `owner`: everyone in that group gets full control. |
| `require_mapped_role` | Refuse sign-in for users who end up with no role (default: they sign in with none). |
| `local_login` | `all`, `owner_only` (only users holding `owner` may use the password form) or `disabled` (needs `confirm_disable_local_login: true` and a group mapped to `owner`; behaves as `owner_only`, shown on the status page, while no SSO user holds `owner` **and while SSO is failing**: an unreadable client secret, an invalid block or an unreachable provider never turns the break-glass login off). |
| `revalidate_interval` | Default `1h` (5m to 24h). How long an SSO session lives before the browser is sent back to the IdP. |
| `rp_logout` | Also end the IdP session at logout (needs `end_session_endpoint`; register `https://admin.<domain>/login` as a post-logout redirect URI). |
| `allow_insecure_dev` | Allow `http://` and private addresses for the IdP. Development only; shown in red on the status page. |

## How sign-in works

`GET /auth/oidc/login` creates `state`, `nonce` and a PKCE verifier (S256), stores them in a signed,
HttpOnly, Secure, **SameSite=Lax** cookie `can_oidc_<flow id>` (10 minutes, path `/auth/oidc`; the flow id is
the first 8 characters of the state, so two tabs signing in at once do not overwrite each other, and a browser
holds at most four: starting another expires the older ones) and redirects to the IdP. The cookie is signed
(HMAC), not encrypted: it holds the PKCE verifier and nonce, which are only secret from other people's
browsers, not from the user's own. The signing key is **per process**: restarting can-core (or running a second
replica) invalidates logins that are in flight; the user simply starts again. The callback `GET /auth/oidc/callback`:

1. checks the cookie signature and expiry, compares `state` in constant time and accepts each state once;
2. exchanges the code (client authentication by HTTP Basic, PKCE verifier);
3. verifies the ID token: signature with the IdP's JWKS (**RS256 and ES256 only**; `none`, `HS*` and
   anything with `crit` are refused), `iss` (exact), `aud` (and `azp` when there are several audiences or
   one is present), `exp`, `nbf`, `iat` (30 s skew, and not older than 10 minutes), `nonce` (constant
   time), `at_hash` when present;
4. applies policy: verified email, allowed domains, required `amr`;
5. finds the user by **(issuer, `sub`)**, creating a user without password, TOTP or roles on first sight;
6. syncs roles from groups (below), creates a **new** session token, and answers with a page that moves the
   browser on to `next` (a redirect would not carry the SameSite=Strict session cookie after a cross-site
   navigation).

The user's subject is `oidc:<first 16 hex of sha256(issuer)>:<sub>`; their admin username is
`sso-<8 hex>-<16 hex>` (no email, no name: audit entries and logs carry no personal data). Email and name
are stored on the identity row for display and for modules.

Failures show one message ("Single sign-on was refused"); the reason code (`state`, `exchange`, `token`,
`email_unverified`, `email_domain`, `amr`, `no_role`, `idp_error`) is in the audit log as `sso.login.failed`.

### Accounts are never matched by email

An SSO sign-in **never** lands on an account because the email looks the same: an IdP can hand out
unverified emails, let users edit them, or be a second IdP that someone else controls. To let a person use an
existing local account (for example the first owner), an **owner** links the identity to it in *Single sign-on
> Known identities* (`sso.link`, audited). The provisioned stand-in user and its roles are deleted. Unlink
reverses it (a provisioned user is deleted; a linked local account stays).

### Roles follow groups

On every sign-in the groups from the ID token (or userinfo when the token has none) are mapped through
`group_roles`. can-core adds the missing roles and **removes roles it granted earlier that the groups no
longer give** (one `rbac.sso_sync` audit entry with `before`/`after`). It only touches assignments it made
itself (`granted_by = sso`): roles an owner assigned by hand, and all scoped assignments, are left alone. A role
that does not exist (a module that is not installed) is skipped and listed in the audit entry. The last owner is never removed by
the sync. A manual grant of a role the sync also grants is the same row: if the mapping later drops it, it is
removed; grant such a role with a scope instead.

**Lag.** can-core learns about group changes only at sign-in. A user removed from a group at the IdP keeps
the role in their open session until it is revalidated: at most `revalidate_interval` (default one hour),
after which the session stops being accepted and the browser goes through the IdP again (usually without a
prompt while the IdP session lasts; the IdP then supplies the current groups). To cut a person off at once,
disable them at the IdP **and** remove their identity (*Unlink*), which deletes their sessions. Back-channel
logout is not implemented.

### Local login and MFA

SSO users have no password or authentication code in can-core; their MFA is whatever the IdP enforces.
Set `require_amr: [mfa]` if your IdP reports `amr` and you want can-core to insist. Local accounts keep
password + TOTP. `local_login: owner_only` restricts the password form to owners; `disabled` turns it off
once an SSO owner exists and SSO is working; if the provider, the client secret or the configuration is
failing, it falls back to `owner_only`. In every mode `can-core admin ...` inside the pod (the break-glass CLI) keeps
working. Local sign-in with valid credentials is never throttled; refused sign-ins by policy are not counted
as failures.

Purging a module (needs the current password) is only possible for local accounts.

### Logout

`POST /logout` (CSRF-protected like every state change) ends the can-core session. With `rp_logout: true`
and an `end_session_endpoint`, the response is a page that continues to the IdP's logout (with `client_id`
and `post_logout_redirect_uri`; no `id_token_hint`, which would mean storing the ID token).

## For modules

`POST /v1/tokens/introspect` on the module API distinguishes three token kinds by shape: a projected
ServiceAccount token (a dotted JWT, TokenReview), a **personal access token** (`canpat_...`, `specs/016-module-wiring`),
and an end-user **session token** (the value of the `can_session` cookie: exactly 43 base64url characters, no
dots, never `canpat_`). For a live session created by SSO the answer is `active: true`, `kind: "user"` and the
same shape a PAT gets: `subject` = `user:sso-...` (the can-core RBAC subject, which the modules' `user:` subject
check accepts) and `permissions` = the caller module's permissions the person holds right now, plus `sub`
(`oidc:<issuer-hash>:<sub>`, the stable external identity), `email`, `name`, `groups`, `amr`, `roles` (the
**caller's own** role names: `people:hr` is `hr` for the people module, plus built-ins) and `all_roles` (full
names). Local-password sessions and expired, revoked or unknown tokens answer `{"active": false}`.

- **PATs and SSO users.** SSO users have ordinary `user:sso-...` subjects, so they can issue and use personal access
  tokens exactly like local admins (`/tokens`); PAT introspection is unchanged and stays a separate path.
  The external `oidc:` identity is not a PAT subject; it is only reported as `sub`. Nothing in the modules
  needs to change.
- **Bare-name ambiguity.** `roles` strips the caller's own module prefix, so a module role `hr` and a built-in
  role of the same bare name would look alike to the module. Built-in names (`owner`, `operator`, `auditor`) are
  reserved by can-core and modules may not define them, so no collision arises today; `all_roles` always has the
  unambiguous names.
- **Trust.** Every enabled module may call introspection, and sees `email`, `name`, `groups` and roles for any
  session token it is shown. A session token is the user's session: only hand it to modules you trust with that.

## Security properties (tested)

Replayed callback and replayed code, state mismatch, forged or expired transaction cookie, nonce mismatch,
wrong `iss`/`aud`/`azp`, `alg: none`, HS256 signed with the IdP's public key, `crit`, a token signed by an
unpublished key, expired / not-yet-valid / stale tokens, key rotation (one rate-limited, single-flight JWKS
refetch; a flood of forged `kid`s does not become a flood of requests), JWKS failure (fail closed), unverified
email and email-lookalike takeover, open redirect through `next`, CSRF on logout and on every SSO POST,
session fixation, group removal on next sign-in, owner mapping without confirmation, last owner kept.

Outbound calls (discovery, JWKS, token, userinfo) have a 10 s timeout, size caps (64 KiB / 256 KiB / 128 KiB),
**no redirects**, ignore proxy environment variables, require https, and refuse to connect to private,
loopback, link-local, carrier-grade-NAT and other non-public addresses, checked on the address actually
connected to (so DNS rebinding does not help). `allow_insecure_dev` lifts https and the address check.

A flood of callbacks **without a valid transaction cookie** is throttled (HardLock mode behind a trusted
proxy; otherwise only counted). A callback that carries a valid, unused, signed transaction is never refused
because of earlier failures from the same address.

## Illustrative provider examples

Not tested against these products. Check each provider's documentation for current menus and claim names.

**Keycloak.** Client: *OpenID Connect*, client authentication on, standard flow on, valid redirect URI
`https://admin.acme.example/auth/oidc/callback`, post-logout redirect URI `https://admin.acme.example/login`.
Add a *Group Membership* mapper (claim name `groups`, *Full group path* off, add to ID token). Issuer
`https://keycloak.acme.example/realms/acme`. Keycloak puts `acr` and `amr` only with the corresponding
authenticators configured.

**Authentik.** *OAuth2/OpenID Provider* with signing key (RS256), redirect URI as above, scopes `openid email
profile`; groups come in the `groups` claim with the default *profile* scope mapping. Issuer
`https://authentik.acme.example/application/o/<slug>/` (note the trailing slash: copy it exactly).

**Google Workspace.** Issuer `https://accounts.google.com`. Google returns neither groups nor `amr`, so use
`allowed_email_domains: [acme.example]` (Google's `hd` claim is not used), map nobody to `owner` and assign
roles by hand or link identities. `rp_logout` has no effect (no end-session endpoint).

## Caveats

- Group names are matched exactly as sent. A provider that sends group *ids* needs the mapping written with ids.
- A provider that omits `email_verified` for verified users needs `require_email_verified: false`; combine it with
  `allowed_email_domains` only if the provider guarantees the email's domain.
- Only the authorization code flow, `client_secret_basic`, RS256 and ES256 are implemented. No refresh tokens,
  no `offline_access`, no JWE, no dynamic client registration, no SCIM.
- Roles from IdP groups for **modules** are only as good as the module: modules enforce their own role names.
- One IdP per installation.

## Not verified

- No real identity provider (Keycloak, Authentik, Google, Entra, Authelia) has been tried; behaviour against
  their quirks (claim names, trailing slashes, `groups` size limits, missing `email_verified`) is untested.
- No browser has run the flow: the SameSite handling (Lax transaction cookie, Strict session cookie set before
  an interstitial page moves on) and the meta-refresh interstitial are reasoned from the specifications and
  tested at the HTTP level only.
- Back-channel logout and `id_token_hint` logout are not implemented.
- The mounted Secret path in `deploy/can-core` and the 30 s reload were not run on a cluster.
- Footprint, measured on this build against `origin/main`: stripped can-core binary 19,140,871 to 19,362,055 bytes (+221 KB). Idle RSS (after 20 requests) 47.7 MiB on main, 48.1 MiB with this change and SSO off, **50.6 MiB with SSO enabled after 20 complete sign-ins** against the in-process fake provider (about +2.9 MiB over main; the fake provider ran in another process). can-core's budget is 48 MiB requested / 96 MiB limit: main alone is already at the request, so SSO enabled is above the request and well under the limit. Not measured against a real provider or under load.
