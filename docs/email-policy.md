# Email policy

Implements the email policy of SPEC.md section 5 for the starter modules (`support`, `newsletter`).
We do not self-host a mail server: deliverability is a trap that costs more than it saves.
Everything below is design; the pieces that need can-core (E03) and the adapters are marked. **Built (E03d):**
the REST path, `email.send`, with its relay settings, allow-lists and delivery status ([email.md](email.md)).
**Built (specs/041):** the
`can-smtp` Secret for `support`, applied to FreeScout's system mail settings.
**Not built:** per-mailbox settings, Listmonk's SMTP settings, `make email`, DNS records and inbound mail.

## Outbound: relay credentials injection

A transactional relay (SES, Resend or Brevo) carries all outbound mail over port **587** or 443,
because many hosts block port 25. A module that declares `email.send` in `requires.capabilities`
gets the relay credentials injected; the mechanism depends on who sends:

| Sender | How it sends | Credentials |
|---|---|---|
| In-house Go modules (forms) | REST call to can-core's `email.send` | none; the projected audience-`can` token |
| SMTP-speaking OSS apps (FreeScout, Listmonk) | SMTP straight to the relay | injected from a Secret |

For the second kind, can-core creates a Secret named **`can-smtp`** in the module namespace of the
apps it lists as SMTP senders (today `support` only), from the same relay settings as `email.send`
(`company.yaml` `email.relay` and the Secret `can-system/can-smtp-relay`). Keys: `host`, `port`,
`user`, `password`, `from`, `encryption` (`tls` for STARTTLS, `ssl` for implicit TLS).

- Such mail does **not** pass `email.send`: no recipient allow-list, quota or delivery log. Setting
  `email.modules.support.enabled: false` withholds the relay. A relay with `tls: none` is never handed out.
- The Secret is empty without a usable relay, and the module starts without one. A relay added or
  changed later arrives at the next install or upgrade of the module.
- Passwords never enter `module.yaml`, values files, ConfigMaps, logs or settings schemas.
- The base namespace policy lets module pods reach the internet but not the node or cluster
  ranges (`deploy/templates/module-namespace.yaml`), which is enough for SMTP on 587 to a hosted
  relay and for IMAP on 993 to a hosted mailbox. No extra NetworkPolicy is needed.
- **Support** keeps mail settings in its database. At start, the relay is written into its system
  settings (invitations, password resets, notifications). Replies to customers use each mailbox's own
  connection settings, which the operator enters.
- **Newsletter** keeps its SMTP servers in its database too and is given nothing: the operator enters
  them in Settings, SMTP. A **separate bulk relay** is recommended once lists grow: marketing
  complaints then cannot damage the reputation of support and invoice mail.
- Authentication DNS: SPF, DKIM and DMARC aligned to the company domain are mandatory before real
  sending; `can email` prints (or, with a DNS plugin, sets) the records. Marketing mail is sent
  from the same domain or a subdomain (`news.<domain>`) with its own DKIM selector.

## Inbound

Forwarding services (Cloudflare Email Routing, ImprovMX) only forward to an existing address, so
they are not enough alone. The support module supports two mechanisms, chosen by the setting
`inbound.mode`.

### `imap`: a hosted mailbox the support address forwards to

`support@<domain>` forwards to a small hosted mailbox; FreeScout fetches it over IMAP (993) on its
own one-minute fetch cycle and creates conversations. Settings: host, port, username, encryption,
folder (`modules/support/settings.schema.json`). The password lives in the module Secret, not in
the settings. Nothing is exposed publicly. Simple and robust, at the price of one mailbox
subscription and up to a minute of latency.

### `webhook`: an inbound-parse service (the adapter's parse path)

Postmark, Mailgun or SES (via SNS to HTTPS) receive mail for the domain and POST it to a public
webhook on the **support adapter**, which creates the ticket through the FreeScout API and
publishes `ticket.created.v1`.

```
sender -> MX (provider) --POST /inbound--> Traefik --> support-adapter :8081
                                                         verify signature, de-duplicate by
                                                         Message-ID, create ticket (FreeScout API)
```

- Public route: `Host(support.<domain>) && PathPrefix(/inbound)` with a higher priority than the
  catch-all, rendered by the chart only when the adapter is enabled (`ingress.adapterRoutes`).
  can-core's module API is never exposed publicly (SPEC 3.1).
- Authenticity: the adapter rejects any request whose provider signature (Postmark basic-auth
  token, Mailgun HMAC with timestamp, SES SNS message signature) or shared secret (the module
  Secret's `webhook-secret`) does not verify, in constant time, before parsing the body.
- Idempotency: de-duplicate on `Message-ID` (providers retry). Size cap on the body; attachments
  stream to the helpdesk and are not kept by the adapter.
- Trust: the parsed sender is untrusted display data; nothing grants access by From address.
- The adapter is not built yet (tasks T030, deferred); this section is its contract.

## What is verified offline

The chart renders the credential `secretKeyRef`s as `optional`; the chart adds no NetworkPolicy
beyond the same-namespace allow (so SMTP and IMAP egress rest on the base namespace policy); and
the `/inbound` route exists only with the adapter (`test/contract/starter_modules_test.go`). Actual delivery, signature verification and DNS alignment are not
verifiable without a relay and a domain and are listed as gaps in
`specs/007-starter-modules/research.md`.
