# Feature Specification: Site Renderer and Static Server (Epic E04)

**Feature Branch**: `epic/E04-site-renderer` (spec dir `002-site-renderer`)

**Created**: 2026-11-25

**Status**: Implemented

**Input**: Epic E04 from `docs/epics/README.md`: "A company website from `company.yaml`, published in under a second." Renderer library shared by the CLI and can-core; section-based templates (hero, features, pricing, FAQ, CTA); SEO basics; a tiny static server for the site pod. The `static` provider deploy path is a stretch and is out of scope here.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Turn `company.yaml` into a website (Priority: P1)

A founder describes the company's landing page in the `site:` section of `company.yaml` (tagline, hero, features, pricing, FAQ, call to action, footer, brand colours, logo) and runs `can site build`. They get a complete static site in a directory, in well under a second, that they can host anywhere.

**Why this priority**: This is the epic's goal and the first thing a visitor of a new company sees (SPEC §1, "five-minute launch").

**Independent Test**: Build the shipped example and the full fixture; compare against golden files; open the result in any browser.

**Acceptance Scenarios**:

1. **Given** a valid `company.yaml` with all sections, **When** `can site build <dir> --out <out>` runs, **Then** `index.html`, `404.html`, `robots.txt`, `sitemap.xml`, a content-hashed stylesheet and the logo are written, and the run takes well under one second.
2. **Given** a `company.yaml` with only name, domain and tagline, **When** the site is built, **Then** a valid page is produced and sections without content are omitted entirely (no empty headings).
3. **Given** the same input twice, **When** built, **Then** the output is byte-identical (no timestamps, no randomness).
4. **Given** invalid site content (unsafe link, unknown field, wrong type), **When** built, **Then** every problem is reported with file, line and field path, exit status is 1, and nothing is written.
5. **Given** a previous build in the output directory, **When** a new build runs, **Then** changed files are replaced atomically and stale files (including precompressed siblings) are removed, but only in a directory this tool created.

---

### User Story 2 - A page that is safe, accessible and findable (Priority: P1)

Visitors on any device, in light or dark mode, with a keyboard or screen reader, get a fast page that works without JavaScript. Search engines and social previews get correct metadata.

**Why this priority**: SPEC §4.1 requires "built-in SEO basics: sitemap, OG tags, robots, structured data, fast by default"; Constitution V requires secure defaults.

**Independent Test**: Golden file and assertion tests on the rendered HTML; hostile-content tests.

**Acceptance Scenarios**:

1. **Given** any content, **When** rendered, **Then** the page has `lang`, a single `h1`, landmark regions, a skip link, visible focus styles, native `details` for the FAQ, responsive layout, light and dark colour schemes and reduced-motion support, and ships no JavaScript.
2. **Given** content containing HTML or script markup in any field, **When** rendered, **Then** it is HTML-escaped everywhere, including the JSON-LD block, and the page contains no inline event handlers or `style` attributes (a strict CSP stays possible).
3. **Given** link targets, **When** they are not `http(s)`, `mailto:`, site-relative or a fragment (for example `javascript:` or `//host`), **Then** validation rejects them.
4. **Given** the site is indexable, **When** rendered, **Then** the title, meta description, canonical URL, Open Graph and Twitter tags, a JSON-LD `Organization`, `sitemap.xml` and `robots.txt` are present; **given** `noindex: true`, **then** robots meta, a disallow-all `robots.txt` and no sitemap are produced.
5. **Given** brand colours, **When** rendered, **Then** text on accent backgrounds is black or white, whichever contrasts better.

---

### User Story 3 - Serve the site from a tiny, hardened server (Priority: P1)

The site pod in `can-system` runs `can-static`, which serves the PVC read-only, answers health probes, and stays within a small, documented RAM budget.

**Why this priority**: SPEC §3.1 and §3.2: the site is "~5 MB static server" and a footprint claim must be measured.

**Independent Test**: Handler tests, a path-traversal table, a fuzz test, and a measured RSS under load.

**Acceptance Scenarios**:

