# Measurements: E01 Foundation

Recorded 2026-11-25 on the author's workstation (Linux 7.3.5, amd64, Go 1.27.3).

| Check | Result | Target |
|---|---|---|
| `make test` (lint + unit + contract + licence check), warm lint cache, cold test cache | 1.4 s | < 2 min (SC-001) |
| First `make test` on a machine that has not built golangci-lint before | ~13 s (includes fetching and building the pinned linter) | < 2 min |
| Unit + contract tests with networking disabled (`unshare -rn`), cold test cache | 0.23 s, all pass | no network needed (FR-016) |
| `can validate examples/acme-co` | < 0.1 s | < 1 s |

Notes:
- macOS is out of scope for now (no build hosts); CI runs on Linux only.
- Lint is a hard gate: the first lint run found 6 `errcheck` issues, which are fixed or excluded
  in `.golangci.yml` (writes to user-facing writers). An earlier Makefile fallback would have
  hidden them by falling back to `go vet` on any lint failure; it now falls back only when the
  linter cannot be fetched.
- SC-004 (CI attributes each failure to the right job) is pending T045, which needs a push.
