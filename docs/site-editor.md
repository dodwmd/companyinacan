# Editing the website in the admin UI

**Website** in the admin UI (`/site`, `platform.operate`: owners and operators) edits the `site:` section of
`company.yaml`: the sections of the page (hero, features, pricing, questions, call to action, contact,
support, footer), theme basics (two colours, language, description, sharing image), extra text pages and a
careers link. It is a form, not a visual editor, and has no JavaScript.

1. Edit and press **Preview the changes**. Mistakes are listed and shown next to their fields; nothing is
   stored until the form is valid.
2. The preview shows each page in a sandboxed frame, with the difference to `company.yaml`. Links inside
   the preview do nothing; pick pages from the list above it.
3. Press **Publish**. If you signed in more than 10 minutes ago you confirm who you are first. The section is
   saved to the applied `company.yaml` and the public site is replaced; allow up to a minute.

Not editable here: the logo (a file of your company directory), the template (there is one), and everything
outside `site:`.

## Pages

```yaml
site:
  careers: { label: Careers }        # a link to https://careers.<domain>/careers, only while jobs is enabled and public
  pages:
    - slug: privacy                  # served at /privacy, linked in the footer
      title: Privacy policy
      body: |
        What we keep and why.

        ## Questions
        - write to us
```

The body is plain text: a blank line starts a paragraph, `## ` a heading, `- ` a list item. HTML in it is
shown as text. The editor offers a privacy and a terms page; one without text is not created. No legal text is
generated: what these pages say is yours to write. `index`, `404`, `assets`, `robots`, `sitemap` and `healthz`
cannot be page addresses. The careers host needs its DNS record, like any module address
([module-routing.md](module-routing.md)).

## History and rollback

**History** lists what the section was: each publish, and content found in `company.yaml` that was not
published from here (a change made with `can`). **Review and restore** shows that content as a preview;
publishing it is the rollback. The last 50 are kept in can-core's database, which is backed up. The logo is
not part of the history.

## The editor and `can`

`company.yaml` has two writers. Neither silently overwrites the other.

- **The admin UI** refuses to publish (409) when the `site:` section is no longer the one the change was
  prepared from, and writes only the document version it read. Changes to other sections made meanwhile are
  kept.
- **`can apply`** refuses to replace a document that was changed in the admin UI, or that changed since this
  directory last applied or pulled it. `can pull` writes the cluster's document into your company directory;
  `can apply --force-company` discards what is in the cluster instead.
- **`can up`** refuses in the same way (`--force-company`), so it does not rebuild the site from an older
  directory over what you published.
- **`can pull`** does not overwrite a `company.yaml` that has edits which were never applied: compare with
  `can pull --out cluster.yaml`, or `--force` (a copy stays in `company.yaml.bak`).

A change of `site:` made with `can apply` is saved but **not published** by it: the editor says the saved
content is newer than the public site, and publishing from there (or `can up`) updates it. A restore
(`can restore`) rebuilds the site from the company directory it writes.

What the directory has seen is recorded in `.can/state.json`. A directory that has never seen the cluster
(a fresh clone) applies over documents written by `can`, as before, but not over ones written by the admin UI.

## How publishing works, and what bounds it

The preview and the published pages come from the same renderer as `can up` and `can site build`. can-core
writes the packed site into ConfigMap `can-system/site-content` and stamps its digest on the pod template of
Deployment `site`, which rolls the pod; this is what `can up` does. The admission policies `can-core-site` and
`can-core-site-content` ([deploy/base/admission-policy.yaml](../deploy/base/admission-policy.yaml)) limit
can-core to exactly that: no other change to the site's workload, no new or deleted workload in `can-system`,
no other key in the ConfigMap. can-core refuses to publish while a policy is missing, so after upgrading run
`can up` once. The site must fit in 700 KiB compressed. The logo is taken from the published site: after
adding or changing a logo, publish once with `can up`.

The preview frame is served with `Content-Security-Policy: sandbox; default-src 'none'` plus the hash of the
page's own stylesheet and inline images only: no script, no forms, an opaque origin.

Publishing and failures are audited (`site.publish`, `site.publish_failed`: who, which sections, digests;
never the text). If company.yaml was saved but the site could not be replaced, publish the same change again.

In the Docker Compose stack there is no cluster: can-core writes into the directory the `site` container
serves (`CAN_SITE_DIR`, `CAN_SITE_BASE_URL`).

## Not verified

The publish path is tested against a fake Kubernetes API, the admission policies by server-side dry runs on
k3d in CI, and the whole flow on the Compose stack (HTTP and headless Chrome). Publishing from a deployed
can-core to a running site pod on a cluster, and any real server, have not been run. A visual editor, image
uploads, blog posts and more than one template are not built.
