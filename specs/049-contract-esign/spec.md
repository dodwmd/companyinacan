# Feature Specification: Contract signing by emailed link (Epic E15d)

**Branch**: `epic/E15d-esign` | **Date**: 2026-11-26 | **Status**: built offline, experimental: not run on a
cluster, no real mail relay, not browser verified, not reviewed by a lawyer

SPEC.md references: the `documents` capability and the "E-sign / documents" row of section 5
(Documenso, with DocuSeal and OpenSign as alternatives), the contract workflow of section 6,
`contract.signed.v1` (4.3), signed documents and sensitive data (7).

## Decision: an in-house simple electronic signature, for now

SPEC.md plans a wrapped e-signature application (Documenso first) behind the `documents`
capability. This epic does **not** do that. It adds a small signing flow inside the in-house
`finance` module, next to the contracts it already holds.

Why:

- Documenso could not be installed, run or verified in the environment this was built in, and an
  unverified wrapper would have been a second fake.
- It is about 400 MB and needs Postgres, more than the rest of the finance path together, and some
  of its features are under a commercial licence (SPEC.md section 5 asks to verify what the open
  core covers; that is still not done).
- The need today is "send this text, get a recorded yes", for a small company.

This is a deviation from SPEC.md, not a replacement of its plan. The `ESigner` adapter seam of
specs/014 is unchanged, and a wrapped signer can still be added behind it. The `documents`
capability is **not** provided by this work. Revisit when any of these is needed: PDF documents,
drawn or certificate-based signatures, identity checks, advanced or qualified signatures, trusted
timestamps, or templates with signature fields.

## Legal limits (read before relying on it)

This is a **simple electronic signature**. It is not an advanced or qualified electronic signature
(in the sense of EU eIDAS or similar laws), and nothing here is legal advice.

- A signer is identified **only by access to the mailbox** whose address the sender typed. No
  identity document, phone number or password is checked. A forwarded mail, a shared mailbox, or a
  compromised account signs as that person.
- The **sender chooses the addresses**. A dishonest member of staff with `finance.write` can send the
  "other side's" link to a mailbox they control and sign as them. The certificate records the
  address used; checking that it is really the counterparty's is the reader's job.
- No cryptographic signature is made with a key held by the signer, no certificate authority is
  involved and no trusted timestamping service is used. Times are the server's clock.
- The signed document is **plain text**. No PDF is produced or sealed.
- The evidence is tamper-**evident** within limits (below), not tamper-proof.
- Some documents may not be signed this way at all (for example wills, many property and family
  law documents, some employment and consumer contracts, anything needing a witness or notary),
  depending on the jurisdiction. The operator decides what to send.

## User scenarios

1. Staff open a draft contract, list who signs for the other side and who countersigns, optionally
   ask for signing in order, and send. Each person invited gets a mail with a link.
2. A signer opens the link without an account, reads the exact text with its SHA-256, may download
   it, then types their name, ticks the consent box and signs; or declines with an optional reason.
3. With signing in order, the next person is invited when the previous one has signed.
4. When everyone has signed, the contract is signed (or active), `contract.signed.v1` is published,
   each signer is mailed a certificate, and staff can read a fuller certificate.
5. A link was lost or expired: staff send a new one; the old one stops working. The request was
   wrong: staff withdraw it and the contract is a draft again.

## Functional requirements

- **FR-001** Only a draft contract can be sent. Sending freezes the current version: its SHA-256 and
  title are stored with the request. Text and titles with control, bidirectional, zero-width,
  other invisible format or tag characters are refused, and so are such characters or line
  separators in signer names and typed names. (This also refuses a byte-order mark and the zero-width
  joiners some scripts and emoji use: retype or remove them.)
- **FR-002** Links are 256 random bits, stored only as SHA-256, expire (default 7 days, 1 to 30) and
  work once. No staff page, API response, log, audit entry or event ever contains a link.
- **FR-003** Mail goes through can-core's `email.send`, one recipient per message, only to the
  addresses of that request's signers.
- **FR-004** Opening a link changes nothing (mail scanners follow links). Signing needs a POST with
  the typed name, the consent box and the hash of the document shown.
- **FR-005** Every link that does not work (unknown, malformed, used, expired, replaced, withdrawn,
  declined, text changed, signing switched off) gets the same 404 page. Bad links are throttled per
  client address; a working link never is.
- **FR-006** A decline ends the whole request, kills every link and returns the contract to draft. A
  new version of the text does the same, in the same transaction.
- **FR-007** While a request is open, nobody can record the signature by hand or by webhook.
- **FR-008** After signing, the contract's text, title, party, signed hash and signing records cannot
  change; the API refuses and database triggers refuse. Termination and dates behave as in specs/014.
- **FR-009** Every state change writes its audit entry in the same transaction, with ids and hashes
  only. Events carry no personal data.
- **FR-010** Each request has a hash chain over `sent`, `invited`, `signed`, `declined`, `revoked`
  and `completed`, with digests (not copies) of the personal data. `GET /v1/contracts/{id}/verify`
  checks the chain, the records against it, and the text against the hash, in one read transaction.
  The "erased" mark is accepted only on a declined or revoked request whose personal fields are all
  empty; it never switches a check off elsewhere.
- **FR-011** IP policy: `signing_record_ip` is `truncated` (default, /24 or /48), `full` or `none`.
  The user agent is not recorded.
- **FR-012** The public path is off unless `modules.finance.expose.signing: true`. can-core then
  routes only `/public/sign/` (GET, HEAD, POST) on `finance.<domain>`, TLS only, with security
  headers and the anonymous edge limit, and gives the module `sign_base_url`. Without that
  setting finance refuses to send and answers 404 to every link.
