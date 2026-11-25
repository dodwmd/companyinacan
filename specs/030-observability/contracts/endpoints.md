# Contract: endpoints, environment and exit behaviour

## `GET /healthz` (unchanged)

Liveness. 200 when the process serves (modules: when the database answers). The `-healthcheck` flag
(can-static) and `healthcheck` command (modules) still probe it and exit 0 or 1.

## `GET /readyz` (new, every in-house service, same listener as `/healthz`)

| Condition | Status | Body |
|---|---|---|
| all components ok | 200 | `{"status":"ok","components":[{"name":"database","status":"ok"},...]}` |
| a non-critical component failing | 200 | `"status":"degraded"`, that component `degraded` |
| a critical component failing or timing out | 503 | `"status":"down"`, that component `down` |

`HEAD` answers the status line only. Components: `database`, `migrations` (all SQLite services),
`reconcile` (can-core, non-critical). Bodies never contain error text, hosts, addresses or credentials.
`Cache-Control: no-store`. Results cached for 2 s. can-static reports an empty component list (always ok).

## `GET /metrics` (new, dedicated listener only)

- Listener: `CAN_METRICS_ADDR`; unset means no listener. `/metrics` is not served on any other listener.
- Auth: `Authorization: Bearer <CAN_METRICS_TOKEN>` (or token read from `CAN_METRICS_TOKEN_FILE`); constant-time
  comparison; 401 with `WWW-Authenticate: Bearer realm="metrics"` otherwise; no throttling.
- A non-loopback address without a token: the process exits at start-up with a configuration error (exit 2
  for modules, error from `Run` for can-core, 2 for can-static).
- Methods: GET, HEAD; others 405. Other paths 404.
- Content type `text/plain; version=0.0.4; charset=utf-8`.

## Environment

| Variable | Used by | Meaning |
|---|---|---|
| `CAN_METRICS_ADDR` | all | metrics listener address |
| `CAN_METRICS_TOKEN`, `CAN_METRICS_TOKEN_FILE` | all | bearer token |
| `CAN_RAM_DECLARED_MB`, `CAN_RAM_DECLARED_BYTES` | all | declared RAM for `can_ram_declared_bytes` |
| `CAN_LOG_LEVEL`, `CAN_LOG_FORMAT` (`json`, `text`) | all but can-static | logging |

## Request id

Request header `X-Request-Id` accepted only if it matches `^[A-Za-z0-9._-]{1,64}$`; otherwise ignored.
Response header `X-Request-Id` always set by services behind the middleware.

## CLI

- `can diagnose [--out FILE] [--company-dir DIR] [--log-file FILE]... [--compose-project NAME]
  [--metrics-url URL] [--lines N]`. Token only from `$CAN_METRICS_TOKEN`. Exit 0 on a written bundle; 1 on
  I/O errors or an existing `--out`; 2 on usage errors (credentials in the URL, `--lines` out of range).
- `can-core admin diagnose [--db FILE] [--out FILE] [--company-file FILE] [--log-file FILE] [--lines N]`.
  Opens the database read-only; fails if it does not exist.
- `can doctor`: unchanged exit codes; extra result rows `docker daemon`, `compose stack companyinacan`,
  `kubernetes API` with status `ok`, `unreachable` or `not-running`.
