# Research: OpenID Connect single sign-on (E11b)

## R1. Library or standard library?

Options: `github.com/coreos/go-oidc/v3` (+ `go-jose/v4`), `golang.org/x/oauth2`, or the small subset written
against `crypto/*`.

What is needed: discovery fetch, authorization URL with PKCE, one token POST, JWS verification for RS256 and
ES256 from a JWKS, claim checks. About 600 lines including the limits that matter here.

Why not the libraries: (a) go-oidc pulls go-jose (a large JOSE implementation with JWE, many algorithms and
its own parsing surface) for two algorithms; (b) none of them gives control over the three things this epic
is graded on: the connect-time address check, size caps and no redirects (all would still need a custom
`http.Client`, which can be passed in, but then the rest of their value is small), and the key-refetch
policy (go-oidc's `RemoteKeySet` shares in-flight fetches but, as far as its documented API shows, has no knob to
rate-limit refetches on unknown `kid`, which is the flood we must avoid; not verified against its source here); (c) footprint: binary and RAM were **not measured** for the libraries because the stdlib build already
cost +254 KB and about 1 MiB RSS, so the libraries could only add to it. Licences (Apache-2.0 for both) would
have been fine. Decision: stdlib. Revisit if more algorithms, JWE or several providers are needed.

## R2. Key rotation without a refetch flood

Unknown `kid`: refetch once, shared by concurrent callers (single flight), at most one attempt per 30 s
whether it succeeded or not. Within that window a token with an unknown `kid` is refused, which also means a
real rotation can fail one sign-in for up to 30 s (tested; acceptable and documented). A cached set older
than one hour is refreshed on use; if the refresh fails, known keys keep working up to 24 hours (an IdP
outage should not end sign-ins for keys that were fine a minute ago) while unknown keys are refused. An
initial fetch failure refuses everything (fail closed).

## R3. SameSite and the callback

The transaction cookie must reach the callback, which is a cross-site top-level navigation: SameSite=Lax
(Strict would not be sent). The session cookie stays Strict, as for the password login. Strict cookies are not
sent on a request that is part of a redirect chain started cross-site (browser behaviour; reasoned from
RFC 6265bis, not tested in a browser here), so the callback answers 200 with a page that navigates on by
`<meta http-equiv="refresh">`; the next request is initiated by our own page and carries the cookie. The same
page type serves the provider logout, because `form-action 'self'` (CSP) also blocks a POST from being
redirected to another origin.

## R4. Identity key and account linking

`sub` is the only identifier an OIDC provider guarantees stable and unique per issuer. Email can be changed
by the user, unverified, reused after someone leaves, or asserted by a malicious second provider. Therefore:
key (issuer hash, `sub`); linking to an older local account is an owner action. The issuer is hashed into the
username so a changed or second issuer can never collide with an existing identity.

## R5. Roles

Roles are synchronised, not just granted: `granted_by = 'sso'` marks what the sync owns, so removing a group
removes exactly those assignments and hand-made ones survive. Owner needs the operator's explicit
`confirm_owner_mapping` (validated in `can validate` and re-checked at run time) and is never removed when
it would leave no owner. Modules that are not installed make a mapped role unknown: skipped and reported, not
an error, so a mapping can be written before the module is enabled.

## R6. Session lag

No refresh tokens or back-channel logout. Instead the SSO session carries `revalidate_at`; after it the
session is refused and the browser goes through the provider again, which supplies current groups. Default
1 hour, minimum 5 minutes. Immediate cut-off: disable at the provider and unlink the identity.

## R7. Secret delivery

can-core's Kubernetes permissions deliberately exclude reading Secrets in `can-system`. Rather than widen
them, the client secret is read from a mounted Secret (`CAN_SSO_SECRET_DIR`), optional in the Deployment.
`client_secret_ref` reuses the module `secret-ref` shape and its denylist (`can-*`, `kubernetes.io*`).

## R8. Outbound safety

Checked in the dialer's `Control` hook on the resolved address (DNS rebinding safe); redirects are an error;
`HTTP(S)_PROXY` ignored; https enforced for the issuer, every discovered endpoint and the redirect base;
size caps 64 KiB (discovery), 256 KiB (JWKS), 128 KiB (token), 64 KiB (userinfo); 10 s per call; at most 4
concurrent code exchanges. `allow_insecure_dev` lifts https and the address rule together and is flagged on
the status page. The discovery document's `issuer` must equal the configured string exactly (no
normalisation), so a provider cannot be swapped by a redirect or a stale cache.

## R9. Throttling

Failures without a valid signed transaction are counted per address (`login_attempts` key `sso-ip:<ip>`),
and refuse further such requests only in hard-lock mode (real client addresses behind a trusted proxy), as
for password login. A callback with a valid, unused transaction is never refused for earlier failures:
the QA lesson "never throttle valid credentials" applied to a flow where validity is the signed state.

## R10. Local policy

`local_login` is evaluated after the credentials were verified, so it neither reveals which accounts exist
nor counts as a failure. `disabled` is ignored while no SSO user holds `owner` (never lock everyone out).
`can-core admin` (CLI in the pod) is outside this path.
