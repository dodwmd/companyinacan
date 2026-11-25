# Research: module management and onboarding wizard (E12b)

## R1. Where does the UI write `company.yaml`?

**Decision.** The single source of truth is the stored document the reconciler applies: the ConfigMap
`can-system/company` in a cluster (`KubeStore`), the file `CAN_COMPANY_FILE` in development (`FileStore`).
The UI reads and writes it through `modmgr.CompanyStore` (`Get`, and `Put` with the expected version).
`can apply` writes the same ConfigMap with server-side apply and `--force-conflicts`.

**Why not the mounted file.** The ConfigMap is mounted read-only into can-core, and the kubelet refreshes
the mount only after up to about a minute. If the periodic loop kept reading the mount, a change the UI
just stored would be reverted by the next tick. So the loop reads the store (`reconcileStored`); the mount
is only used when no store override exists in tests and in `reconcileOnce` (kept for tests and `can apply`
parity).

**Concurrency.** A version per document: `rv:<resourceVersion>` for the ConfigMap (sent as a precondition
in the merge patch, so the API server rejects a concurrent writer with 409) and `sha:<hash>` for a file.
A stored proposal remembers the version it was prepared against; apply refuses it when the version moved
(`ErrStale`, 409). Two UI sessions or the UI and `can apply` therefore cannot silently overwrite each other,
except that `can apply` forces its own write: documented, with the advice to copy the stored document (the
`/company` page) into the repository first.

**Patches, not rewrites.** A change is a list of allow-listed `set` operations on a `yaml.Node` tree
(`name`, `domain`, `site.template`, `provider.options.server_ram_mb`, `primary.<cap>`,
`modules.<id>.enabled`, `modules.<id>.settings`). Comments and every untouched key survive. Any other
path fails the whole patch (fail closed). The patch is stored as JSON (integers kept as integers) so the
proposal can be re-applied to whatever the document is when it is previewed.

## R2. Settings forms from JSON Schema

**Decision.** A safe subset is rendered: `type: string` (with `maxLength`), `integer` (with `minimum` and
`maximum`), `boolean`, `enum` of strings, and `format: secret-ref`. Everything else is not rendered and its
current value is preserved byte for byte (the form lists it as "edit in company.yaml").

**Validation.** The merged object is validated with the module's own schema using the
`jsonschema/v6` library that already validates manifests. Browser attributes are hints only. The schema
loader refuses every external `$ref` (no network or file access from a hostile schema); schema files must
live inside the module directory and are limited to 64 KiB.

**Secrets.** A `secret-ref` setting is the *name* of a Kubernetes Secret (optionally `name/key`), checked
against a DNS-label-like pattern, so a pasted password with spaces or symbols is rejected. Secret values never
enter `company.yaml` or the audit log. The existing module schemas already say "secrets are never settings".

## R3. Footprint constants

SPEC section 3.2 budgets k3s + Traefik + CoreDNS + local-path at about 600 MB and can-core with the site at
about 100 MB (forms is counted as a module, so 50 MB of that 100 is used here), and a shared database engine
at 150 to 400 MB. The meter uses `PlatformBaselineMB = 650` and `DatabaseEngineMB = 150` once if any enabled
module needs Postgres or MariaDB. These are estimates from the spec and the CI measurements in
`docs/measurements/phase0-ci.md`, not measurements of the operator's server, and the page says so. The
server size is `provider.options.server_ram_mb` (the free-form options object already allowed by the company
schema); without it the verdict is `unknown`. "Tight" is above 85 percent. The preview shows the total for the
*proposed* selection, which is the live number before the operator commits.

## R4. Requires/provides analysis

Per selected module and required capability: `met` (a selected module serves it), `platform` (can-core provides
it, from `schemas/names.yaml`), `planned` (only a planned provision exists, shown "not yet served" and counted
as unmet), `missing` (an unselected module could serve it) or `none`. Exclusive capabilities (not `aggregate`)
served by two selected modules without a `primary` are conflicts. A change is refused only for blockers it
*introduces*: problems that already existed in `company.yaml` do not stop an unrelated settings change.

## R5. Apply and progress

`Apply` claims the single apply slot first (a unique partial index on `state = 'running'`), then audits the
intent, then writes the store, then starts the reconcile in a goroutine with a 15 minute timeout. A failure
before the write closes the run as failed and leaves `company.yaml` untouched. Runs interrupted by a restart
are closed at start-up. The reconcile itself is `reconcile.Reconciler.Apply` behind a `Runner` mutex shared
with the periodic loop, so the two never run at once. The progress page polls with
`<meta http-equiv="refresh" content="3">`; `?refresh=0` turns it off (WCAG 2.2.1: the user can stop the reload).

## R6. Purge

There is no purge in the installer today (the manifest declares a `purge` hook). The destructive step is an
interface (`Purger`), not wired in this build, and the UI flow around it is complete and tested with a fake:
distinct permission, typed `purge <id>`, current password, recorded state disabled and `company.yaml` not
enabling it, audit before and after, deletion of the module record only after the executor succeeded. The
spec's "approval for destructive purge" is implemented as these three independent gates rather than a
workflow through the approvals engine (a possible follow-up: an `approvals` type for purge).

## R7. Wizard state

One row in `wizard_state` (an installation has one company), JSON answers, a `revision` for optimistic
saving and an `apply_key` (hash of patch, stored version and acknowledgements) for idempotent apply. The
first read seeds the answers from the stored `company.yaml`. Completing or dismissing closes it. Hardening
status is read from the audit log (`admin.password.change`, `admin.totp.rotate` by the user), not from a
checkbox. The recovery kit step is an acknowledgement only; the kit itself is made by `can backup kit`
(E08), which this UI does not run.

## R8. Why `meta refresh` and not a streaming response

No JavaScript is allowed and CSP forbids scripts. Server-sent events would need a held connection per viewer
and a custom client; a three-second reload of a small page is simpler and keeps working through the ingress.

## R9. Sanitising errors

`modmgr.Sanitise` strips control characters, redacts URL credentials and `password=`, `token:`, `secret=`,
`api_key=` and `Authorization: Bearer ...` pairs, and truncates to 240 characters. It is applied to every
installer or cluster error before it is stored in a run, a status row or a page. It is a best-effort filter
(documented as such): the installer remains responsible for not putting secrets in errors.
