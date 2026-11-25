# Data Model: E06

## State (`.can/state.json`, mode 0600, version 1)

| Field | Type | Notes |
|---|---|---|
| `version` | int | `1`; a newer file is refused |
| `provider` | string | provider that wrote it |
| `server` | object? | `id` (hetzner), `name`, `ip` (host for ssh), `user`, `port` |
| `ssh_key_id`, `ssh_key_owned` | int, bool | Hetzner key; deleted on `down` only when owned |
| `firewall_id` | int | Hetzner cloud firewall |
| `steps` | map id -> RFC 3339 | completed idempotent steps (`ssh.bootstrap`, `hetzner.bootstrap`) |
| `k3s_version` | string | version the bootstrap installed |
| `issued` | list | ACME issuance history `{at, hosts, ok}`, pruned after 14 days |

Never stored: tokens, passwords, private keys, kubeconfig contents.

## Provider interface

```go
type Provider interface {
    Name() string
    Plan(ctx, Spec, *Store) ([]Action, error)             // read-only
    Create(ctx, Spec, *Store, io.Writer) error            // idempotent
    Destroy(ctx, Spec, *Store, io.Writer) error           // idempotent
    Status(ctx, Spec, *Store) (Status, error)             // read-only
    Address(*Store) (string, error)                       // from state
}
```

`Status.Phase`: `absent`, `provisioning`, `running`, `ready`, `degraded`. `ready` requires bootstrap
marker, `k3s` service active and one Ready node.

## Runner

```go
type Runner interface {
    Run(ctx, script string) (output string, err error)   // script on stdin of `bash -s`
    Copy(ctx, remotePath string, data []byte, mode string) error
}
```

## Bootstrap options

`K3sVersion` (pinned `vX.Y.Z+k3sN`), `NodeName`, `SSHPort`, `OperatorCIDR`, `PodCIDR`
(10.42.0.0/16), `ServiceCIDR` (10.43.0.0/16), `SwapMB` (1024), `ImageTarballs[]{Name, URL(https),
SHA256}`.

## Instant plan

`mode` (`https` | `http-fallback`), `reason`, `hosts`, `urls`, `admin_via_tunnel_only`.

## DNS record

`type` (A/AAAA), `name`, `value`, `ttl` (300).
