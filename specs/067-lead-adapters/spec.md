# Feature Specification: website leads reach the support inbox and the newsletter (E07d)

**Feature Branch**: `epic/E07d-lead-adapters`
**Created**: 2026-11-26
**Status**: Built. Verified on k3d (`lead-adapters` CI job) with the pinned FreeScout and Listmonk images and an
in-memory mail server. No real mail provider, domain or server.
**Input**: SPEC.md section 4.3 (lead flow); [007](../007-starter-modules/spec.md),
[019](../019-wiki-crm-status/contracts/lead-to-crm-adapter.md), [041](../041-wrapped-modules-live/spec.md),
[054](../054-lead-erasure/spec.md), [059](../059-support-mailbox/spec.md).

## Goal

A lead was stored, notified and then went nowhere. When `company.yaml` names a form, its leads become support
conversations, or, for senders who ticked a consent box, unconfirmed newsletter subscriptions.

## Requirements

- **FR-001** Nothing is handed on unless `company.yaml` names the form:
  `modules.support.settings.tickets_from_forms`, `modules.newsletter.settings.subscribe_from_forms`.
- **FR-002** Forms have a field type `consent`: an unticked checkbox with its wording, never required.
  `modules.forms.settings.newsletter_consent: {text, forms}` adds one named `newsletter`. A lead records which
  consent boxes were ticked (`consents`); a value in a field of another type is not consent.
- **FR-003** The newsletter adapter MUST NOT call Listmonk for a lead without that record, and MUST refuse a list
  that is not public and double opt-in.
- **FR-004** The adapters run inside can-core, driven by the event outbox: a failed delivery is retried with
  backoff, dead-lettered after 8 attempts, shown with its reason in the admin UI (Events) and can be requeued.
  A redelivery makes nothing twice.
- **FR-005** Events stay ids only; the lead is read from `leads` at delivery time. can-core keeps ids only
  (`lead_links`). Delivery errors are fixed texts: no response body is recorded. Logs and audit entries
  (`lead.forward`, `lead.erase_copy`, `lead.copy_kept`) name the lead id and the module.
- **FR-006** `lead.erased.v1` reaches every module that holds something made from the lead, also one that is
  disabled (the delivery waits, and is not pruned). Newsletter: the subscriber the lead created is deleted.
  Support: an internal note on the conversation, which is reopened.
- **FR-007** Credentials. Listmonk: an API user whose list role covers the one list, in the Secret
  `can-system/can-newsletter-api` (`username`, `token`), mounted in can-core only. Subscribing itself uses
  Listmonk's public endpoint and no credential. Support: no credential of FreeScout's; requests are signed with
  the module's webhook secret.
- **FR-008** No new network rule: can-core could already reach module namespaces. The support listener's port
  has no route.
- **FR-009** No unpublished image.
- **FR-010** An upgrade of an installed `support` or `newsletter` to the version that receives leads waits for
  approval (the rule of can-core's unattended upgrades).
- **FR-011** No automatic reply is sent for a conversation made from a form: the address is unverified.

## Design

- **Why in can-core.** The outbox already gives idempotency keys, retries, dead letters and their UI. A
  subscription marked `via: can-core` in a manifest is delivered to an in-process adapter instead of a webhook.
- **Why a listener in the support pod.** FreeScout's free core has no API. As for its mail settings
  ([059](../059-support-mailbox/spec.md)), a PHP script in the chart uses FreeScout's own models, served by PHP's
  built-in server on port 8081, only while a form is named. It depends on FreeScout 1.8.252's internals.
- **Listmonk 6.2.0**, as probed: the API user creates the subscriber (on no list, with the attribute
  `can_lead`), then the public subscription adds the list and sends the confirmation, and reports a mail failure.
  Its search finds nothing for a `+`, so the longest part without one is searched.
- **CRM** stays out: not deployable. Its subscriptions remain `planned`.

## Limits

- A conversation and its customer record keep their copy until an agent deletes them: the note asks for it.
- A subscriber who existed before the form is never deleted (`lead.copy_kept`): erase them in Listmonk if the
  request covers it. Also kept while another lead that was not erased led to the same record.
- A subscriber taken off the list by hand is out of the API user's reach: the erasure is dead-lettered and says so.
- A crash between creating a subscriber and recording its number can leave a record on no list, which no
  erasure finds. Mail already sent, Listmonk's and FreeScout's own logs, exports and backups are not touched.
- A dead-lettered erasure is kept until requeued, but the admin UI lists only the latest deliveries.
- "Delivered" also means "nothing to do" (form not named, no consent, lead already erased).
- The consent record is the fact and the lead's date; the wording is not stored with it. Keep a record of what
  you showed. The double opt-in confirmation is Listmonk's.
- Anybody can type anybody's address: the confirmation mail then goes to a stranger (rate limited by forms), and
  a support conversation can carry somebody else's address. Check before replying. The name in the confirmation
  mail is replaced when it is long or looks like a link.
- Leads that arrived while the module was off, or before the form was named, are not handed on later.
- `forms` publishes `lead.created.v1` without an outbox of its own: if can-core is unreachable for its retries,
  that lead is stored and notified but not handed on.
- After a restore of can-core, the applications and the links are as old as their backups.
- The Listmonk API user, its roles, the list and the SMTP settings are made by hand.
- Neither adapter provides the `tickets` or `newsletter` capability; those stay `planned`.

## Verification

- Unit: consent field and record, adapters against fake services (consent, double opt-in, repeats, erasure,
  errors without personal data), outbox retry, dead letter, requeue and erasure of a disabled module, approval.
- `test/integration/lead_adapters_test.go` (`lead-adapters` job) on k3d.
- Locally (podman, the pinned images): the Listmonk calls and permissions, the support listener, no auto reply.
- Not verified: a real server or mail provider, an upgrade of a real installation, volume.
