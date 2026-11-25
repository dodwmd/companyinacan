# Research: Site Renderer and Static Server (E04)

## R1. Template engine
- **Decision**: stdlib `html/template`, templates embedded with `go:embed`, one file per concern
  (`layout.gohtml`, `sections.gohtml`).
- **Rationale**: contextual auto-escaping is the XSS defence (Constitution V); no new dependency; SPEC §4.1 says "Go templates in a renderer library".
- **Alternatives**: `text/template` plus manual escaping (rejected: one missed call is an XSS); third-party engines (no benefit).

## R2. No new dependencies
- **Decision**: the epic adds no module dependency. `os.Root` (Go 1.24+) gives symlink-safe
  confinement; `compress/gzip` gives precompression; `net/http` serves files.
- **Rationale**: Constitution II/VII; the licence check and `THIRD_PARTY_LICENSES` stay unchanged.
- **Alternatives**: `andybalholm/brotli` to precompress `.br` (rejected for now: `.br` is served if
  present, but producing it is left to a later epic or an external step; gzip covers the cheap case).

## R3. CSS delivery and CSP
- **Decision**: one external stylesheet `assets/site.<sha256[:8]>.css`, no inline `<style>` or `style=`
  attributes. Brand colours are injected with `text/template` into the stylesheet after being
  validated as `#rrggbb` (schema and renderer), and the text colour on accent backgrounds is chosen
  by WCAG relative luminance.
- **Rationale**: allows `style-src 'self'` with no `unsafe-inline`; the content hash makes the file
  immutably cacheable and busts caches on change; validation closes CSS injection.
- **Contrast**: the accent is used as a fill (buttons, skip link) with black or white text, which always reaches at least 4.5:1. Links and the secondary button use a separate `--link` colour: the accent moved towards black (light scheme) or white (dark scheme) in 5% steps until it reaches 4.5:1 against the page background (`core/site/colour.go`), so a pale accent cannot produce unreadable links. The CTA band uses the second brand colour with a black/white text colour chosen the same way. The accessibility claims are limited to these checks and the structural ones; no full WCAG audit has been done.
- **Alternatives**: inlining the CSS (needs `unsafe-inline` or a nonce, which a static host cannot provide).

## R4. JSON-LD
- **Decision**: build an `Organization` struct, marshal with `encoding/json` (which escapes `<`, `>`
  and `&`), and pass it as `template.JS` into the `application/ld+json` script block.
- **Rationale**: `html/template` treats that script type as JavaScript; a `template.JS` value is
  emitted verbatim, and `json.Marshal` output cannot terminate the element. Tested with hostile names.
- **Alternatives**: letting `html/template` JS-escape a string (produces a quoted string, not an object).

## R5. Link safety
- **Decision**: allow-list: `http(s)://host`, `mailto:`, `/path` (not `//`, not `/\`), `#fragment`;
  no whitespace or control characters; checked by a schema pattern and again in Go (`safeHref`).
- **Rationale**: `html/template` already neutralises `javascript:` in URL attributes, but rejecting
  at validation gives the author an error instead of a silently dead link, and protects any future
  consumer that bypasses the template. The schema patterns (host required, no `//`, no `/\`) and
  `og_image`/`same_as`/`template` rules are kept equivalent to the renderer by `TestSchemaAndRendererAgree`.

## R6. Writing output safely and atomically
- **Decision**: `Output.Write` opens the directory as an `os.Root`; each file is written to
  `<name>.tmp` and renamed over the target; assets first, `index.html` last; the `.can-site` marker
  records every generated path, and afterwards only paths the previous marker listed that are no
  longer generated are removed. Foreign files are never deleted (even in a marked directory, even
  with `--force`), a hostile or legacy marker cannot widen that, and a directory containing
  `company.yaml` is always refused. A non-empty directory without a marker needs `--force`.
- **Rationale**: the static server sees either the old or the new file, never a torn one; a mistyped
  `--out` cannot delete user data; symlinks planted in a PVC cannot redirect writes (tested).
- **Alternatives**: building into a sibling directory and swapping with a rename (cleaner, but a
  PVC mount point cannot be renamed; per-file atomicity is enough for a static site).

## R7. Static server design
- **Decision**: `core/staticsrv` handler; the root directory is opened per request via `os.OpenRoot`
  (one `openat`, negligible next to the file read); path validation rejects `..`, NUL, backslash,
  invalid UTF-8, dotfiles (except `.well-known`) and direct `.gz`/`.br` access before touching the
  filesystem; `http.ServeContent` provides ranges and conditional requests; ETag is weak
  size+mtime, distinct per encoding.
- **Rationale**: per-request opening means a rebuild that replaces the whole directory is picked
  up, and a symlink escape is refused by the kernel-level `openat2`/`os.Root` checks rather than by
  string handling alone. Fuzzing found no escapes (FuzzServePath, 760 k executions locally).
- **Alternatives**: `http.FileServer(http.Dir)` (follows symlinks out of the root, serves dotfiles, directory listings).

## R8. Security headers
- **Decision**: `default-src 'none'; img-src 'self' data:; style-src 'self'; font-src 'self';
  connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'`, plus nosniff,
  `X-Frame-Options: DENY`, referrer policy, permissions policy, COOP. HSTS is left to Traefik
  (it terminates TLS; setting HSTS on plain HTTP is meaningless).
- **Rationale**: `connect-src`/`form-action 'self'` already allow E05's same-origin `/_forms`.
  SVG logos are loaded through `<img>`, where scripts are inert, and a direct visit is covered by
  `default-src 'none'`.
- **Alternatives**: a Traefik middleware for headers (rejected: the site must be safe on its own,
  including under `can site serve` and on non-Traefik hosts).

## R9. Schema additions
- **Decision**: add `description`, `lang`, `og_image`, `noindex`, `same_as`, `hero`, `features`,
  `pricing`, `faq`, `cta`, `footer` under `site:` with length limits and `additionalProperties:
  false`, plus shared `$defs` `href` and `button`. Existing documents remain valid.
- **Follow-up**: SPEC §3.3's `site:` example and `specs/001-project-foundation/data-model.md` should
  list the new fields; this is a documentation change for the maintainer to approve with the PR.

## R10. Preview server
- **Decision**: `can site serve` builds into a temp directory, serves it with the production handler,
  and re-renders when the mtime of `company.yaml` changes (checked per request, so no watcher goroutine
  and no inotify dependency). A failed rebuild keeps the last good output.
- Both `company.yaml` and the logo file are watched (modification time and size). A non-loopback `--addr` prints a warning, since the preview has no authentication.

## R11. Compression
- **Decision**: `can site build` writes `.gz` siblings (deterministic: zero header, best compression)
  for html, css, xml, txt, svg and json; Traefik does not need to compress.
- **Rationale**: cheap, saves CPU on the small server, and the files are part of the atomic publish.
