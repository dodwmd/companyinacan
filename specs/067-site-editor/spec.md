# Feature 067: website editor in the admin UI (E04c)

Status: built. Unit tested (fake Kubernetes API), compose end-to-end and headless Chrome; the admission
policies are proven by server-side dry runs on k3d (`dev-cluster` workflow). Not run: publishing from a
deployed can-core to a running site pod on a cluster, or any real server.

## Why

SPEC §1 and §6.2/6.3 describe a control panel for business people, but the website could only be changed by
editing `company.yaml` and running the CLI, and the renderer produced one page and a 404.

## Requirements

- Edit every section the renderer has, and theme basics, from `/site`; errors per field; no JavaScript.
- Preview before publish, rendered by the same renderer, with no script and isolated from the admin UI.
- Publish from can-core with the least write access that works, bounded by admission.
- History and rollback.
- The admin UI and `can up` / `can apply` never silently overwrite each other.
- `platform.operate`; recent proof of identity to publish; CSRF; audit.
- Extra text pages (privacy, terms, custom) and a careers link when jobs is enabled.

## Decisions

- **Source of truth** stays the applied `company.yaml`. The editor replaces `site:` and keeps comments and
  unchanged formatting; an unchanged section leaves the document byte for byte. `site.brand.logo` is never
  changed by the editor or by a rollback.
- **Conflicts**: the editor compares a hash of the `site:` section (not of the whole document), so module
  changes made meanwhile do not block a publish and are kept; the write itself is conditional on the version
  read. `can apply` compares the cluster document with what this directory last applied or pulled
  (`.can/state.json`, per ConfigMap UID) and with the annotation can-core puts on documents it writes
  (`can.dev/server-edit-sha256`). It applies with the resourceVersion it checked. `can pull` is new.
- **Preview** is a stored draft (24 h, 20 kept) shown in `<iframe sandbox="">`; its response has
  `Content-Security-Policy: sandbox; default-src 'none'` with the stylesheet allowed by hash.
- **Step-up for publish and rollback**: yes. A changed public site is what a stolen session would use for
  phishing.
- **Publish path**: before this feature only `can up` wrote the site. can-core's RoleBinding in `can-system`
  already allowed the writes (and far more); no RBAC was added except `get` on the two new admission
  policies, which narrow it.
- **Order of a publish**: audit, record the previous content, write `company.yaml` (conditional), replace the
  site. If the last step fails the same draft can be published again.
- **Careers** is opt-in (`site.careers`), like `site.contact`, so an upgrade changes no published page.

## Limits

[docs/site-editor.md](../../docs/site-editor.md). No visual editor, uploads or auto-publish of CLI changes.
`can apply` has the same short window as `can up` for the documents it does not guard with a resourceVersion
(`--force-company`). A careers link points at `careers.<domain>`, which does not exist on an sslip.io
install.
