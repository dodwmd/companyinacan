# Feature Specification: The support inbox's mail from company.yaml (E07c)

**Feature Branch**: `epic/E07c-support-inbox`
**Created**: 2026-11-26
**Status**: Built. Verified on k3d against an in-memory mail server (`support-mail` CI job) and, for the script,
with the pinned FreeScout image locally. No real mail provider, relay, domain or server.
**Input**: SPEC.md sections 1, 3.4 and 5 (`make up` gives a site, forms and a support inbox);
[specs/041](../041-wrapped-modules-live/spec.md) (installs, mailbox and mail by hand);
[docs/email-policy.md](../../docs/email-policy.md).

## Goal

After `company.yaml` names a support address, an IMAP mailbox and the relay, and the two passwords are given from
files, a customer's mail is a ticket and an agent's reply is delivered, with nothing done in FreeScout's settings.
The operator can get the administrator's password and see whether mail works.

## User Scenarios

- **US1 (P1)** Enable `support` with `support_address` and `inbound.imap`, opt the relay in, give the passwords. The
  mailbox exists with outgoing and incoming settings; a mail becomes a conversation within the minute; a reply
  arrives from the support address in the thread.
- **US2 (P1)** Run `can support credentials` and sign in.
- **US3 (P1)** A password expires. `can support status` and `can verify --server` say which direction fails. A new
  Secret is applied without any other change.
- **US4 (P2)** Withdraw inbound mail or the relay opt-in: what was written is removed; conversations stay.
- **US5 (P2)** `site.support` puts a "Support" section with a mailto link on the public site.

## Requirements

- **FR-001** Configuration MUST be `modules.support.settings` (`support_address`, `mailbox_name`,
  `inbound.imap.{host,port,username,encryption,folder}`) and the existing `email.relay` with
  `email.modules.support.enabled: true`. No password may be in `company.yaml`, a ConfigMap, values or a pod template.
- **FR-002** The IMAP password MUST come from the Secret `can-system/can-support-imap` (written by
  `can up --support-imap-credentials-file` or by hand), be used only when the Secret's `host` equals company.yaml's,
  reach the module as the Secret `can-imap` (empty otherwise), and be left alone when the mounted Secret cannot be read.
- **FR-003** IMAP MUST use TLS (`ssl` or `starttls`). Certificate verification MUST be on unless the Secret (not
  company.yaml) says `tls_verify: "false"`.
- **FR-004** `can validate` MUST warn about settings that cannot work (a support address equal to the administrator's,
  no TLS, no host) and about incomplete ones (no inbound, no relay, webhook mode). It MUST NOT refuse them: these keys
  were accepted and ignored before. While a value cannot be applied, no mailbox setting MUST change.
- **FR-005** The module MUST apply the settings at every pod start, using FreeScout's own models; a change of a
  setting or of either password MUST restart it. It MUST be repeatable without creating a second mailbox, take over
  an existing mailbox with the address, keep conversations when the address changes, and remove only what it wrote.
- **FR-006** Health MUST report configuration, fetch and send as `ok`, `failing`, `pending` or `not_configured`:
  a sign-in to each server (nothing read or sent), that each fetched folder exists, the age of the last successful scheduled fetch, and failed or
  waiting replies. Messages MUST be fixed texts: the mail libraries' errors quote commands, the SMTP one the password
  in base64.
- **FR-007** `can support credentials` MUST print the password only to a terminal or with `--print-secrets`.
- **FR-008** The public site MUST NOT link to the helpdesk's sign-in; `site.support` links to the address and is
  left out, with a warning, without one.
- **FR-009** A k3d job MUST prove inbound mail to ticket and reply to delivery through can-core.
- **FR-010** FreeScout MUST verify the relay's certificate and name. Only `tls_verify: "false"` in the relay Secret,
  with its `host`, may turn that off.
- **FR-011** Values from `company.yaml` MUST reach the container literally (`$` escaped against `$(VAR)` expansion).

## Design notes

- FreeScout has no command or API (in its free core) to create a mailbox. The script boots the application and uses
  `App\Mailbox` and `App\Option`, whose setters encrypt the passwords as its own forms do. This depends on the
  internals of FreeScout 1.8.252: a version bump must run the `support-mail` job.
- The script is part of the pod's command (`chart/templates/_provision.tpl`), so a change of it restarts the pod.
- Upstream sets `verify_peer` false in its copy of SwiftMailer, but merges the transport's stream options over it,
  and its `TransportManager` passes `config('mail.stream')`. The pod's command keeps upstream's `config/mail.php`,
  wraps it with `stream.ssl` before the first boot step caches the configuration, and stops if it cannot. A copy of
  the file in a ConfigMap was not used: it would drift from upstream.
- Options `can_mailbox_id`, `can_mailbox_in`, `can_mailbox_out` and `can_system_mail` record what was written.

## What cannot be done, or was not

- **STARTTLS downgrade** (upstream SwiftMailer): if `EHLO` is refused it sends `HELO`, skips STARTTLS and sends in the
  clear, without authenticating. Implicit TLS (465) is not affected and is recommended.
- OAuth mailboxes need a consent in the browser and are not configured. POP3 is not offered.
- The inbound-parse webhook still needs the adapter (specs/007 T030).
- DNS records (SPF, DKIM, DMARC) and the mailbox itself are the operator's.
- The admin UI's status page does not show mail health; `retention_days` is still not applied.
- `support` stays outside `make up`'s default bundle and the starter template (SPEC.md section 10): third-party
  image, never on a real server.
- Mail settings written by the release before this one (system mail) are not removed on opt-out.
- **Upgrade hazards** ([README](../../modules/support/README.md#upgrading-from-module-010)): a hand-made mailbox with
  the support address is taken over; hand-configured mailboxes with unverifiable certificates stop sending.

## Success Criteria

- **SC-001** `make test` passes.
- **SC-002** The `support-mail` and `wrapped-modules` jobs pass on the pull request.

## Out of scope

Listmonk's SMTP settings; the `tickets` adapter; ticket events; upgrades between FreeScout versions.

## Verification

- Unit tests: settings, credential binding, the chart's use of Secrets, `can support`, verify checks, site section,
  the mail server.
- `test/integration/support_mail_test.go` with `dev/mailsink`. The test adds a network rule so that the module can
  reach a server inside the cluster. It shows the self-signed relay refused by default, then accepted with the opt-out;
  IMAP verification is off throughout.
- Locally also: a trusted certificate with the right name is accepted and with another name refused (STARTTLS); the
  self-signed one is refused on implicit TLS.
- Locally (rootless podman, the pinned images): the chart's start command, mailbox creation, scheduled fetch, a
  reply, rename, removal, address clash and each failure message. IMAP over STARTTLS was not exercised against a
  server that offers it.
