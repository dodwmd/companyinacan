# Contract: `digitalocean` options and the `dns` block

## `provider: {type: digitalocean}` options

Unknown keys are rejected by `can validate` and `can up`.

| Key | Default | Notes |
|---|---|---|
| `token_secret_ref` | `env:DIGITALOCEAN_TOKEN` | `env:NAME` or `file:/path` (`~/` expanded). A literal token is rejected (the value is never echoed). |
| `region` | `fra1` | slug, unverified default |
| `size` | `s-1vcpu-2gb` | slug, unverified default; $12/month per DigitalOcean's pricing page |
| `image` | `ubuntu-24-04-x64` | slug, unverified default |
| `ssh_public_key_file` | `~/.ssh/id_ed25519.pub` | uploaded, or reused when the fingerprint exists |
| `ssh_private_key_file` | ssh default | used for the bootstrap wait and status |
| `ssh_key_fingerprints` | none | list of MD5 fingerprints (`aa:bb:...`) of keys already in the account, added to the droplet; never uploaded or deleted |
| `server_name` | `can-<slug>` | DNS label |
| `operator_ip` | any | restricts SSH in the cloud firewall and the host firewall |
| `wait_seconds` | 600 | budget for droplet active plus cloud-init |
| `port` | not allowed | anything but 22 is an error |
| `k3s_version`, `node_name`, `swap_mb`, `image_tarballs` | as for `ssh` | bootstrap keys |

Cloud firewall (inbound): tcp 22 (from `operator_ip` if set, else anywhere), tcp 80, tcp 443 from
anywhere, icmp. Outbound: all. Never 6443 or 10250.

## `dns` block (top level, optional)

| Key | Default | Notes |
|---|---|---|
| `provider` | `manual` | `manual` or `cloudflare` (`provider-native` is not built) |
| `zone` | found from the domain | must be the domain or a parent of it |
| `ttl` | 300 | 1 (automatic) or 60..86400; forced to 1 when proxied |
| `proxied` | `false` | `true` breaks ACME HTTP-01 |
| `adopt_existing` | `false` | take over an unproxied same-name, same-type record; never deleted later |
| `token_secret_ref` | `env:CLOUDFLARE_API_TOKEN` | `env:NAME` or `file:PATH`; a literal fails the schema pattern |

`--dns manual|cloudflare` on `can up` overrides `dns.provider`.

## Validation rules (`can validate`)

`semantic.provider-option` (unknown key, port, bad slug/fingerprint), `semantic.secret-ref`
(literal provider token), `semantic.dns-zone`, `semantic.dns-ttl`, `semantic.dns-option` (a
Cloudflare-only key with the manual plugin), plus `schema.*` for types, unknown `dns` keys and a
literal `dns.token_secret_ref`.

## Exit codes

Unchanged: `local` and `static` still exit 3; `--dns provider-native` exits 3.
