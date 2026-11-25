# wiki

> **Status: experimental.** Never run on a cluster. The `wiki` capability is marked `planned`
> (adapter not built, T030); the first admin account is created by hand (see "First run").

Internal wiki and optional public help centre. Wraps [DokuWiki](https://github.com/dokuwiki/dokuwiki)
(PHP, flat files) and provides the `wiki` capability.

| | |
|---|---|
| Licence | GPL-2.0 (recorded as `GPL-2.0-only`; whether upstream means "only" or "or later" is not verified, specs/019-wiki-crm-status/research.md R-licences). GPL-2.0 has no network clause, so running it as a service creates no source-offer duty, and unmodified aggregation does not reach this repository's Apache-2.0 code. Still a copyleft licence for anything you patch and distribute: see [modules/README.md](../README.md#licences-and-network-copyleft). |
| Footprint (estimate) | 64 MB request (app 56 MB, adapter 8 MB), 160 MB limit, 2 GiB disk. No database. **Unverified estimate, not measured**; SPEC.md section 6.1 put the BookStack/Wiki.js class at ~200 MB plus a database. |
| Database | none: pages, history and media are files on the module's PVC (`/storage`) |
| Public host | `wiki.<domain>` |
| SSO | `none`. DokuWiki has its own user file; OIDC exists only as a community plugin (unverified), so the module does not claim `sso: oidc`. |
| Capability | `wiki` (`schemas/capabilities/wiki.v1.json`), served by `wiki-adapter` (:8081, off until the adapter image exists) |
| Backup | `hooks/backup.sh` tars the storage directory and PUTs it to can-core; `hooks/restore.sh` replaces the volume contents from it |
| Data classes | `wiki-page` 10 years, `wiki-revision` 2 years, standard sensitivity |

Do not keep HR, payroll or other sensitive records in this wiki: third-party modules may not hold
sensitive data classes. The public help centre is a `help` namespace the operator makes readable;
nothing is public by default.

## Why DokuWiki

Flat files mean no MariaDB or Postgres to run, back up or size, and the lowest idle memory of the
candidates. The trade is features: no WYSIWYG editor by default, a dated look, ACLs configured
through its own admin pages, and no built-in OIDC. See research.md R-wiki for the comparison.

## Alternative (heavier)

**BookStack** (MIT, PHP, needs MariaDB; GitHub reports its licence as MIT) has a friendlier editor
and a book/chapter/page model, at roughly the ~200 MB catalogue estimate plus the database. To use it
write a sibling module `wiki-bookstack` that provides the same `wiki` capability; two providers of one
capability need the operator to name a primary in `company.yaml`. Wiki.js is AGPL-3.0 (network
copyleft) and Docmost is AGPL-3.0 with a separate enterprise licence for some features; Outline is
source-available and needs Postgres, Redis and object storage, so it is excluded. None are built here.

## First run (manual) and open items (unverified offline)

- **Install.** Open `/install.php` through a port-forward, create the superuser, then remove
  `install.php` (the upstream installer asks for this). The `can-module` admin credentials are not
  consumed by DokuWiki; the adapter will later create the account.
- **Non-root.** Upstream's README describes running as a uid (`--user 1000:1000`) with a writable
  `/storage`; the chart asks for exactly that. Whether the image starts cleanly with all capabilities
  dropped and the restricted seccomp profile is **unverified**.
- **Pin the image.** `stable` is a floating tag and `image.digest` is a `REPLACE_WITH_DIGEST`
  placeholder; `make release-check` fails until both are resolved.
- **Mail.** DokuWiki sends notifications through PHP mail; it does not read `can-smtp`. Not wired.
- **Retention.** The `wiki-revision` class is declared; automatic pruning of old revisions is not built.
