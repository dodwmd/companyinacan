# 066: equity web UI (E17g)

**Epic**: E17g. **Depends on**: equity (`specs/018`), holder identity (`specs/051`), the UI session kit
(`specs/058`), module single sign-on (`specs/036`), module routing (`specs/040`).
**Status**: unit, contract and session-conformance tested; run against the Compose stack, including the
headless-Chrome suite. Not run on a cluster, behind TLS, on a real phone or with a screen reader.

## Problem

The owner asked for stock options given to employees. `core/equity` can record grants, vesting, exercises,
the share ledger and the cap table, but only through a JSON API: an employee cannot see what they were
granted, and nobody can administer it without scripting.

## What it adds

A server-rendered web UI inside `can-equity`: `html/template`, one embedded stylesheet, no JavaScript, the
CSP of the other module UIs. Sessions are those of `core/uikit/session`; equity runs the conformance suite.

- **A holder** (`/me`): their grants; granted, vested, unvested, exercised and exercisable today; the vesting
  schedule; an exercise request with a review page (quantity, amount due, notice, tick); withdrawal before
  payment; a printable statement per grant and the disclaimer.
- **Administration and finance** (`/company`): entities, share classes, plans and pools; the cap table as of a
  date; stakeholders and their registration; the holder verification queue with verify and attest; drafting a
  grant (periodic schedule), submitting it, its approval status; vesting; the exercise chain; CSV exports.

The route table is [`contracts/ui-routes.md`](contracts/ui-routes.md).

## Requirements

1. **One authorisation path.** Every page calls `Service` methods, which ask `policy.go`. `ui*.go` holds no
   authorisation and no SQL; a test checks the sources. `Capabilities` only chooses which forms to show.
   Separation of duties, holder establishment and roles are the Service's, unchanged.
2. **A holder sees only their own, by verified identity.** The `/me` pages answer for records linked to the
   caller's person **and confirmed by a second person**, whatever other roles the caller has. Anything else is
   the same 404 as a grant that does not exist. This is stricter than `GET /v1/me/grants`, which does not
   require confirmation.
3. **Step-up** before what cannot be undone: grant approval and activation, deleting a draft, recording a
   payment, issuing shares, verifying or attesting a holder. A token session types its token again (checked
   without a cache); a launch-ticket session needs an admin sign-in at most 5 minutes old. Both tick a box.
4. **Sign-in.** A launch ticket from Apps, or a personal access or introspected token. The service account is
   not a person and cannot sign in. Roles of a launch ticket come from permissions exactly as for a personal
   access token (`canauth.go`; the board needs `equity.board`, `specs/068`).
5. **Exact figures.** Quantities are the 1/10000 integers and money the minor units the Service holds,
   formatted and parsed textually. No floating point. Ownership is shown from parts per million.
6. **CSRF** on every post, same-origin check, 64 KiB form bound. Exports are posts, so no link starts one.
7. **Off by default** (`ui.enabled`), routed only with `modules.equity.expose.ui`, which also switches the
   pasted-token sign-in off unless `token_login` is set. Only `/`, `/manifest.webmanifest`, `/company`,
   `/login`, `/logout`, `/me` and `/static` are routed.
8. **Phone and accessibility** to the rules of the other UIs (static checklist, contrast, browser suite).

## What the UI cannot do on a stock `can` install

These follow from the policy and the wiring as they are. The UI does not loosen either.

- **Grant approval** needs the `board` role. Since [`specs/068`](../068-equity-board-role/spec.md) an account
  assigned `equity:board` carries it in its tokens and Apps sign-ins, in directory mode only.
- **Identity mode.** In the default attestation mode a token or ticket names no person: no self-service, and
  drafting, activation and the exercise steps are refused. `modules.equity.settings.identity: directory` with the
  `people` module is needed. The pages say so.

## Not covered

Creating entities, share classes and plans; tranche, milestone and acceleration schedules; editing a draft;
amend, cancel, terminate, milestones; manual ledger entries; valuations, convertibles and round modelling;
legal hold, erasure and retention. All stay API-only. No signed grant documents are stored: the statement is a
summary of the records. English only.

## Budgets

`html/template` and the pages add to the binary: `can-equity` goes from 12.95 MiB to the figure recorded in
`docs/measurements/footprint.md`, and its binary budget in `dev/footprint/budgets.json` is raised to match.
Pages are parsed on first use. The memory budgets are unchanged.
