# Research: E12c DigitalOcean and Cloudflare

Sources were fetched on 2026-11-25. The DigitalOcean API reference pages are single-page apps that
the fetch tool could not expand, so most DigitalOcean request shapes below come from the author's
knowledge of the public API and are **unverified**; each is marked.

## Sources actually read

| URL | What it gave |
|---|---|
| https://www.digitalocean.com/pricing/droplets | Basic plans with 1 vCPU: 512 MiB / 10 GiB $4 (`s-1vcpu-512mb-10gb`), 1 GiB / 25 GiB $6 (`s-1vcpu-1gb`), 2 GiB / 50 GiB $12 (`s-1vcpu-2gb`) per month, as extracted by the fetch tool. Prices change; re-check. |
| https://developers.cloudflare.com/api/resources/dns/subresources/records/methods/create/ | `POST /zones/{zone_id}/dns_records`; fields `type`, `name`, `content`, `ttl` (1 = automatic, 60-86400), `proxied`, `comment`, `tags`; A/AAAA cannot share a name with a CNAME; permission "DNS Write". Only the first 100 000 characters of the page were read. |
| https://developers.cloudflare.com/fundamentals/api/reference/limits/ | 1,200 requests per 5 minutes per user; HTTP 429 blocks calls for five minutes; `retry-after` header in seconds. |
| https://docs.digitalocean.com/reference/api/reference/ (index only) | Lists Droplets, Firewalls, SSH Keys, Actions, Tags endpoint groups. No shapes. |

## D1. Defaults

**Decision**: size `s-1vcpu-2gb` (2 GiB, $12/month): the smallest plan that meets the 2 GB target
(the 1 GiB plan cannot hold the ~510-563 MiB k3s node plus workloads; SPEC.md section 3.2 targets
2 GB). Image `ubuntu-24-04-x64`, region `fra1`.
**Unverified**: all three slugs against `GET /v2/sizes`, `/v2/images`, `/v2/regions`; the price table;
that 2 GiB is enough in practice (E00 measures on a real box). Operators can override each.

## D2. Ownership without labels

Hetzner objects carry labels; DigitalOcean has **tags** on droplets and no labels on firewalls or
SSH keys.

- **Droplet**: tags `managed-by:can` and `company:<slug>`. Tag names may contain letters, numbers,
  colons, dashes and underscores (unverified recollection of the rule); the slug is `[a-z0-9-]`.
  A droplet is can's only if both tags are present.
- **Firewall**: a DigitalOcean firewall's `tags` field is the set of droplet tags it *applies to*.
  can creates it with `tags: [managed-by:can, company:<slug>]` and treats a firewall as its own only
  if its name is `can-<slug>` **and** it targets exactly those tags. This is a weaker proof than a
  label (someone could create a same-named firewall targeting the same tags on purpose), and is
  documented as such. Deleting it affects only droplets with can's tags.
- **SSH key**: no labels. A key found by fingerprint is reused. It is "owned" (deleted by `can down`)
  only if can uploaded it in this run (recorded in state) or, when state is lost, if its name is
  `can-<slug>`. A key with another name is never deleted.

## D3. Rate limits and retries

DigitalOcean documents 5,000 requests/hour and 250/minute with `RateLimit-*` headers (unverified
here); `429` carries `RateLimit-Reset` (unix seconds), reused via the Hetzner `resetWait`. Retries:
`429` for any method (the request was refused, so repeating cannot duplicate), `GET` on 5xx and
network errors; **never** `POST`/`DELETE` on 5xx or a network error (the effect may have happened).
Cloudflare: `Retry-After` seconds, same rules.

## D4. Firewall outbound

A DigitalOcean firewall with no outbound rules is believed to block all outbound traffic, which
would stop image pulls, apt, ACME and the k3s installer. can therefore adds allow-all outbound rules
(`tcp`/`udp` ports `0` meaning all, plus `icmp`). **Unverified**: the exact all-ports spelling
(`"0"` vs `"all"`); first real run decides.

## D5. Droplet creation and polling

`POST /v2/droplets` returns the droplet (status `new`) and `links.actions[]`. can saves the droplet
id immediately, polls `GET /v2/actions/{id}` until `completed` (error on `errored`), then polls the
droplet until `status: active` with a public IPv4 in `networks.v4[]`. Poll counts, not wall-clock,
bound every loop (`wait_seconds / interval`) so cancelled contexts and tests cannot hang. SSH key
lookup uses `GET /v2/account/keys/{fingerprint}` (MD5 form), which avoids a paginated list.
**Unverified**: all response shapes.

## D6. Pagination

Lists are read with `per_page=200&page=N` until `links.pages.next` is absent. The `next` URL itself
is never followed, so the bearer token can only be sent to the configured base URL.

## D7. Cloudflare ownership

Records carry `comment: "managed-by=can"` (the comment field is available on all plans; `tags` are
not used). Ownership is decided by that comment. `can down` re-reads each record by id and deletes it
only if the comment is still there. A record changed by someone else (comment removed) is left alone.
Deletion works from **state only**: unlike droplets there is no discovery-by-comment on an empty
state, because deleting every comment-matching record in a zone is too broad.

## D8. Adopt flag

`dns.adopt_existing: true` lets can take over a same-name, same-type record without can's comment,
but only if it is not proxied. can updates content and TTL, leaves the comment alone, records it in
state as `adopted`, and never deletes it. A `CNAME` at the same name always blocks (Cloudflare does
not allow it next to A/AAAA).

## D9. Proxy and ACME

`proxied` defaults to false: Traefik's ACME HTTP-01 challenge must reach the origin. With
`proxied: true` TTL is forced to 1 (automatic, a Cloudflare requirement) and the operator must use a
different challenge. **DNS-01 is future work** (it needs a second, narrower token flow); documented
as the way to use the proxy.

## D10. Token scope

Least privilege: a Cloudflare API token with `Zone / DNS / Edit` and `Zone / Zone / Read`, limited to
the one zone under "Zone Resources". Cloudflare's current docs name the DNS permission "DNS Write";
both names appear in the dashboard history, so docs mention both. **Unverified** on a real account.

## D11. Preflight

`can up` calls `DNSChecker.Preflight` (token resolves, zone found; read-only) before creating any
server so a typo cannot cost a droplet-hour.

## D12. A secret echoed by `ResolveSecret`

`ResolveSecret` used to echo an invalid reference in its error. A user pasting a literal token
there would have had it printed. The message no longer includes the value, and the wording was
changed so the redactor's `token:`/`secret:` pattern does not mangle the variable name.
