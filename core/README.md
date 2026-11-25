# core

Go packages for the `can` CLI and, in later epics, can-core.

- `cli/` cobra commands (init, validate, doctor, apply, version, licences)
- `config/` YAML loading with source positions, typed company and module structs
- `schema/` embedded JSON Schema validation and error mapping
- `validate/` semantic rules and company/module validation entry points
- `licence/` accepted-licence evaluation and THIRD_PARTY_LICENSES generation
- `doctor/` local prerequisite checks
- `problem/` the shared validation result type
- `server/` can-core: SQLite state, admin and module APIs, registry, audit log, events and webhooks, reconcile (see `specs/005-can-core-mvp/`)
