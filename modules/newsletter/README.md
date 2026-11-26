# newsletter

> **Status: experimental.** Installed through can-core on k3d in the advisory `wrapped-modules` CI job
> ([specs/041](../../specs/041-wrapped-modules-live/spec.md)); never on a real server or mail relay. The `newsletter`
> capability is marked `planned` (adapter not built, T031); website leads are subscribed by can-core
> ([specs/069](../../specs/069-lead-adapters/spec.md)). The
> **SMTP/bulk-relay settings are manual**: enter the relay in Listmonk Settings, SMTP. can-core gives
> this module no mail credentials.

> **Backups:** can-core takes a `pg_dump` of the database and an archive of the volume (uploads) in every dump run, and no unpublished image is needed
> ([docs/backup-restore.md](../../docs/backup-restore.md#wrapped-modules)). Verified on k3d only.

Mailing lists, double opt-in and campaigns. Wraps [Listmonk](https://github.com/knadh/listmonk)
(Go), provides the `newsletter` capability, publishes `subscriber.added.v1`, and optionally
subscribes people who tick a newsletter box on a form (consumes `leads`, subscribes to
`lead.created.v1`).

| | |
|---|---|
| Licence | AGPL-3.0 (`AGPL-3.0-only` in SPDX). Network copyleft: see [modules/README.md](../README.md#licences-and-network-copyleft). We run it unmodified. |
| Image | `listmonk/listmonk:v6.2.0` (upstream), pinned by digest |
| Footprint | 50 MB request, 128 MB limit, 1 GiB disk (uploads). Measured 26 to 29 MiB idle on k3d, 31 to 35 MB locally, plus the shared Postgres. A one-shot `migrate` init container (up to 96 MB) runs before the app |
| Database | Postgres (shared instance in `can-data`, own database and user) |
| Public host | `newsletter.<domain>` (admin UI, subscription and unsubscribe pages) |
| Email | needs `email.send`. A separate bulk relay is recommended: [docs/email-policy.md](../../docs/email-policy.md) |
| Capability | `newsletter` (`schemas/capabilities/newsletter.v1.json`), served by `newsletter-adapter` (:8081, off until the adapter image exists) |
| Backup | can-core: a `pg_dump` of the database and an archive of the volume (uploads) ([specs/046](../../specs/046-wrapped-backups/spec.md)) |
| Data classes | `newsletter-subscriber` 3 years after last activity, `campaign-engagement` 2 years, standard sensitivity |

## Consent

Subscribing from a website form happens only for the forms named in `subscribe_from_forms`, only for a sender who
ticked the consent checkbox, and only on a **double opt-in** list: they stay `unconfirmed` and receive no campaign
until they follow the link in the confirmation mail. A list that is not public and double opt-in is refused.

## Subscribe website leads

1. In Listmonk: set up SMTP and the root URL (Settings), and make the list: **public**, **double opt-in**.
2. Users, Roles: a *user role* with only `subscribers:get` and `subscribers:manage`, and a *list role* with
   `list:get` and `list:manage` on that one list. Users: an **API** user with both. Copy its token (shown once).
3. Give can-core the token (it is not read from `company.yaml`):

   ```sh
   kubectl -n can-system create secret generic can-newsletter-api \
     --from-literal=username=can_leads --from-file=token=./token   # the file holds the token, nothing else
   ```

4. `company.yaml`:

   ```yaml
   modules:
     forms:
       settings:
         newsletter_consent: { text: "Send me the newsletter. I can unsubscribe at any time." }   # forms: [contact]
     newsletter:
       settings:
         subscribe_from_forms: [contact]
         default_list: Newsletter            # the list's name or UUID
   ```

Deliveries and their errors are in the admin UI under Events; a dead one can be requeued once the cause is fixed.
Erasing a lead deletes the subscriber that lead created. A subscriber who was there before is **not** deleted
(audit: `lead.copy_kept`); erase them in Listmonk. All limits: [specs/069](../../specs/069-lead-adapters/spec.md#limits).

Adding a form to `subscribe_from_forms` on an installed module **waits for your approval**, as does an unattended
upgrade from 0.1.0: can-core may then send the module personal data of leads. Nothing is sent before it is approved;
taking a form out takes effect at once.

## First sign-in

User `admin`; the password is generated for each installation:

```sh
kubectl -n mod-newsletter get secret can-module -o jsonpath='{.data.CAN_NEWSLETTER_ADMIN_PASSWORD}' | base64 -d
```

Change it in Listmonk afterwards; the Secret is not used again.

## Updates and security patches

The image is pinned by digest and nothing updates it automatically: a patch arrives only with a can-core
release that bumps the digest and the module's `version`.

## Open items

- A new installation has Listmonk's example SMTP server: set a real one before sending.
- Set the root URL in Listmonk's settings to `https://newsletter.<domain>`, or links in mail are wrong.
- Of the settings of `settings.schema.json`, only `subscribe_from_forms` and `default_list` do anything; `can validate`
  warns about the others ([docs/module-wiring.md](../../docs/module-wiring.md#settings)).
- Upgrades to a newer Listmonk are untested.
