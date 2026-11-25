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
| `digitalocean` | available (offline-tested, see below) | Creates an SSH key, a Cloud Firewall and a tagged Droplet with cloud-init on DigitalOcean. |

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
     interfaces; port 6443 is never opened to the internet (pods can still reach it and the other node
     ports, see below); enabled last so there is no lockout window;
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

**Pod-to-node reachability.** Because the pod and service CIDRs and the CNI interfaces are allowed
in, workloads (module pods included) can connect to the node's own ports, the k3s API server on 6443
and the kubelet on 10250, and Kubernetes NetworkPolicy does not prevent it on k3s (Phase 0, issue
#13, tested on k3d only). The barrier is RBAC, authentication on those ports and not mounting
ServiceAccount tokens. The isolation design is an open decision (follow-up issue).

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

## digitalocean

```yaml
provider:
  type: digitalocean
  options:
    token_secret_ref: env:DIGITALOCEAN_TOKEN    # default
    ssh_public_key_file: ~/.ssh/id_ed25519.pub
    region: fra1                # default, unverified
    size: s-1vcpu-2gb           # default, unverified: 1 vCPU, 2 GiB
    image: ubuntu-24-04-x64     # default, unverified
    operator_ip: 198.51.100.9   # optional: SSH from this address only
```

The full option list is in `specs/020-digitalocean-cloudflare/contracts/options.md`. Unknown option
keys are an error (also in `can validate`), and a literal token is rejected: use `env:NAME` or
`file:PATH`. `port` other than 22 is an error, as for Hetzner.

Create a personal access token with read and write scope in the DigitalOcean control panel
(API, Tokens) and `export DIGITALOCEAN_TOKEN=...`. `can up` creates, in order: the SSH key (reused
if the fingerprint exists), a Cloud Firewall (inbound 22, 80, 443 and icmp; SSH only from
`operator_ip` when set; all outbound; never 6443 or 10250) and the Droplet with the bootstrap as
user-data, then waits for the create action, for the Droplet to be active with a public IPv4 address,
and for the bootstrap marker. Ids are saved at once, so an interrupted run resumes.

**Ownership by tag.** The Droplet is tagged `managed-by:can` and `company:<slug>`; the firewall is
named `can-<slug>` and targets exactly those tags (DigitalOcean firewalls have no labels, so this
is the marker; it is a weaker proof than a Hetzner label). A Droplet or firewall with the expected
name but without them is never adopted (`can up` stops) and never deleted. `can down` re-reads the
tags right before deleting, deletes the Droplet and waits until it is gone, then the firewall, then
the SSH key only if `can` uploaded it. With an empty state file it finds can's resources by tag and
name. **Deleting the Droplet deletes its data.** SSH keys already in your account can be added to the
Droplet with `ssh_key_fingerprints` (MD5 form); can never uploads or deletes those.

**Size and cost** (the 2 GB target of SPEC.md section 3.2; prices as shown on DigitalOcean's
pricing page when this was written, per month, USD, not verified by running anything and subject to
change; check the page):

| Slug | RAM | Disk | Price | Use |
|---|---|---|---|---|
| `s-1vcpu-512mb-10gb` | 512 MiB | 10 GiB | $4 | too small |
| `s-1vcpu-1gb` | 1 GiB | 25 GiB | $6 | too small for the trimmed k3s plus modules |
| `s-1vcpu-2gb` | 2 GiB | 50 GiB | $12 | default |

Droplets are billed while they exist, including when powered off; `can down` is how to stop paying.
The slugs for image and region are from memory of the public lists and not checked: list the real
ones with `doctl compute size list`, `doctl compute image list-distribution` and
`doctl compute region list`, and override the defaults if they differ.

## DNS plugins

```yaml
dns:
  provider: cloudflare            # manual (default) | cloudflare
  zone: acme.example              # optional; default: the zone found for `domain`
  ttl: 300                        # 1 (automatic) or 60..86400
  proxied: false                  # default false, see below
  adopt_existing: false
  token_secret_ref: env:CLOUDFLARE_API_TOKEN     # default; or file:~/.config/can/cloudflare-token
```

`can up --dns manual|cloudflare` overrides `dns.provider`. `provider-native` (DigitalOcean's domains
API) is not built and exits 3.

### Cloudflare

Create an API token (Cloudflare dashboard, My Profile, API Tokens, Create Token, "Create Custom
Token") with **Zone / DNS / Edit** (shown as "DNS Write" in newer docs) and **Zone / Zone / Read**,
and under Zone Resources choose *Include, Specific zone*, your zone. Nothing else: no account or
other-zone permissions. Then `export CLOUDFLARE_API_TOKEN=...`. The domain must already be a
zone in your Cloudflare account (the name servers delegated to Cloudflare).

What it does: `can up` first checks the token and finds the zone (failing before a server is
created), then, after the server is up, upserts an `A` (or `AAAA`) record for the apex, `admin` and
`help`, each with the comment `managed-by=can`. Re-runs write nothing when nothing changed and update
can's own records in place when the address changed. `can up --plan` shows the planned records
without calling Cloudflare.

Safety rules:

- A record at the same name and type that does **not** carry the comment is never changed; `can up`
  stops. A `CNAME` at the same name also stops it (Cloudflare does not allow both).
- `adopt_existing: true` lets `can` take over such a record, but only if it is not proxied. It
  updates content and TTL, leaves the comment alone, and `can down` will never delete it.
- `can down --yes` removes the DNS records first (so no record points at an address that is about to
  be recycled), only those recorded in `.can/state.json` that were created by `can` and still carry
  the comment. If this fails, the teardown stops so you can repeat it; fix the token or remove the
  `dns` block to skip it.
- `proxied` defaults to `false`: the certificate challenge (ACME HTTP-01) must reach your server
  directly. With `proxied: true` the TTL is automatic and certificates will not issue until a DNS-01
  challenge exists, which is **not built** (future work).
- Cloudflare allows 1,200 API requests per five minutes; `can` waits for `Retry-After` on HTTP 429 and
  retries only requests that cannot repeat an effect (429 for any method, GET on 5xx).

DNS propagation time is not part of the five-minute budget.

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
`--yes`), `3` not implemented (`local`, `static`, the `provider-native` DNS plugin).
