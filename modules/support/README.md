# support

> **Status: experimental.** Installed through can-core on k3d in CI ([specs/041](../../specs/041-wrapped-modules-live/spec.md));
> mail configured from `company.yaml` and tested there against an in-memory mail server
> ([specs/059](../../specs/059-support-mailbox/spec.md)). Never on a real server, domain, mail relay or mail provider. The
> `tickets` capability is marked `planned` (no adapter yet).

> **Backups:** can-core takes a `pg_dump` of the database and an archive of the volume (attachments) in every dump run, and no unpublished image is needed
> ([docs/backup-restore.md](../../docs/backup-restore.md#wrapped-modules)). Verified on k3d only.

Shared support inbox. Wraps [FreeScout](https://github.com/freescout-help-desk/freescout) (PHP),
provides the `tickets` capability and publishes `ticket.created.v1`.

| | |
|---|---|
| Licence | AGPL-3.0 (`AGPL-3.0-only` in SPDX). Network copyleft: see [modules/README.md](../README.md#licences-and-network-copyleft). We run it unmodified. |
| Image | `pikapods/docker-freescout:1.8.252-r2`, pinned by digest. FreeScout publishes no image: this is a **third party** build, non-root, **amd64 only** |
| Footprint | 192 MB request, 672 MB limit, 2 GiB disk. Measured 63 to 86 MiB idle on k3d over two runs, 90 to 115 MB locally and 140 MB while migrating, plus the shared Postgres (35 to 50 MB) |
| Database | Postgres (can-core's shared server in `can-data`, own database and role) |
| Public host | `support.<domain>` (the whole app, including its sign-in page) |
| Sign in | `admin@<domain>` (setting `admin_email`, used on the first start only); `can support credentials` |
| Email | the mailbox, outgoing (opt-in relay) and incoming (IMAP) from `company.yaml`: [Set up mail](#set-up-mail) |
| Capability | `tickets` (`schemas/capabilities/tickets.v1.json`), served by `support-adapter` (:8081, off until the adapter image exists) |
| Backup | can-core: a `pg_dump` of the database and an archive of the volume (attachments) ([specs/046](../../specs/046-wrapped-backups/spec.md)) |
| Data classes | `support-conversation`, `support-attachment`: 3 years, standard sensitivity |

## Set up mail

You need a mailbox you can fetch over IMAP with TLS (the support address itself, or one it forwards to) and an SMTP
relay that lets you send **as the support address**, with SPF, DKIM and DMARC set up for your domain
([docs/email.md](../../docs/email.md)). `can` creates none of these.

1. `company.yaml` (no passwords here, and none in git):

   ```yaml
   email:
     relay: { host: smtp.example.com, port: 465 }   # replies leave through it; 465 is implicit TLS
     modules:
       support: { enabled: true }               # hands the relay password to FreeScout
   modules:
     support:
       settings:
         support_address: support@acme.example  # not the administrator's sign-in address
         mailbox_name: Acme Support             # optional
         inbound:
           imap:
             host: imap.example.com
             username: support@acme.example     # or IMAP_USERNAME in the file below
             # encryption: ssl (port 993, default) or starttls (143); folder: INBOX
   site:
     support: {}                                # optional: a "Support" section with a mailto link
   ```

2. The passwords, in files outside the repository (mode 0600):

   ```sh
   printf 'SMTP_USERNAME=...\nSMTP_PASSWORD=...\n' > ~/smtp.env
   printf 'IMAP_PASSWORD=...\n' > ~/imap.env
   make up SMTP_CREDENTIALS=~/smtp.env SUPPORT_IMAP_CREDENTIALS=~/imap.env
   ```

   Without `make up`, create the Secrets by hand: `can-system/can-smtp-relay` ([docs/email.md](../../docs/email.md))
   and `can-system/can-support-imap` with keys `host`, `username`, `password` and optionally `port`, `encryption`,
   `tls_verify`. The password is used only when the Secret's `host` is the one in `company.yaml`, so that editing
   `company.yaml` cannot send it elsewhere.

3. can-core installs (or upgrades) the module and applies the settings at every start of its pod. Check:

   ```sh
   can support status        # config, fetch, send; exit 1 when something fails
   can support credentials   # URL, user and generated password of the first administrator
   ```

Both commands ask the server over ssh; `--context NAME` uses your own `kubectl` instead. Change the administrator's
password after the first sign-in: the generated one stays in the cluster and is not updated.

What is applied:

- A mailbox for `support_address` (an existing one with that address is taken over; changing the address renames the
  mailbox, keeping its conversations). Settings written this way are **overwritten at every pod start**; change them
  in `company.yaml`. Other mailboxes, users, signatures and auto replies are yours.
- Outgoing: the relay, with `From: support_address`. System mail (invitations, password resets) uses the relay's `from`.
- Incoming: IMAP, fetched by FreeScout every minute. Fetched mail is marked as read and left on the server.
- FreeScout's own alert when fetching stops is switched on once; you may turn it off.
- Removing `support_address`, `inbound` or the relay opt-in removes the connection settings that were written. The
  mailbox and its conversations stay. FreeScout and its image have had the passwords: rotate them.

New passwords: update the Secret (or run `make up` again with the file); can-core sees it in a minute or two and
restarts the module.

### The relay's certificate

FreeScout is set to verify the relay's certificate and name (this module wraps its `config/mail.php`: as shipped, it
verifies nothing). A relay with a self-signed or private-CA certificate is refused. To send to it anyway, add the key
to the relay Secret, which also has to name the `host`; nothing in `company.yaml` can turn the check off, and
can-core's own `email.send` ignores the key and always verifies:

```sh
kubectl -n can-system patch secret can-smtp-relay --type merge -p '{"stringData":{"tls_verify":"false"}}'
```

**Use port 465 (implicit TLS) if your relay offers it.** With STARTTLS (587), FreeScout's mail library falls back to
plain `HELO` when the server answers `EHLO` with an error, and then sends **without TLS**. Someone on the network path
could force this and read the message (the relay password is not sent in that case). With 465 there is no such
fallback.

### Upgrading from module 0.1.0

Releases before 0.2.0 accepted these settings and ignored them. Read this before upgrading an installation that has
them in `company.yaml`:

- **A mailbox with the `support_address` that you set up by hand is taken over**: what `company.yaml` configures
  (outgoing with the relay opted in, incoming with `inbound.imap`) replaces its connection settings, and the pod log
  says `can: WARNING: the existing mailbox ... was taken over`. Note your settings first, or remove `support_address`.
  A different address creates a second mailbox.
- Values that cannot be applied (for example `encryption: tls` or `none`, or a support address equal to the
  administrator's) are warnings of `can validate`, not errors; while there is one, no mailbox setting is changed and
  `can support status` says which.
- **The relay's certificate is now checked**, for every mailbox, including ones set up by hand, with the relay
  opted in or not. One that relied on a self-signed certificate stops sending; `tls_verify` above reaches the module
  only with the relay opted in.

### Health

| Where | What |
|---|---|
| `can support status` | Signs in to the IMAP server and the relay now (nothing is read or sent), checks that each fetched folder exists, looks at the last scheduled fetch and at replies that failed or wait |
| `can verify --server` | The same as `support.mail-config`, `support.mail-fetch`, `support.mail-send` |
| Pod log | `can: mail status {...}` when the state changes (checked every 5 minutes) |
| FreeScout | Manage, Logs and System; its fetch alert by email |

The admin UI's status page does not show mail health.

### Not covered

- STARTTLS can be downgraded to no encryption by an attacker on the path (above); use 465.
- OAuth sign-in (Google Workspace and Microsoft 365 mailboxes that refuse passwords) cannot be set from
  `company.yaml`; POP3 and the inbound-parse webhook (`inbound.mode: webhook`) neither.
- No real mail provider has been tried.

FreeScout's knowledge base is a paid add-on; the public help centre comes from the wiki module.
SSO is `none`: FreeScout has no OIDC in its free core.

## Updates and security patches

The image is pinned by digest and nothing updates it: no bot watches chart values, and a patched
FreeScout, PHP or nginx reaches an installation only after a maintainer bumps the tag, digest **and the
module's `version`** (can-core upgrades a module only when its version or settings change), releases
can-core (the charts are baked into its image) and the operator upgrades. A third party builds this
image, which adds its own delay. Watch FreeScout's security releases yourself if this matters.

## Without a support address

With the relay opted in and no `support_address`, only FreeScout's system mail is configured, as before; mailboxes
are then yours to set up in FreeScout. The relay mail does not pass `email.send`: no allow-list, quota or delivery log
([docs/email-policy.md](../../docs/email-policy.md)).

## Open items

- Inbound-parse webhook (`inbound.mode: webhook`) needs the adapter, which does not exist.
- Upgrades to a newer FreeScout are untested; the image is not updated automatically.
- `retention_days` is accepted and not applied.
