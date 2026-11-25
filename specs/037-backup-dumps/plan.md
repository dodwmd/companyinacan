# Implementation plan: E08b

Design A' of [research.md](research.md).

| Part | Where |
|---|---|
| dump store, runner, marker, schedule, request file, hook Jobs | `core/server/dumps` |
| `PUT\|GET /v1/dumps` | `core/server/moduleapi/dumps.go` |
| wiring, `CAN_DUMPS_DIR`, `can-core dump-request` | `core/server/runtime`, `core/server/corecli` |
| static PV and claim | `deploy/can-core/can-core.yaml` |
| directory ownership | `core/provider/bootstrap.go`, `core/launch/backup.go`, `can backup install` |
| pinned restic | `core/backup/provision.go` |
| CLI override removed | `core/cli` |
| k3d verification | `test/integration/backup_k3d_test.go`, workflows `make-up-e2e`, `dev-cluster` |

## Constitution check

- **Small footprint**: no new container or image; dumps run inside can-core (limit unchanged, measured).
- **Security by default**: no pod hostPath; no new RBAC (Jobs, ConfigMaps and claim listing were already granted per
  module namespace); hook pods restricted, no ServiceAccount token other than the `can` audience one.
- **Honesty**: restore, real S3, real server and the hooks image are listed as unverified in docs/backup-restore.md.
- **Test first**: unit tests for each package written before the code.
- **Licences**: no new Go dependency. restic (BSD-2-Clause) is downloaded on the server, not distributed.
