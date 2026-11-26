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
holds at most four: a fifth login in the same browser expires the older ones: a tab still waiting at the IdP then fails its callback with the generic refusal and the user starts again) and redirects to the IdP. The cookie is signed
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

## Opening module web pages from the admin UI (E11c)

The admin UI has an **Apps** page. It lists the enabled modules that have a web page and in which your roles
give you at least one permission (the same rule personal access tokens use, including `work.admin` implying
`work.read`). **Open** is a POST: can-core mints a one-time *launch ticket* for that one module and the browser
is sent to the module's sign-in address. The module starts a session of its own for you, with exactly the
permissions you hold in it at that moment. You never see or paste a token, and the module never receives a
long-lived credential. Design, threat model and tests: [`specs/036-module-ui-sso/`](../specs/036-module-ui-sso/).

**The link works only in the browser that opened it.** With the ticket, can-core sets a 60-second nonce cookie
(`__Secure-can_launch_<module>`, `HttpOnly`, `SameSite=Strict`, `Domain=<company domain>`) and the module needs
both. A link copied or mailed to someone else signs nobody in (this stops an attacker from signing a victim in
to the attacker's account). The module's page names the account before you continue, and if the browser is
already signed in there as someone else it asks you to confirm the switch. So open the admin UI at its
configured address, on the same domain as the modules: reached by IP or port-forward, launching is refused. Do
not host untrusted apps on subdomains of the company domain (they could plant such a cookie).

Changing or resetting your password or TOTP, revoking one of your admin sessions, unlinking SSO and ending an
account also end that person's module sessions and unused tickets. After an SSO sign-in a module session ends
no later than the admin session's revalidation deadline.

```
admin UI  --POST /apps/work/launch (session + CSRF)-->  can-core mints ticket (60 s, once, for "work") + nonce cookie
browser   --GET  https://work.<domain>/ui/login/ticket?t=cant_...   -->  work peeks, names the account (nothing spent)
browser   --POST /ui/login/ticket (CSRF)                            -->  work -> can-core POST /v1/tickets/redeem {ticket, nonce}
work      <-- {subject, name, permissions, session handle}          <--  can-core (audience = the calling module)
browser   <-- 303 /ui/ + work's own session cookie                       (clean URL, no ticket)
work      --POST /v1/tickets/revalidate every 30 s ... 5 min-->  can-core: still allowed? with what now?
```

Properties (all tested): the ticket works once, for one module, for at most 60 seconds, and only its hash is
stored; a GET (link scanner, prefetch) never spends it; every refusal is the same `{"active": false}` and
failed redeems are slowed per module while valid ones never are; the landing path is a strict allowlist and
the address of the module comes from configuration, never from the request; a disabled account, a removed role,
an SSO unlink or an access revocation ends the module session at the next revalidation (1 minute for
privileged sessions in people, jobs and finance, 5 minutes for the others, 30 seconds in work); a module session
never outlives the identity provider's revalidation deadline; finance's party erasure needs an admin sign-in at
most 5 minutes old (can-core sends `max_age=31536000` so the provider reports `auth_time`); minting is
limited to 30 per person and minute; mint and redeem are audited with ids only. Logging out of a module ends
that module's session only.

**Configuration.**

| Setting | Meaning |
|---|---|
| module manifest `runtime.web_login {base_path, via_subdomain}` | the module's UI accepts tickets. Declared by work, people, jobs and finance. An app without it (wiki, CRM, ...) is never offered. With `via_subdomain: true` the address is `https://<runtime.subdomain>.<company domain><base_path>` |
| `CAN_MODULE_UI_URLS` (can-core env) | `work=https://work.acme.example/ui,people=https://people.acme.example`: the operator's address (https, on the company domain or below, not the admin host); wins over the manifest; a bare origin gets `base_path` appended |
| `CAN_COMPANY_DOMAIN` | module UI hosts must be this domain or below; the admin host is refused; it is also the launch cookie's `Domain`. can-core warns at start-up when it is missing or a single label, and about unacceptable `CAN_MODULE_UI_URLS` entries and entries that name a module that is not installed |
| plain-http development (`CAN_COOKIE_SECURE=false`) | the cookie has no `__Secure-` prefix; run the admin UI and the module on the same host name (for example both on `localhost`, different ports). `admin.localhost` + `work.localhost` cannot share a cookie |
| `ui.token_login` (each module's `ui:` block / env, see its UI doc) | `true` (default, for compatibility) keeps the paste-a-token form; set `false` once SSO is used so the only way in is a ticket. Scripts keep using tokens against the APIs |

**Not routed by default.** The charts shipped today do not expose work, people or finance on a public host
(jobs exposes only `/careers`). Until you route a module's UI and set its address in `CAN_MODULE_UI_URLS`, it
does not appear under **Apps**; the paste-a-token page keeps working. The people, jobs and finance web UIs also
need `ui.enabled: true`. A module installed before this version is offered after its next upgrade or re-enable. The compose stack offers work, people, jobs and finance in
Apps when the admin UI is opened at `http://localhost:8180` ([local-compose.md](local-compose.md#modules-access-tokens-and-single-sign-on)).

**Not verified:** no browser or cluster run. See `specs/036-module-ui-sso/tasks.md`.

## For modules

People who sign in through SSO use modules with **personal access tokens** (`/tokens` in the admin UI,
`specs/016-module-wiring`): an SSO user is an ordinary `user:sso-...` subject, so the PAT machinery (issue only
permissions you hold, introspection bound to one module, expiry, revocation, revocation when the issuer loses
the permission) applies unchanged. Modules need no SSO-specific code.

(Scripts and other tools still use tokens. For the module web pages, use the Apps page above.)

**The admin UI session is never a module credential.** `POST /v1/tokens/introspect` does not recognise the
value of the `can_session` cookie (an earlier draft did; it was removed): a module that was handed it could replay
it as a cookie and act as the user in the admin UI, and a token given to one module would work at all of
them. A session value presented there is just an unknown token and answers `{"active": false}` (tested with a
live session). The identity data (`email`, `groups`) that a session-token introspection would have carried is
therefore not offered to modules; a module that needs the person's email gets it from its own directory
(`people`), not from the login.

- The `oidc:<issuer-hash>:<sub>` identity stays inside can-core (audit, status page).
- Removing an SSO user (*Unlink*, or *Link* which replaces the provisioned stand-in) **revokes the personal
  access tokens that user issued** in the same transaction. Usernames of provisioned users are derived from
  (issuer, subject), so without this a person who signed in again would have got their old tokens back.
- Local accounts cannot be created with a name starting `sso-` (`can-core admin bootstrap`, store level):
  that prefix belongs to provisioned users, so a local account can never collide with, or be mistaken for, one.

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
