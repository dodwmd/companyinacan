# `make up`, `make down`, `make status`

Thin wrappers over `can up|down|status` (extra flags go in `ARGS`). Run them in the directory that
holds `company.yaml`, or pass the directory: `make up ARGS=./my-company`.

```sh
make up ARGS=--plan     # show what would happen, change nothing
make up                 # provision, bootstrap k3s, print URL and DNS records
make status             # exit 0 only when the server is ready
make down ARGS=--plan   # show what would be removed
make down ARGS=--yes    # tear down (destructive, needs --yes)
```

`make down` runs `can down`, which depends on `provider.type` in `company.yaml` (so it needs a
`company.yaml` in the directory): `local` deletes the k3d cluster exactly like `can dev down` (no
`--yes` needed, `ARGS=--plan` prints the k3d commands); `ssh`, `hetzner` and `digitalocean` destroy what `make up`
created (plus the Cloudflare DNS records it created, if `dns.provider: cloudflare`) and require `--yes`. `make server-down` is kept as an alias.

## What `make up` does today

1. Validates `company.yaml`.
2. Checks the DNS plugin (Cloudflare: token and zone, read-only), so a bad token fails before a
   server exists.
3. Runs the provider (`ssh`, `hetzner` or `digitalocean`, see [providers.md](providers.md)):
   preflight, bootstrap, wait for k3s to be Ready.
4. Fetches the kubeconfig to `.can/kubeconfig`.
5. Prints the address and the instant URL (https on sslip.io, or the plain-HTTP fallback with the
   reason), then creates the DNS records (Cloudflare) or prints them for you to add (manual).

Not yet part of `make up`: installing can-core and the site (epic E03/E04), generating admin
credentials, the recovery kit (epic E08). Until then the server runs bare k3s, so the instant URL
does not serve a site yet.

It is safe to run `make up` again after a failure or interruption: finished steps are skipped and
resources are adopted instead of duplicated.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success (`status`: ready) |
| 1 | failure: invalid `company.yaml`, preflight failed, API or SSH error, status not ready |
| 2 | usage: unknown flag, too many arguments, `down` without `--yes` |
| 3 | not implemented: provider `local` or `static`, or the `provider-native` DNS plugin |

## Troubleshooting

- `preflight failed: ram: ...`: the server is too small; use at least 2 GB.
- `ssh ...: exit status 255`: SSH could not connect; check `host`, `user`, `key_file`, and that you
  can `ssh user@host` yourself. The first connection trusts the host key.
- `secret reference "env:HCLOUD_TOKEN" is not usable (the environment variable is empty or unset)`: export the token.
- Hetzner and DigitalOcean: look at `/var/log/can-bootstrap.log` on the server if the bootstrap wait times out.
- DigitalOcean: `a droplet named ... was not created by can`: a Droplet with that name exists without
  the `managed-by:can` and `company:<slug>` tags; rename it, delete it yourself, or set
  `provider.options.server_name`. Same for a firewall named `can-<slug>`.
- Cloudflare: `no Cloudflare zone found`: the domain is not a zone in the account the token belongs
  to, or the token lacks Zone Read for it. `already exists ... not created by can`: see
  `adopt_existing` in [providers.md](providers.md).
- `.can/state.json` says something that is no longer true (server deleted by hand): `can status`
  reports `absent`; remove `.can/` to start clean.

## Timing the five-minute budget (human-gated, epic E00)

The budget in SPEC.md section 3.4 is **not measured yet** and this feature does not claim it. To
measure it, a human with a fresh Hetzner account does the following. Do not run it from CI.

1. Create a throwaway project and API token; `export HCLOUD_TOKEN=...`. Use a new company
   directory with `provider: {type: hetzner}` and an SSH key not used elsewhere.
2. Make sure the local machine has `ssh` and a warm Go build (`make build`), so build time is not
   counted.
3. Note the wall clock, then run `time make up`. Also note the moments printed in the progress
   lines: server created, server running, `bootstrap: done`.
4. Record: total time, time to server running, time from running to bootstrap done (this covers the
   k3s install and image pulls; without `image_tarballs` containerd pulls on first use), the
   server type and location, and whether `image_tarballs` were configured.
5. On the server, record idle memory after the bootstrap settles for two minutes:
   `free -m`, and the memory line of `systemctl status k3s` (`kubectl top` is unavailable because
   metrics-server is disabled).
6. Run `make up` a second time and record how long the idempotent re-run takes.
7. `make down ARGS=--yes`; confirm in the Hetzner console that the server, firewall and key are gone.
8. Repeat three times and report median and worst case in `specs/000-*/measurements.md` (E00).

Until the whole stack is deployed (epic E03) the measurement covers only provisioning plus k3s, not
the full "live HTTPS page" milestone; label results accordingly.
