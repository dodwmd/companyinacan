# Outbound email (`email.send`)

can-core sends the platform's mail through an SMTP relay you choose (Amazon SES, Resend, Brevo, your
own submission server). We do not run a mail server ([email-policy.md](email-policy.md)). Built in
epic E03d, tested offline and against the compose stack's SMTP sink; **no real relay has been used**.
Spec: [specs/039-email-send](../specs/039-email-send/spec.md).

## What sends mail

| Sender | How | Recipients |
|---|---|---|
| Forms gateway: new lead | `POST /v1/email` on the module API | the form's `notify_email` only |
| Notifications and approvals | can-core's own e-mail channel ([notifications.md](notifications.md)) | the address a person set, if it is vouched for (below) |
| Finance: contract signing invitations and certificates | `POST /v1/email` | the signers, within its allow-list or `external_recipients` |
| Other modules | `POST /v1/email`, if they declare `email.send` | their allow-list |

SMTP-speaking third-party apps (FreeScout, Listmonk) cannot call this API and are **not** served by
it. `support` can be handed the relay itself, only with `email.modules.support.enabled: true` (off by
default); the rules below then do not apply to its mail, and **FreeScout does not verify the relay's TLS
certificate**. It uses the relay for its system mail and, with a `support_address`, for replies
([modules/support/README.md](../modules/support/README.md#set-up-mail)); turning it off removes what was written
into FreeScout, which has had the password all the same. See [email-policy.md](email-policy.md).

## Set up the relay

1. In `company.yaml`:

   ```yaml
   email:                     # the relay; who is emailed about a new lead is notify_email below
     relay:
       host: email-smtp.eu-west-1.amazonaws.com
       port: 587              # default 587; 465 selects implicit TLS
       tls: starttls          # starttls (default) or implicit
       from: no-reply@acme.example   # default no-reply@<domain>
   modules:
     forms:
       settings:
         notify_email: [sales@acme.example]
   ```

2. Give can-core the credentials. They live only in the Secret `can-system/can-smtp-relay`, never in
   `company.yaml`:

   ```sh
   printf 'SMTP_USERNAME=...\nSMTP_PASSWORD=...\n' > smtp.env   # outside the repository, mode 0600
   make up SMTP_CREDENTIALS=smtp.env                            # or: can up --smtp-credentials-file smtp.env
   ```

   The admin setup wizard writes the same Secret. By hand:

   ```sh
   kubectl -n can-system create secret generic can-smtp-relay \
     --from-literal=host=smtp.example.com --from-file=username=./user --from-file=password=./pass
   ```

   Keys: `host`, `username`, `password`, and optionally `port`, `tls`, `from`. A key in the Secret
   overrides `company.yaml`. Credentials are used only when the Secret also names the `host`, and a
   different `email.relay.host` in `company.yaml` disables sending: editing `company.yaml` cannot
   redirect your password to another server.

3. Publish SPF, DKIM and DMARC records for the sender domain as your relay describes. `can` does not
   do this yet.

Both the Secret and `company.yaml` are re-read on every send; no restart is needed (a mounted Secret
or ConfigMap takes up to a minute to refresh).

### Transport rules

- TLS 1.2 or later with certificate verification is required. With `starttls`, a relay that does not
  offer STARTTLS gets nothing: there is no plaintext fallback. There is no option to skip
  verification.
- The relay address is checked after name resolution: private, loopback, link-local and other
  non-public addresses are refused. `CAN_SMTP_ALLOW_PRIVATE=true` (can-core environment) permits a
  relay on a private network; link-local (cloud metadata) stays refused. In a cluster you also need
  a NetworkPolicy: the shipped one allows public addresses on ports 25, 465, 587 and 2525 only.
- `tls: none` is refused unless `CAN_SMTP_ALLOW_INSECURE=true` (development), and credentials are
  never sent without TLS.
- **A relay on the server itself does not work.** The host firewall blocks pods from the node's own
  address ([node-isolation.md](node-isolation.md)), and the NetworkPolicy blocks private addresses.
  Use a hosted relay.
- Link-local and cloud metadata addresses (including `fd00:ec2::254`) are refused even with
  `CAN_SMTP_ALLOW_PRIVATE=true`.
- Authentication is `AUTH PLAIN`. Relays that only offer `LOGIN` or XOAUTH2 are not supported.

## Who may be written to, and how much

Every recipient of a module's message must be on its allow-list. By default that is every address at
the company domain plus the addresses in the module's `notify_email` setting. An explicit list
replaces the default:

```yaml
email:
  modules:
    forms:
      recipients: [sales@acme.example, "@partner.example"]  # domains match exactly, no subdomains
      per_hour: 60        # defaults: 60 per hour, 500 per day, counted per recipient
      per_day: 500
    crm:
      enabled: false      # this module may send nothing
```

A form added by editing the module's `config` (not its settings) with its own `notify_email` outside
the company domain also needs that address under `email.modules.forms.recipients`.

**Contract signers.** `finance` mails the people asked to sign a contract
([finance.md](finance.md#signing-by-emailed-link)), who are usually outside the company. List their
domains under `email.modules.finance.recipients`, or set

```yaml
email:
  modules:
    finance: { external_recipients: true }   # finance only; default false
```

With it, the finance module may mail **any** address (still within its quota). The default is the
allow-list. A queued invitation contains the signing link until it has been sent.

At most 500 messages per sender may be waiting at once. A refused message is not stored
(HTTP 403, 429 or 503) and the caller records the failure.

The sender address, `To`, `Date`, `Message-ID` and every other header are can-core's. A module
supplies recipients, an optional `Reply-To`, a one-line subject (control characters are refused) and
a plain-text body of up to 64 KiB. There is no HTML and there are no attachments. Messages carry
`Auto-Submitted: auto-generated`.

## Notification mail

A notification caused by a module (its `POST /v1/notifications`, its approval requests, its events)
**counts against that module**: its `enabled: false` stops it and its quota covers it, together with
the module's own sends. can-core's own notices are limited to 300 per hour and 2000 per day. There is
also the limit per person.

When the module wrote any of the content, the email carries a **fixed text**: the module id, the
notification type and a link to `https://admin.<domain>/notifications`. The module's title and body
stay in the inbox. To mail the module's wording, opt in per module:

```yaml
email:
  modules:
    crm: { notification_text: true }
```

Addresses people type in their notification settings are not verified, so notification mail only goes
to the **company domain** (or to `email.notification_recipients`, which replaces it) and to the
person's **single sign-on address** when sign-on requires verified email claims
(`sso.require_email_verified`, the default) and is switched on, and the provider said the address was
verified at the sign-in that stored it (after an upgrade from before this was recorded, people sign in
once again). Anything else is logged as a failed delivery. Mailed text shows notification types with
spaces around their dots, so a mail reader does not make a link of one. Mail still queued from before
can-core recorded which module caused it is attributed by the type's module prefix on upgrade.

```yaml
email:
  notification_recipients: ["@acme.example", "@acme-partners.example"]
```

## Delivery, retries, status

Mail is stored in can-core's database before the caller is answered, with an audit entry in the same
transaction. One worker delivers it. A temporary failure (network, TLS, authentication, SMTP 4xx) is
retried after 1, 2, 4, 8, 16, 32 and 60 minutes; after 8 attempts, a permanent refusal (SMTP 5xx for
the sender, recipient or content), or 72 hours of waiting, the message is a **dead letter**.
When the failure is about the relay itself (connection, TLS, login) can-core stops the pass and
pauses all delivery for 1 to 15 minutes. A relay that goes silent while taking one message delays only
that message; three different messages in a row without an answer count as a relay failure. Senders take turns, so one sender's backlog does not hold
up the others.
Delivery is at least once: a crash at the wrong moment can send a message twice (same `Message-ID`).

**Admin UI → Email** (`/email`, permission `notifications.admin`) shows the relay, and the newest 100
messages with sender module, masked recipient, status, attempts and the last error (a fixed text
and the SMTP reply code; never server text). Someone with `platform.operate` can send a dead letter
again for seven days. Modules read the status of their own messages with `GET /v1/email/{id}`.

**Lead notifications.** The forms gateway records the notification with the lead. While can-core
does not accept it (down, over quota, no relay, recipient not allowed yet) it is retried after 1, 2,
4 ... 60 minutes, or later if can-core says so (`Retry-After`), for seven days. The admin UI's leads
view marks such a lead "Email notification retrying" or "failed", and the leads API returns
`notification_status`. After a long outage, or when a relay is first connected, up to a week of
notifications arrive.

Metric: `can_email_messages_total{result="queued|sent|retry|dead|refused"}`.

## Data kept

Recipient, reply-to, subject and body are blanked as soon as a message is sent, and seven days after
it is given up on. The row (id, module, masked recipient, status) is deleted 30 days after it
finished. Addresses and content never appear in the audit log, application logs, events or error
texts. While a message waits, its content is in can-core's state database, and so in a backup of it;
erasing a lead in the forms module does not reach a copy that is still waiting.

## Trying it locally

The compose stack runs a **development-only** SMTP sink that keeps mail instead of delivering it:

```sh
make compose-up
# submit the form linked from http://localhost:8080, then:
curl --resolve mail.localhost:8080:127.0.0.1 http://mail.localhost:8080/messages
```
