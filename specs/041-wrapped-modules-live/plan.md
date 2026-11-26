# Implementation Plan: Wrapped modules that install

**Spec**: [spec.md](spec.md) | **Research**: [research.md](research.md) | **Tasks**: [tasks.md](tasks.md)

## Constitution check

- Footprint (II): no new always-on component unless a module needs it; Postgres is created on demand;
  figures in manifests come from measurements.
- Licences (VI): no AGPL or EUPL code is linked or vendored; the apps run unmodified in their own
  containers. Postgres (PostgreSQL licence) is infrastructure, not a module.
- Security: restricted pods, no default passwords, per-module database roles, secrets only in Secrets.
- Contracts (IV): `module.v1` is unchanged; no new Go dependency.

## Design

| Concern | Where |
|---|---|
| Shared Postgres | `deploy/templates/postgres.yaml`, `core/server/dbprov/managed.go` (can-core already holds `can-core-module` in `can-data`) |
| Wiring | `core/server/runtime/sync.go`: managed server unless `CAN_POSTGRES_ADMIN_URL` is set |
| Admin passwords | `core/server/modsecrets` (kind `Password`, never rotated) |
| SMTP | `modsecrets.WantsRelay`, `installer.Config.Relay`, `runtime.appRelay`, Secret `can-smtp` |
| analytics | init container creates the site and user (idempotent: migrate, `db show`, `db create`) |
| support | the image's entrypoint, plus a one-shot beside it that writes the relay into FreeScout's options after the first boot |
| newsletter | existing migrate init container; admin from `LISTMONK_ADMIN_USER/PASSWORD` |
| CI | `.github/workflows/wrapped-modules.yml`, `test/integration/wrapped_test.go` |

`core/server/runtime/runtime.go` and `deploy/can-core` are not touched, to stay out of the way of the
backup work (PR #49), which does edit them.

## Decisions

1. **Postgres for FreeScout, not MariaDB.** FreeScout and this image support it, can-core already
   provisions it, and one engine for support and newsletter costs one server's memory.
2. **Superuser for provisioning.** On Postgres 16 and later a `CREATEROLE` role cannot create a database
   owned by a role it made without extra grants, so the existing provisioner needs a superuser on the
   managed server. can-core already administers `can-data`, so this grants it nothing new.
3. **Which modules get the relay is code, not manifest**, like generated secrets.
