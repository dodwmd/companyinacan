# Contract: `lead.created.v1` to CRM contact (adapter design, PLANNED)

Status: designed, **not built**. The manifest entry is `status: planned`, so can-core's event routing
ignores it until `crm-adapter` exists and `adapter.enabled` is true (the drift test ties both).

## Flow

```
forms gateway --lead.created.v1--> can-core events --(HMAC-signed POST, at least once)--> crm-adapter /hooks/lead
                                                                                           |
                                                          upsert contact (idempotent) <----+--> EspoCRM REST API
```

No public route: can-core delivers inside the cluster; the adapter's NetworkPolicy allows only
can-core in. The request carries the `can-module` webhook secret signature; the adapter rejects an
unsigned or stale delivery with 401 before parsing.

## Mapping

| lead.created.v1 | EspoCRM | Rule |
|---|---|---|
| `data.lead_id` | contact `external_key` (custom field `canLeadId`) | idempotency key |
| `data.name` | `firstName`/`lastName` | split on the last space; whole string into `lastName` if no space; empty becomes the email local part |
| `data.email` | `emailAddress` | lower-cased; absent email: still create, flagged "no email" |
| `data.party_id` | `canPartyId` | when present |
| `fields.phone` / `fields.company` | `phoneNumber` / account name | only these two named fields are mapped |
| other `fields.*` and `form_id` | one note on the contact: `Form <form_id>: ...` | text, truncated at 10 000 characters |
| (constant) | `source: form`, a Lead record optionally created per `settings.lead_intake` | |

Minimisation: nothing outside the table is copied. The note is the only free text.

## Behaviour

- **Idempotent:** `upsert_contact` with `external_key = lead_id`. A redelivery updates the same
  contact; the event `id` is also remembered for 7 days.
- **Matching:** if no contact has that key but one has the same lower-cased email, attach the key to
  it instead of creating a duplicate, and add a new note.
- **Filtering:** if `settings.lead_intake.form_ids` is non-empty only those `form_id`s are processed;
  others get 204.
- **Failure:** a 5xx from EspoCRM returns 502 to can-core, which retries with backoff (SPEC.md section
  4.3 delivery); a 4xx validation error returns 422 and is dead-lettered, not retried.
- **Consent:** the adapter does not subscribe the person to marketing; newsletter has its own
  optional `lead.created.v1` subscription keyed on its opt-in field.
- **Erasure:** `lead.erased.v1` to `/hooks/lead-erased` (`{lead_id, form_id, erased_at}`, ids only) removes
  what that lead created: its note, and the contact with `external_key = lead_id` when nothing else hangs on
  it (`erase_contact`). An unknown lead id is a 204; an erasure may arrive before the creation it undoes, so
  the id is remembered and a later `lead.created.v1` for it is dropped. A `person.erased.v1` subscription (to
  add with the adapter) covers people. Until then erasure is manual (modules/crm/README.md).

## Tests to write with the adapter

Redelivery creates one contact; same email different lead attaches; unsigned request 401; form filter;
oversize note truncated; EspoCRM down returns 502; fixtures in `testdata/capabilities/crm.v1`
(`valid-UpsertContactRequest-lead`) already pin the request shape.
