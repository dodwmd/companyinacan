# Data Model: Site Renderer and Static Server (E04)

Extends `company.v1` (E01 data-model). Everything is optional; defaults are noted. Unknown fields
are rejected. Limits are enforced by the schema.

## `site` (in `company.yaml`)

| Field | Type | Rules / default |
|---|---|---|
| `tagline` | string | existing; max 200; used in the title and as the description fallback |
| `template` | string | existing; only `classic` exists (default) |
| `brand.colours` | list of `#rrggbb` | existing; first is the accent, second the CTA band (default accent `#2457c5`) |
| `brand.logo` | string | existing; relative path inside the company directory; `.svg .png .jpg .jpeg .webp .gif`; max 1 MiB; copied to `assets/logo.<ext>` |
| `description` | string | max 300; meta description; default tagline, then company name |
| `lang` | string | BCP 47 tag; default `en` |
| `og_image` | string | `https://` URL or site-relative path (made absolute with the base URL) |
| `noindex` | bool | default false; adds robots meta, disallow-all `robots.txt`, no sitemap |
| `same_as` | list of `https://` URLs | max 10; JSON-LD `sameAs` |
| `hero` | object | `headline` (required, max 120), `subheadline` (max 300), `cta`, `secondary_cta` (buttons). Absent: name and tagline are used |
| `features` | list (max 12) | `title` (required, max 80), `description` (max 400) |
| `pricing` | object | `title`, `subtitle`, `plans` (1 to 4): `name`, `price` (required), `period`, `description`, `features` (list of strings), `cta` (button), `highlighted` (bool) |
| `faq` | list (max 20) | `question`, `answer` (both required; answer max 1000); rendered as `details`/`summary` |
| `cta` | object | `headline`, `button` (required), `text`; the closing band, anchor `#contact` |
| `footer` | object | `text` (max 200), `links` (max 12 of `label`, `href`) |

`button` = `{label (1-40 chars), href}`; `href` matches `^(https?://\S+|mailto:\S+|/\S*|#\S*)$`
(schema) and is re-checked by the renderer (also rejecting `//` and `/\`).

A section with no content is not rendered, and its navigation entry is not emitted.

## Output (`site.Output`)

A map of slash-separated relative path to bytes; `Paths()` is sorted.

| Path | When |
|---|---|
| `index.html` | always |
| `404.html` | always (always `noindex`) |
| `robots.txt` | always |
| `sitemap.xml` | unless `noindex` |
| `assets/site.<8 hex>.css` | always; the hash is of the final CSS bytes |
| `assets/logo.<ext>` | when `brand.logo` is set |

On disk, `Write` adds `.can-site` (marker) and, with compression, `<file>.gz` for html/css/xml/txt/svg/json.

## Page view model (internal)

Title (`Name - Tagline`), description, canonical (`<base>/`), Open Graph image, navigation entries
(only for present sections), hero, features, pricing, FAQ, CTA, footer, JSON-LD `Organization`
(`name`, `url`, `logo`, `description`, `sameAs`). Base URL defaults to `https://<domain>` and can be
overridden by `Options.BaseURL` (`--base-url`, and the local address for `can site serve`).

## Server path resolution (`core/staticsrv`)

1. Reject (400) paths that do not start with `/`, are not valid UTF-8, or contain NUL or `\`, or have a `..` segment.
2. Reject (404) any segment starting with `.` (except `.well-known`) and any path ending `.gz` or `.br`.
3. Drop empty and `.` segments; join; look the result up inside `os.Root`: a directory redirects to `/dir/` (301), then `dir/index.html`; a missing extensionless path falls back to `<path>.html`.
4. For a hit, prefer `<file>.br` then `<file>.gz` when accepted (`q>0`) and present; otherwise the plain file.
5. Miss: serve `404.html` with status 404 if present, else a plain-text 404.

Cache policy: `assets/<name>.<8 hex>.<ext>` immutable one year; `.html` `no-cache`; other files one hour.