1. **Given** a built site directory, **When** requested, **Then** pages, clean URLs, directory redirects, the custom 404 page, `HEAD`, conditional requests and ranges behave correctly, and other methods get 405.
2. **Given** hostile paths (`..`, encoded separators, NUL, backslash, symlinks leaving the directory, dotfiles), **When** requested, **Then** nothing outside the site, and no dotfile, is ever served.
3. **Given** precompressed `.gz`/`.br` files, **When** the client accepts them, **Then** they are served with correct `Content-Encoding`, `Vary` and ETag; the compressed files are not directly addressable.
4. **Given** any response, **Then** it carries the security headers and an appropriate `Cache-Control` (immutable for hashed assets, revalidate for HTML).
5. **Given** an empty or missing site directory, **When** `/healthz` is probed, **Then** it returns 200.
6. **Given** a read-only root filesystem, **Then** the server works (it never writes).

---

### User Story 4 - Preview locally (Priority: P2)

`can site serve` builds the site to a temporary directory, serves it on loopback, and re-renders when `company.yaml` changes. A broken edit keeps the last good build and prints the problems.

**Acceptance Scenarios**:

1. **Given** a valid company directory, **When** `can site serve` runs, **Then** the site is served on `127.0.0.1:8080` with the same handler and headers as production.
2. **Given** an edit to `company.yaml`, **When** the next request arrives, **Then** the new content is served; **given** an invalid edit, **then** the last good build is still served.

## Edge Cases

- `--out` points at a non-empty directory the tool did not create: refuse unless `--force`.
- The output directory contains symlinks: writes never follow them out of the directory.
- Logo path is absolute, traverses upward, has an unsupported extension, is missing, or is over 1 MiB: rejected with the field path.
- Hostile colour strings (CSS injection): rejected by the schema and again by the renderer.
- The site directory is replaced while the server runs: the next request sees the new directory.
- Only one page exists in this epic; multi-page content (legal pages, `jobs`) is a later epic and the renderer's `Output` map already supports more files.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The `site:` section of `company.v1.schema.json` MUST be extended additively (existing files stay valid) with `description`, `lang`, `og_image`, `noindex`, `same_as`, `hero`, `features`, `pricing`, `faq`, `cta` and `footer`; unknown fields stay rejected.
- **FR-002**: `core/site` MUST render a `config.Company` to an in-memory file set deterministically, reporting all configuration problems together.
- **FR-003**: Output MUST include `index.html`, `404.html`, `robots.txt`, `sitemap.xml` (unless `noindex`), one content-hashed stylesheet, and the logo when configured.
- **FR-004**: All user content MUST pass through `html/template` contextual escaping; JSON-LD MUST be produced by `encoding/json` (HTML-escaped).
- **FR-005**: Links MUST be restricted to `http(s)`, `mailto:`, site-relative paths and fragments, in the schema and in the renderer.
- **FR-006**: The page MUST NOT require JavaScript and MUST NOT use inline styles or event handlers.
- **FR-007**: The writer MUST replace files atomically, remove stale files only in marked directories, never follow symlinks, and optionally write deterministic gzip siblings.
- **FR-008**: `can site build [dir] --out <dir>` and `can site serve [dir]` MUST exist, support `--output json` for build, and use the existing exit codes.
- **FR-009**: `cmd/can-static` MUST be stdlib-only (plus the repository's own `core/staticsrv`), never write to disk, and provide `/healthz` and a `-healthcheck` flag for distroless exec probes.
- **FR-010**: The server MUST resolve paths through `os.Root`, refuse dotfiles and traversal, serve precompressed files, set cache and security headers, and bound its timeouts and header size.
- **FR-011**: A Dockerfile fragment under `deploy/can-static/` MUST build a distroless, non-root image with base images pinned by digest (placeholders documented).
- **FR-012**: The can-static RAM budget MUST be stated from a measurement.

### Key Entities

See [data-model.md](data-model.md): `Site` (config), `Output`, the page view model, and the server's path resolution rules.

## Success Criteria *(mandatory)*

- **SC-001**: Building the example site takes under 1 second (measured: about 20 ms).
- **SC-002**: can-static idle RSS and peak RSS under load are measured and documented; the pod budget has headroom over the peak.
- **SC-003**: Golden-file tests cover the full page; table and fuzz tests cover traversal; escaping tests cover hostile content in every field.
- **SC-004**: `make test` and CI stay green with no new third-party dependency.

## Assumptions

- A single landing page is enough for this epic; the template name `classic` is the only built-in template and `site.template` selects it.
- Section headings (Features, Pricing, FAQ) are English; localising them follows with the `lang` work in a later epic.
- TLS, HSTS and compression of dynamic responses are handled by Traefik; can-static serves plain HTTP inside the cluster.
- Writing the output to the PVC is can-core's job (E03); this epic delivers `site.Output.Write`, which it will call.
