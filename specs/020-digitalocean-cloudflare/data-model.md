# Data Model: E12c

## State additions (`.can/state.json`, version stays 1)

| Field | Type | Notes |
|---|---|---|
| `firewall_uuid` | string | DigitalOcean firewall id (UUID). Hetzner keeps numeric `firewall_id`. |
| `dns_records` | list | `{plugin, zone, id, type, name, adopted?}` records a DNS plugin created or adopted. `adopted` records are never deleted. |
| `server.id` | int | droplet id |
| `ssh_key_id`, `ssh_key_owned` | int, bool | as for Hetzner |
| `steps["digitalocean.bootstrap"]` | RFC 3339 | bootstrap completed |
| `provider` | string | `digitalocean` |

Older readers ignore the new fields; no migration. Never stored: tokens.

## Ownership markers

| Object | Marker | Adopt rule | Delete rule |
|---|---|---|---|
| Droplet | tags `managed-by:can`, `company:<slug>` | same name and both tags: adopt; same name without: error | tags re-read right before DELETE |
| Firewall | name `can-<slug>` and `tags` = those two tags | same name and tags: adopt; same name otherwise: error | re-read, same check |
| SSH key | none; reuse by fingerprint | `owned` only if uploaded by can (state) or named `can-<slug>` when state is lost | only if owned |
| Cloudflare record | `comment: managed-by=can` | foreign: refuse, or adopt with `dns.adopt_existing` if unproxied | only records in state, not adopted, comment still present |

## Interfaces

```go
type DNS interface{ Name(); Apply(ctx, domain, recs, w) error }
type DNSRemover interface{ Remove(ctx, domain, w) error }     // can down
type DNSPlanner interface{ PlanRecords(domain, recs) []string } // can up --plan, offline
type DNSChecker interface{ Preflight(ctx, domain) error }       // before any server is created
func NewDNSWith(name, DNSConfig, Deps, *Store) (DNS, error)
```

## `company.yaml` additions

See [contracts/options.md](contracts/options.md).
