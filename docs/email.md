# Outbound email (`email.send`)

can-core sends the platform's mail through an SMTP relay you choose (Amazon SES, Resend, Brevo, your
own submission server). We do not run a mail server ([email-policy.md](email-policy.md)). Built in
epic E03d, tested offline and against the compose stack's SMTP sink; **no real relay has been used**.
Spec: [specs/039-email-send](../specs/039-email-send/spec.md).

## What sends mail

| Sender | How | Recipients |
|---|---|---|
| Forms gateway: new lead | `POST /v1/email` on the module API | the form's `notify_email` only |
| Notifications and approvals | can-core's own e-mail channel ([notifications.md](notifications.md)) | the address each person set for themselves |
| Other modules | `POST /v1/email`, if they declare `email.send` | their allow-list |

SMTP-speaking third-party apps (FreeScout, Listmonk) cannot call this API and are **not** served by
it; see [email-policy.md](email-policy.md).

## Set up the relay

1. In `company.yaml`:

   ```yaml
   email:
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

can-core's own notification mail is limited to 300 per hour and 2000 per day, on top of the limit per
person. At most 500 messages per sender may be waiting at once. A refused message is not stored
(HTTP 403, 429 or 503) and the caller records the failure.

The sender address, `To`, `Date`, `Message-ID` and every other header are can-core's. A module
supplies recipients, an optional `Reply-To`, a one-line subject (control characters are refused) and
a plain-text body of up to 64 KiB. There is no HTML and there are no attachments. Messages carry
`Auto-Submitted: auto-generated`.

## Delivery, retries, status

Mail is stored in can-core's database before the caller is answered, with an audit entry in the same
transaction. One worker delivers it. A temporary failure (network, TLS, authentication, SMTP 4xx) is
retried after 1, 2, 4, 8, 16, 32 and 60 minutes; after 8 attempts, a permanent refusal (SMTP 5xx for
the sender, recipient or content), or 72 hours of waiting, the message is a **dead letter**.
Delivery is at least once: a crash at the wrong moment can send a message twice (same `Message-ID`).

**Admin UI → Email** (`/email`, permission `notifications.admin`) shows the relay, and the newest 100
messages with sender module, masked recipient, status, attempts and the last error (a fixed text
and the SMTP reply code; never server text). Someone with `platform.operate` can send a dead letter
again for seven days. Modules read the status of their own messages with `GET /v1/email/{id}`.

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
# submit the form at http://localhost:8080/contact/, then:
curl --resolve mail.localhost:8080:127.0.0.1 http://mail.localhost:8080/messages
```
