# Feature Specification: DigitalOcean Provider and Cloudflare DNS (Epic E12c)

**Feature Branch**: `epic/E12c-digitalocean-cloudflare`

**Created**: 2026-11-25

**Status**: Implemented offline against `httptest` fakes. Nothing here has run against a real
DigitalOcean or Cloudflare account (see [measurements.md](measurements.md)).

**Input**: Epic E12 in `docs/epics/README.md` ("DigitalOcean provider; Cloudflare DNS") and SPEC.md
sections 3.4 and 3.5: "DNS is a separate plugin (`cloudflare`, `provider-native`, `manual`)";
"records created automatically when a DNS provider token is configured". Builds on
[E06](../006-providers-make-up/spec.md): same `Provider` interface, same state file, same safety
rules.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - `make up` on DigitalOcean (Priority: P1)

An operator with a DigitalOcean API token writes `provider: {type: digitalocean}` and runs
`make up`. A tagged Droplet with the cloud-init bootstrap, a Cloud Firewall (22, 80, 443; never 6443
or 10250) and an SSH key exist afterwards; k3s is installed; the kubeconfig is fetched.

**Independent Test**: `core/provider/digitalocean_test.go` runs the whole flow against a stateful
`httptest` fake (paginated lists, action polling, error bodies).

**Acceptance Scenarios**:

1. **Given** an empty account, **When** `can up` runs, **Then** the key, firewall and droplet are
   created in that order, the droplet carries the tags `managed-by:can` and `company:<slug>`, ids are
   saved to `.can/state.json` as soon as they exist, and the bootstrap is awaited.
2. **Given** a previous run that crashed after creating resources but before saving state, **When**
   `can up` runs again, **Then** the tagged resources are adopted and nothing is created twice.
3. **Given** a droplet or firewall with the expected name but without can's tags, **When** `can up`
   runs, **Then** it stops with an error and changes nothing; `can down` never deletes it either.
4. **Given** provider options with an unknown key, `port` other than 22, or a literal token, **Then**
   `can validate` and `can up` reject them without calling any API.

### User Story 2 - `can down` removes exactly what can made (Priority: P1)

**Acceptance Scenarios**:

1. **Given** a finished `make up`, **When** `can down --yes` runs, **Then** the droplet is deleted
   and confirmed gone (404), then the firewall, then the SSH key only if can uploaded it.
2. **Given** the droplet was re-tagged by someone, **When** `can down --yes` runs, **Then** it
   refuses to delete it (tags are re-checked right before the delete).
3. **Given** an empty state file, **When** `can down --yes` runs, **Then** resources are found by
   tag and name and removed; same-named resources without the tags are reported and left alone.
4. Running `can down` twice is a no-op the second time.

### User Story 3 - DNS records created automatically (Priority: P2)

With `dns: {provider: cloudflare}` and a scoped token, `can up` creates `A`/`AAAA` records for the
apex, `admin` and `help` in the right zone, unproxied, and `can down` deletes only the records it
created.

**Acceptance Scenarios**:

1. **Given** a token that cannot see the zone, **When** `can up` runs, **Then** it fails before a
   server is created.
2. **Given** an existing record at the same name and type that can did not create, **Then** it is
   not touched, unless `dns.adopt_existing: true` and the record is not proxied; an adopted record
   is never deleted by `can down`.
3. **Given** the server address changed, **When** `can up` runs again, **Then** can's own records
   are updated in place; unchanged records cause no write.
4. `can up --plan` prints what would be written and calls no API.

## Requirements

- **FR-001** `digitalocean` implements `Provider` with the E06 safety rules: ownership by tag before
  adopt or delete, re-check before delete, `~/` expansion, port rejection, wait-until-gone, per
  company operation lock, `known_hosts`, redaction, rate-limit handling, retries only for safe calls.
- **FR-002** The DO API shapes are fixtures marked unverified; defaults (`s-1vcpu-2gb`,
  `ubuntu-24-04-x64`, `fra1`) are documented as unverified.
- **FR-003** A DNS plugin may implement `DNSRemover`, `DNSPlanner`, `DNSChecker`; `manual` implements
  none and is unchanged in behaviour.
- **FR-004** `company.yaml` gains a `dns` block; `can validate` rejects unknown keys, literal tokens,
  a zone that does not contain the domain, and DNS options on the `manual` plugin.
- **FR-005** No token is ever logged, printed, put in state, or echoed in an error (the API echoing
  one back is redacted).

## Out of scope

`provider-native` DNS (DigitalOcean domains API; still exits 3), reserved IPs (the droplet's own
public address is used), DNS-01 challenges (documented as the alternative for proxied records), IPv6
only droplets, firewall reconciliation on re-run (same limitation as Hetzner).
