# Providers

A provider decides where the company runs. It is chosen in `company.yaml`:

```yaml
provider:
  type: ssh        # local | ssh | hetzner | digitalocean | static
  options: {}      # provider specific, see below
```

| Type | Status | What it does |
|---|---|---|
| `ssh` | available | Bring-your-own Ubuntu or Debian server reachable over SSH. |
| `hetzner` | available | Creates an SSH key, a cloud firewall and a server with cloud-init on Hetzner Cloud. |
| `local` | stub (epic E02) | k3d cluster via `can dev`. `can up` exits 3. |
| `static` | stub (epic E18) | Site-only deploy, no server. Exits 3. |
| `digitalocean` | stub (epic E12) | Exits 3. |

All options are listed in `specs/006-providers-make-up/contracts/options.md`. Secrets are never put
in `company.yaml`: use a reference, `env:NAME` or `file:/path`.

## What every server provider does

1. **Preflight** (`ssh`): over SSH, checks Ubuntu/Debian, amd64/arm64, systemd, root or
   passwordless sudo, at least 1700 MB RAM, 10 GB free disk, ports 80, 443 and 6443 free. Any
   failure stops before the host is changed. On a re-run of a bootstrapped host the ports are
   expected to be busy and are not checked.
2. **Bootstrap**: one generated, idempotent bash script (`can up --plan` shows the steps):
   - swap file on hosts under 4 GB (and `kubelet fail-swap-on=false`, which k3s then needs);
   - host firewall with `ufw`: default deny incoming; allow 22 (or only `operator_ip`), 80, 443,
     the pod CIDR 10.42.0.0/16, the service CIDR 10.43.0.0/16 and the `cni0` / `flannel.1`
     interfaces; port 6443 is never opened; enabled last so there is no lockout window;
   - k3s at a pinned version, configured through `/etc/rancher/k3s/config.yaml`: metrics-server and
     the cloud controller disabled, secrets encryption on, Traefik, CoreDNS and local-path kept;
   - optional image pre-seeding: tarballs from `image_tarballs` are downloaded in parallel with the
     k3s install, SHA-256 verified, and placed in `/var/lib/rancher/k3s/agent/images`;
   - placeholders for backups (`/etc/can/backup.d`, `/var/lib/can/dumps`; the restic and systemd
     timer arrive with epic E08);
   - completion marker `/var/lib/can/bootstrap.done` containing the k3s version.
3. **Kubeconfig**: read from the host, rewritten to `https://127.0.0.1:16443` and stored in
   `.can/kubeconfig` (mode 0600). `can` reaches the API through
   `ssh -N -L 127.0.0.1:16443:127.0.0.1:6443`; the API port is never exposed.
4. **State** in `.can/state.json` (mode 0600, no secrets, updated under a file lock) so that
   `can up` can be re-run safely and `can down` knows what to remove.

## ssh

```yaml
provider:
  type: ssh
  options:
    host: 203.0.113.7
    user: root              # or a sudo user
    key_file: ~/.ssh/id_ed25519
    operator_ip: 198.51.100.9   # optional: restrict SSH on the host firewall
```

`can down --yes` uninstalls k3s and can-core and leaves the server alone. That includes the host
firewall: ufw stays enabled with default-deny incoming and only 22/80/443 (and the pod/service CIDR)
allowed, and the swap file stays. Remove them yourself (`ufw disable`) if you want the box back as it
was.

**`operator_ip` can lock you out.** With it set, SSH is allowed only from that address on the host
firewall. If your address changes you lose SSH access (use the provider console to fix ufw). Leave it
unset unless the address is stable.

**Host keys** are trusted on first use (`StrictHostKeyChecking=accept-new`) against your
`~/.ssh/known_hosts`. If a host is rebuilt, ssh refuses with "host key verification failed"; after
confirming the rebuild is yours, run `ssh-keygen -R <host>`. For `hetzner`, `can` uses its own
`.can/known_hosts`, because cloud IPs are recycled between customers: delete that file after
rebuilding a server on the same address.

**NodePort exposure.** The bootstrap runs `ufw default allow routed`, so forwarded traffic to pods
is not filtered by ufw. A Service of type NodePort or LoadBalancer you create later on a
bring-your-own host may therefore be reachable from the internet even though ufw lists only
22/80/443. The cloud firewall of the `hetzner` provider does not have this gap. Tightening this to
specific interfaces needs measurement on a real host (epic E00) and is not done yet.

## hetzner

```yaml
provider:
  type: hetzner
  options:
    token_secret_ref: env:HCLOUD_TOKEN    # default
    ssh_public_key_file: ~/.ssh/id_ed25519.pub
    server_type: cx23
    location: fsn1
```

