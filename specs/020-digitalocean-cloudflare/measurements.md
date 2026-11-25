# Verified offline, and what is not

## Verified offline (fakes only)

- Every request the provider and plugin make has the method, path and body asserted against a
  hand-written fake; ordering of create and delete calls; retry rules; pagination; redaction; state
  contents; CLI flows and exit codes.
- Schema and semantic validation through the contract fixtures.

A fake that agrees with the code proves the code is self-consistent, not that DigitalOcean or
Cloudflare behave like the fake.

## Not verified (needs a real account) `[V]`

- V001 DigitalOcean: `POST /v2/droplets` body and response (`links.actions`, `networks.v4[].type ==
  "public"`), `user_data` accepted, tag names with a colon accepted, droplet tags applied at create.
- V002 `GET /v2/account/keys/{fingerprint}` with the MD5 form for an ed25519 key; 404 body shape;
  behaviour when the same key already exists under another name (422?).
- V003 Firewall: body shape (`inbound_rules[].sources.addresses`, `tags`), that it attaches to
  droplets by tag at create time, that an empty outbound list really blocks outbound, the all-ports
  spelling `"0"`, that `icmp` takes no ports, that deleting a firewall while droplets are being
  deleted works, and what the real effect of the rules is (22/80/443 reachable, 6443 not).
- V004 Defaults: `s-1vcpu-2gb`, `ubuntu-24-04-x64`, `fra1` exist today; price; whether 2 GiB is
  enough for the trimmed k3s (E00).
- V005 Cloud-init timing on DigitalOcean (droplet active vs bootstrap done) and the default
  `wait_seconds` of 600.
- V006 Rate-limit headers (`RateLimit-Reset` as unix seconds) and the 429 body.
- V007 Cloudflare: token permissions actually required (Zone Read + DNS Edit/Write on one zone),
  `GET /zones?name=` for a subdomain miss, `dns_records?name=` filter and `result_info.total_pages`,
  `comment` round-trip on create/update, `proxied: false` with `ttl: 300`, the 5-minute/1200-request
  limit and `Retry-After` on 429.
- V008 DNS propagation and ACME HTTP-01 on the created records (not part of the five-minute budget).
- V009 The five-minute timing on DigitalOcean (E00).
