# Contract: `scripts/check-doc-links.sh`

- `sh scripts/check-doc-links.sh [FILE...]`; with no arguments it checks every `*.md` outside `.git`, `.claude`,
  `.specify`, `node_modules`, `testdata` and `specs/` (the per-feature working notes, whose historical links point at
  files that moved).
- A link is `](target)`. Skipped: `http://`, `https://`, `mailto:`, `tel:`, pure `#anchors`, text inside fenced code
  blocks and inline code spans. A `#fragment` or `?query` is dropped; the file or directory must exist. Absolute
  `/path` targets are resolved from the repository root; everything else from the file's directory. Headings are not checked.
- Output `file:line: broken link <target>` per failure and `N broken link(s)` on stderr; exit 1. `doc links ok`, exit 0.
- POSIX `sh` and `awk`, no network. `make docs-check` calls it; `TestDocumentationLinksResolve` runs it in `make contract`,
  and `TestDocLinkCheckerCatchesBrokenLinks` proves it detects a broken link and ignores code and external links.