`~/` is expanded in `ssh_public_key_file`, `ssh_private_key_file`, `key_file` and `file:` secret
references. The SSH port is fixed to 22 for `hetzner`: setting `port` to anything else is an error,
because the cloud firewall only opens 22.

**Adoption and ownership.** Every key, firewall and server `can` creates is labelled
`managed-by=can` and `company=<slug of the name>`. If a firewall or server with the expected name
already exists *without* those labels, `can up` stops with an error and does not adopt it; `can down`
re-checks the labels right before each delete and refuses to delete anything that does not carry
them. A resource from an interrupted earlier run (labelled) is adopted. An SSH key already uploaded
with the same fingerprint is reused and never deleted by `can down`.

Create a project and a read/write API token in the Hetzner console, then
`export HCLOUD_TOKEN=...`. `can up` creates, in order, the SSH key (reused if the fingerprint is
already uploaded), a firewall (22, 80, 443, icmp) and the server with the bootstrap as cloud-init
user-data, then waits until the completion marker appears. Every id is saved immediately, so an
interrupted run resumes without creating duplicates. `can down --yes` deletes the server, the
firewall, and the SSH key only if `can` uploaded it. **Deleting the server deletes its data.**

The defaults for `server_type`, `image` and `location` have not been checked against the live API
yet (epic E00). Override them if Hetzner has renamed a type.

## Option values

`port` and the other numeric options must be whole numbers: a quoted `"22"` or `22.5` is an error,
not silently replaced by the default. Only a leading `~/` is expanded in file paths; `~user/...`,
`$HOME` and `${HOME}` are not.

## Resuming and changed options

A resumed `can up` reuses the firewall it already created and does **not** reconcile it: changing
`operator_ip` after the first run does not change the cloud firewall rules (the host firewall is
re-applied by the bootstrap only if it re-runs). To apply a new `operator_ip` on Hetzner, run
`can down --yes` and `can up` again, or edit the firewall in the Hetzner console.

If `can down` finds an empty state (for example a crash between creating a resource and saving
its id), it looks for the server, firewall and SSH key by name and labels and removes only those
labelled `managed-by=can, company=<slug>`; a same-named resource without the labels is reported and
left alone. An SSH key adopted by fingerprint is treated as can's only if it carries those labels.
`can down` also deletes `.can/known_hosts` and `.can/kubeconfig`.

## Bootstrap caveats

- The k3s installer (`get.k3s.io`) is downloaded over TLS but **not hash-verified**; the k3s
  version is pinned, the installer script is not.
- Image tarballs are checksum-verified and placed in `agent/images` in parallel with the install.
  That k3s imports them after it starts (rather than pulling) is **unverified** until E00.
- Re-running `can up` with the same pinned version restarts k3s when `config.yaml` differs from the
  hash recorded in `/var/lib/can/k3s-config.applied`. The hash is written only after k3s is Ready,
  so a run that crashed before restarting is retried next time. A version change re-runs the
  installer. Temp files from interrupted runs are removed.
- A crash between creating a cloud resource and saving its id in state is recovered by name and
  labels on the next `can up`. A crash during the bootstrap script leaves a partial host; re-running
  converges because every step is guarded.
- `can up` and `can down` take a per-company lock, so a second run fails fast ("already running").

## DNS

The DNS plugin is separate from the provider. `manual` (the default, `--dns manual`) prints the
records to add at your DNS host: the apex, `admin` and `help` (A, or AAAA for an IPv6 address, TTL
300). Cloudflare and provider-native plugins are planned (epic E12) and exit 3 today.

## Instant URL (sslip.io)

Before DNS exists the site is meant to be served on `https://<ip-dashed>.sslip.io` (203.0.113.7
becomes `203-0-113-7`), with `admin.` and `help.` prefixes and one certificate per name.
`can up` decides whether that is safe now:

- sslip.io must resolve the name to your server's IP;
- fewer than 4 successful certificates for these names in the last 7 days (Let's Encrypt allows 5);
- fewer than 3 failed issuances in the last hour (limit 5).

Otherwise it prints `http://<ip>` instead: plain HTTP, public site only, and the admin UI reachable
only through the SSH tunnel until a domain and TLS are set. The certificates themselves are issued
by Traefik once can-core deploys the site (epic E03), which also records each attempt in state.

## Output and secrets

All output, errors and JSON pass through a redactor. Tokens read through a secret reference are
removed wherever they would appear, even when an API echoes them back; bearer tokens, `token=`,
`password=` assignments and private key blocks printed by a remote command are masked too. State
and kubeconfig files are mode 0600 and the state holds no secrets.

## Exit codes

`0` ok, `1` failure (including `status` when not ready), `2` usage (bad flag, `down` without
`--yes`), `3` not implemented (`local`, `static`, `digitalocean`, Cloudflare DNS).
