# Feature specification: Browser end-to-end suite (E19c)

**Epic**: E19c. **Depends on**: the compose stack and its e2e suite (`specs/032-e2e-docs`), the module web
UIs, module single sign-on (`specs/036`), contract signing (`specs/049`).
**Status**: runs against the compose stack with headless Chromium/Chrome, in the advisory CI job
`browser-e2e`. No real phone, no other browser engine, no https.

## Problem

The web UIs had never been opened in a browser. `test/e2e` speaks HTTP, so it cannot see what a browser
decides itself: which cookies are sent (SameSite), what `Origin` and `Sec-Fetch-Site` a form post carries,
what the Content-Security-Policy blocks, whether a page fits a phone.

## What it does

`test/browser` (build tag `browser`) drives Chrome through the DevTools protocol with chromedp, each scenario
at a desktop viewport (1366 x 900) and an Android phone viewport (360 x 780, touch, mobile user agent):

- admin sign-in with password and TOTP; the session cookie is HttpOnly and SameSite=Strict;
- business overview, its "Open module" buttons, and the admin pages; an API token; a catalogue preview;
- Apps launch into work, people, jobs, finance and files; the launch cookie; the launch link refused in
  another browser; a launch from `admin.localhost` refused; the confirmation before another account's
  session is replaced;
- a create flow in each module UI; resume and file upload and download; a share link;
- a contract sent for signature, signed in a browser with no session through the link taken from the SMTP
  sink; the used link refused;
- the public contact form and careers application;
- token sign-in and sign-out of the module UIs;
- form posts from another site and from another origin of the same site, which must end no session.

On every page it fails on: an HTTP error status that was not expected; console errors and warnings,
Content-Security-Policy violations and other issues Chrome reports; missing `lang`, title, viewport, `main`
or `h1`; heading levels skipped; controls, links or buttons without an accessible name; broken ARIA or label
references; text contrast below WCAG AA in the light and dark schemes; and on the phone viewport, horizontal
overflow and controls smaller than 24 by 24 CSS pixels. Pages are reached by the flows and by a bounded crawl
of each UI's own links (one page of each shape).

One Chrome advisory is ignored: `FormInputAssignedAutocompleteValueToIdOrNameAttributeError` (a field named
`name` or `title`).

## Running it

```sh
make e2e-up
make e2e-browser          # E2E_CHROME=/path/to/chrome, E2E_ARTIFACTS=dir for screenshots of failing pages
make e2e-down
```

The run rotates the admin TOTP secret and adds data. The careers form is rate limited per address, so many
runs in a row against one stack end in HTTP 429; recreate the stack.

## Dependencies and licences

Used only by files behind the `browser` build tag: no command imports them, so no release binary, image or
`THIRD_PARTY_LICENSES` entry changes (that file lists wrapped applications and images, not Go modules). They
appear in `go.mod`.

| Module | Licence |
|---|---|
| github.com/chromedp/chromedp, cdproto, sysutil | MIT |
| github.com/go-json-experiment/json | BSD-3-Clause |
| github.com/gobwas/ws, httphead, pool | MIT |

Chrome itself is not distributed: CI uses the one on the GitHub runner.

## Defects found by the first run, and fixed

1. **The public lead form could not be sent from a browser.** The documented embedding fetches the timing
   token with an inline script, which can-static's Content-Security-Policy blocks; the post was refused
   (HTTP 400). can-forms now serves `GET <prefix>/<id>/form`, a page with the token in it and no script.
   Not offered for forms with a CAPTCHA, which still need a page that runs the provider's script.
2. The compose stack could not send a contract for signature (`sign_base_url` unset), and would then have
   had every invitation refused (`external_recipients` unset).
3. `/favicon.ico` was requested and answered 404 on the people, jobs, finance and files UIs, the careers and
   signing pages and a generated site without a logo.
4. Admin UI on a phone: a screen-reader-only table heading and the permission fieldsets widened the page;
   a new API token did not wrap.
5. Finance on a phone: a long email address widened the page (the same style was in people, jobs and files).
6. Targets under 24 pixels on a phone: checkboxes in files, the resume file field of the careers form.
7. can-forms' HTML answers had no `lang` and no `main` landmark.

## Not covered

- A real Android device, installed-app mode, Web Push delivery, screen readers, keyboard-only use.
- Firefox and WebKit.
- https: `Secure` cookies, the `__Secure-` prefix and separate host names per module.
- Public invoice links and payment, OIDC sign-in, the setup wizard's apply step.
- board and entity have no web UI. equity's (`specs/066-equity-ui`) is covered since E17g.
