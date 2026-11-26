# Spec 055: a contact section on the site (epic E04b)

Status: built. User documentation: [docs/site-contact.md](../../docs/site-contact.md).

## Problem

`make up` promises a landing page with working email forms. The rendered site had no form and no
link to one unless the operator wrote a call to action pointing at the hosted page by hand.

## Requirements

1. `site.contact` (`title`, `text`, `form`, `button`, all optional) renders a section and a
   navigation entry that lead to `/_forms/<form>/form`.
2. The site stays without script: its Content-Security-Policy is not loosened.
3. A `form` the forms module does not have, or one with a CAPTCHA, is an error at build and
   validation time. With forms disabled or not public the section is left out, with a warning.
4. `can init` and the example company include it; the example also sets the lead notification
   (`modules.forms.settings.notify_email`, `email.relay`).
5. The hosted page redirects a trailing slash, leads back to the site, and a CR LF counts as one
   character against `max_length`. Forms module 0.3.0.

## Decision: a link to the hosted page

| Option | Why not |
|---|---|
| Inline `<form>` plus a script fetching the timing token | the site would need `script-src` and `connect-src`; a visitor without script could not send |
| Same-origin iframe | `frame-src` on the site and weaker `frame-ancestors` on the gateway (clickjacking); fixed height without script; the answer page opens inside the frame |
| Inline `<form>` without a token | gives up the timing check |

The link costs one click and changes no security header; the page carries the token and the
honeypot as before.

## Verification

Unit and contract tests; the Compose suites (`make e2e`, `make e2e-browser`) send the form from the
landing page; `make-up-e2e` does so on k3d through Traefik and finds the notification in can-core's
outbox. No real server and no real mail relay.
