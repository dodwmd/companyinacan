# Contract: `provider.options` keys (company.yaml)

The schema keeps `options` a free map (E01); these keys are validated by the providers.

## `ssh`

| Key | Default | Notes |
|---|---|---|
| `host` | required | IP or hostname; validated (no leading `-`, no spaces) |
| `user` | `root` | non-root needs passwordless sudo |
| `port` | 22 | also the port the host firewall keeps open |
| `key_file` | ssh default | identity file |
| `operator_ip` | any | restrict SSH in the host firewall to this IP/CIDR |
| `k3s_version` | pinned default | must match `vX.Y.Z+k3sN`, v1.32.2 or later (specs/040) |
| `node_name` | `can` | DNS label |
| `swap_mb` | 1024 | used on hosts with < 4 GB RAM |
| `image_tarballs` | none | list of `{name, url (https), sha256}` pre-seeded into k3s |

## `hetzner`

| Key | Default | Notes |
|---|---|---|
| `token_secret_ref` | `env:HCLOUD_TOKEN` | `env:NAME` or `file:/path`; the value is never logged |
| `ssh_public_key_file` | `~/.ssh/id_ed25519.pub` | uploaded (or matched by fingerprint) |
| `ssh_private_key_file` | ssh default | used by the bootstrap wait and status |
| `server_type` | `cx23` | unverified default |
| `image` | `ubuntu-24.04` | |
| `location` | `fsn1` | |
| `server_name` | `can-<slug of name>` | DNS label |
| `operator_ip` | any | restricts SSH in the cloud firewall and the host firewall |
| `wait_seconds` | 600 | budget for server running plus cloud-init |
| `port` | not allowed | any value other than 22 is an error (the cloud firewall opens only 22) |
| plus the bootstrap keys above (`k3s_version`, `swap_mb`, `image_tarballs`, `node_name`) | | |

Secret-looking values must be references; plain secrets in `company.yaml` are not read.