- **FR-013** Reading a signing request or a certificate is audited.
- **FR-014** The outcome of mailing each signer's certificate is stored, audited and shown to staff,
  who can send it again (at most 10 times, never after the party is erased). The last signer's own
  certificate is handed to the mail service before the page answers, so the page does not announce
  one that was refused. There is no automatic retry beyond can-core's own delivery retries.

## Interfaces

- API: `GET` and `POST /v1/contracts/{id}/signing`, `POST .../signing/revoke`,
  `POST .../signing/signers/{signer}/resend`, `GET .../signing/certificate`
  (`specs/014-finance-basics/contracts/openapi.yaml`).
- Public: `GET`/`POST /public/sign/{token}`, `GET /public/sign/{token}/document`,
  `GET /public/sign/assets/sign.css`. Server-rendered, no script, no session, no cookie.
- Web UI: the contract page (`specs/031-finance-ui/contracts/ui-routes.md`).
- `company.yaml`: `modules.finance.expose.signing`, `email.modules.finance.external_recipients`.
- Module configuration: `sign_base_url`, `signing_record_ip`.
- Storage: migration `006_signing.sql`.

## Mail to people outside the company

`email.send` refuses addresses outside a module's allow-list (specs/039), and a counterparty is
usually outside it. The operator can list their domains under `email.modules.finance.recipients`, or
set `email.modules.finance.external_recipients: true`, which lets **finance only** mail any address
within its quota. This widens what a compromised finance module could do (see T11), so it is off by
default. Without either, the invitation is shown to staff as refused and nobody outside is mailed.

## Threat model

| # | Threat | Mitigation | Residual risk |
|---|---|---|---|
| T1 | Guessing a link | 256 random bits; looked up by hash; failures throttled per address and limited at the edge | None practical |
| T2 | Link leaks through logs or `Referer` | The access log and metrics record the route pattern, never the path; `Referrer-Policy: no-referrer`; no outside resources; `no-store`; `noindex` | A proxy or CDN in front, or a Traefik access log switched on by the operator, records paths |
| T3 | Link leaks from a database or backup | finance stores only the hash | can-core's mail outbox holds the message, with the link, until it is sent (up to 72 h waiting, 7 days if dead), so its state database and backups can contain live links. The relay and the mailbox see it too |
| T4 | Someone other than the signer uses the link | Single use, expiry, withdrawal, new link kills the old one | Whoever can read the mail can sign: this is the limit of the method |
| T5 | Staff sign for the other side | Staff never see links; certificate and audit name the sender and the address used | The sender chooses the address (see legal limits). No second person is required to send |
| T6 | Mail scanner or prefetch signs or consumes the link | Only a POST with consent and a typed name changes anything | A scanner that submits forms |
| T7 | Cross-site request forgery | The token in the path is itself needed; `Sec-Fetch-Site` is checked; no cookies exist | None practical |
| T8 | The signer is shown something other than what is recorded | The page renders the stored version after re-checking its hash; the download is the exact bytes; escaped output; strict CSP, no script; control, bidirectional, zero-width and tag characters refused | Look-alike characters, unusual spaces and other characters not on the list are not detected |
| T9 | Records altered by someone with the database | Triggers; hash chain; `verify`; the chain head is in the certificate mailed to every signer and the signed hash in the `contract.signed.v1` event held by can-core | Someone who can write the database can drop the triggers and rebuild a consistent chain. Only copies **outside** the database (signers' mail, can-core's events, backups) then show it, and comparing them is manual. The chain is not signed with a key |
| T10 | Replay or double submission | The signature, status change and token removal are one transaction on one writer; the second request gets the 404 page | A double click may show the 404 page after a successful signature; the page says so |
| T11 | A compromised finance module sends mail | One recipient per message, can-core's headers, plain text, per-module quota, off switch | With `external_recipients` it can send any text to any address, 60 an hour and 500 a day by default |
| T12 | Mail bombing through resend | At most 10 links per signer per request; module quota; staff only | Staff can open new requests |
| T13 | Flooding the public path | Edge rate and in-flight limits; 16 KiB form cap; failure throttle | Distributed floods; a working link may be requested repeatedly |
| T14 | Link outlives the reason for it | Withdraw, new version, decline, party erasure and switching the setting off all kill it | A link already opened shows the text the browser holds |
| T15 | Personal data kept too long | Class `finance-contract`: removed with the contract by retention; erasure of a party removes signer data of requests that did not complete | Completed requests are retained as evidence; requests of a never-purged draft or open-ended contract stay; mailed copies cannot be recalled |

## Known gaps

- Nothing here has run on a cluster, through a real relay or in a real browser; Traefik's handling of
  the route is unit tested only.
- Signers get the certificate by mail, **not a copy of the document**: the page tells them to save or
  download it before answering, and shows it once more after signing.
- Staff are not notified of a signature or a refusal; they look at the contract (or consume
  `contract.signed.v1`).
- A certificate can be accepted by the mail service and still never arrive; that shows only in
  can-core's delivery list.
- An invitation interrupted by a crash stays "not confirmed" until someone sends a new link. Expired
  requests are not closed automatically.
- Installed under can-core, `company.yaml` settings do not yet reach finance's configuration, so the
  company name on the page and in mail is the web address and `signing_record_ip` stays `truncated`.
- The signing page and mail are English only.
- Restoring an older backup brings back older signing state: links used since would work again until
  they expire.
- No external legal or security review.
