# Contract: company.yaml patches and the stored document (E12b)

## The stored document

`company.yaml` is applied from one store (`modmgr.CompanyStore`):

| Mode | Store | Version |
|---|---|---|
| `CAN_INSTALLER=kubernetes` | ConfigMap `can-system/company`, key `company.yaml` | `rv:<metadata.resourceVersion>` |
| development, tests | file `CAN_COMPANY_FILE` | `sha:<first 8 bytes of SHA-256, hex>` |

`Put(data, expected)` succeeds only if the stored version equals `expected`; otherwise `ErrConflict`. The
ConfigMap write also sends `metadata.resourceVersion` so the API server enforces it. Maximum document: 256 KiB.

`can apply` (CLI) writes the same ConfigMap with `--force-conflicts` and is not version-checked. The admin
UI shows the stored document at `/company` so it can be copied back into the company repository.

## Patch

```json
{"ops":[{"path":["modules","web","enabled"],"value":true},
        {"path":["modules","web","settings"],"value":{"site_title":"Acme","max_rows":7}}]}
```

Allowed paths (anything else fails the whole patch with `ErrPatchPath`):

| Path | Value |
|---|---|
| `name`, `domain` | string |
| `site.template` | string |
| `provider.options.server_ram_mb` | integer |
| `primary.<capability>` | module id |
| `modules.<id>.enabled` | boolean |
| `modules.<id>.settings` | object (validated against the module's `settings.schema.json`; refused if the module has no schema and the object is non-empty) |

Rules: at most 200 operations and 64 KiB; module ids match `^[a-z][a-z0-9-]{1,30}$`; integers stay integers
through JSON; comments and untouched keys are preserved; missing intermediate mappings are created.

## Validation of a proposal

1. Apply the patch to the stored document (in memory).
2. Validate the result with `validate.CompanyBytes` (the checks of `can validate`: schema, manifests of
   enabled modules, duplicate subdomains, exclusive capabilities and primaries).
3. For each module the patch newly enables: it exists, is deployable (`status` is not `not-deployable`,
   manifest valid) and carries its acknowledgements (`licence:<id>` for `licence_class: source-available`,
   `experimental:<id>` for `status: experimental`).
4. Compare the requires/provides analysis before and after; refuse blockers the change introduces.
5. Store the proposal (`company_changes`), audit `company.change.propose` (paths only, no values).

## Apply

1. Reject if applied before (returns its run), expired (> 24 h), or stale (stored version differs).
2. Re-run the validation above against the current document.
3. Claim the single apply slot (`ErrBusy` if taken).
4. Audit `company.change.begin`; **if this fails nothing is written**.
5. `Put` with the expected version (`ErrStale` on conflict).
6. Audit `company.change.saved`; mark the change applied; start the reconcile.
7. When it ends: update the run, record the reconcile status, audit `company.apply.done`.

## Audit actions

`company.change.propose`, `company.change.begin`, `company.change.saved`, `company.apply.done`,
`module.purge.begin`, `module.purge.done`, `module.purge.failed`, `wizard.update`, `wizard.complete`,
`wizard.dismiss`, `email.relay.set`, `email.relay.failed`. Details carry paths, versions, run ids and error
texts (sanitised); never setting values, passwords or Secret contents.
