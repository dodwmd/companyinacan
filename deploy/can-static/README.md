# can-static

The tiny static file server that serves the generated company site from the `can-system` pod
(SPEC §3.1 "Site"). Standard library only; one static binary; nothing is ever written to disk.

```
can-static [-dir /srv/site] [-addr :8080] [-access-log]
can-static -healthcheck [-addr :8080]
```

Environment: `CAN_STATIC_DIR`, `CAN_STATIC_ADDR` (flags win). Source: `cmd/can-static`,
handler in `core/staticsrv` (shared with `can site serve`).

## What it does

- Serves `GET`/`HEAD` (and `OPTIONS`); everything else gets 405.
- `/healthz` returns `ok` with no dependency on the site directory, so a pod with an empty
  volume is still alive (and serves 404 until the first publish).
- Clean URLs: `/about` serves `about.html`; `/docs` redirects to `/docs/`; unknown paths get
  the site's own `404.html` with status 404.
- Safe paths: the site directory is opened with `os.Root` on every request, so `..`, encoded
  separators, NUL bytes, backslashes and symlinks pointing outside the directory cannot escape.
  Dotfiles (including the `.can-site` marker) are never served, except `/.well-known/`.
  A rebuild that replaces the whole directory is picked up without a restart.
- Precompressed content: if `x.css.br` or `x.css.gz` exists and the client accepts it, that is
  served with `Content-Encoding` and `Vary: Accept-Encoding` (`can site build` writes `.gz`).
  The `.gz`/`.br` files themselves are not directly addressable.
- Caching: `assets/<name>.<8 hex>.<ext>` is `public, max-age=31536000, immutable`; HTML is
  `no-cache` (revalidated through `ETag`/`If-None-Match`); everything else is cached for one hour.
- Security headers on every response, including errors: a strict `Content-Security-Policy`
  (`default-src 'none'`, same-origin styles and images, `form-action 'self'`, no framing),
  `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy`,
  `Permissions-Policy`, `Cross-Origin-Opener-Policy`. HSTS is left to Traefik, which terminates TLS.
- Bounded server: header, read, write and idle timeouts and a 16 KiB header limit.

## RAM budget

Constitution II: every component has a budget, and the number below is measured, not guessed
(`specs/002-site-renderer/measurements.md`).

| State | Measured RSS | 
|---|---|
| Idle, small site | about 6.7 MiB |
| Peak after 6 000 requests at 32 concurrent connections | about 13 MiB |

**Budget: `ram_mb: 16` requested, `ram_limit_mb: 32`** for the pod. The image sets
`GOMEMLIMIT=24MiB` and `GOGC=50` so the Go runtime collects before the limit is reached; the
container limit stays above the soft limit so a garbage-collection lag cannot OOM-kill it.
The SPEC §3.2 line "site ~5 MB" is a rounded target for the idle process; the measured
idle figure is 6.7 MiB, and the SPEC table should be updated by E00 once measured on the
target cluster. The stripped binary is about 6.4 MiB (image size not yet measured).

## Running in the cluster (sketch, finalised in E02/E06)

- Mount the site PVC read-only at `/srv/site`; `readOnlyRootFilesystem: true`;
  run as non-root (the image uses UID 65532); drop all capabilities.
- Liveness/readiness: `httpGet /healthz` on 8080, or `exec: ["/can-static", "-healthcheck"]`.
- can-core writes a new build with `site.Output.Write`, which replaces files atomically and
  removes stale ones, so no pod restart is needed.

## Pinning base images

`Dockerfile` contains `@sha256:REPLACE_WITH_...` placeholders on purpose: the digests move
with every upstream patch, so they are resolved at release time, not committed blind. To pin:

```sh
docker buildx imagetools inspect golang:1.27 --format '{{.Manifest.Digest}}'
docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot --format '{{.Manifest.Digest}}'
```

Pass them with `--build-arg GO_IMAGE=golang:1.27@sha256:... --build-arg RUNTIME_IMAGE=...@sha256:...`.
The release pipeline (E06/E08) must refuse to build while a `REPLACE_WITH_` placeholder is present.
