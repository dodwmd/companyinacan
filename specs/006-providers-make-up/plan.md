# Implementation Plan: Server Providers and `make up` (Epic E06)

**Branch**: `epic/E06-providers` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

## Summary

Add `core/provider` (interface, state store with file locking, redactor, SSH runner, bootstrap and
cloud-init generators, preflight, `ssh` and `hetzner` providers, DNS plugins, sslip.io helper,
tunnel and kubeconfig retrieval) and replace the `can up` stub with real `up`, `down`, `status`
commands plus Makefile targets. Design choices are in [research.md](research.md), entities in
[data-model.md](data-model.md), the CLI and option contracts in [contracts/](contracts/).

## Technical Context

**Language/Version**: Go 1.27 (existing module)

**Primary Dependencies**: none added. Standard library `net/http` for Hetzner; the system `ssh`
binary (via `os/exec`) for SSH, so no `golang.org/x/crypto` and no new licence entries.

**Storage**: `.can/state.json` (+ `.lock`), `.can/kubeconfig`; both inside the git-ignored `.can/`.

**Testing**: `go test`; Hetzner via `httptest` with `testdata/hetzner/fixtures.json`; SSH via a
`Runner` fake and a helper-process test for the real `SSHRunner` argv; bootstrap by golden files,
`bash -n`, `shellcheck` (skipped if absent) and structural assertions; CLI tests in-process.

**Target Platform**: Linux operator machines (flock); managed hosts Ubuntu/Debian, amd64/arm64.

**Constraints**: no real servers, accounts or clusters in tests; token never in output.

## Constitution Check

| Principle | Assessment |
|---|---|
| I. Spec-first | SPEC.md sections 3.1, 3.4, 3.5 are the source; this feature restates only what it implements. The k3s config-file form of the flags (instead of `INSTALL_K3S_EXEC`) is a refinement recorded in research.md R2. |
| II. Footprint is a feature | Bootstrap installs only trimmed k3s (metrics-server and cloud controller disabled), swap for small boxes, no extra packages except `ufw` when missing. No resident component is added; the CLI is a client. |
| III. Test-first | Tests are written per unit before the code that satisfies them (generator, redactor, state, runner, API client, providers, CLI). Golden files pin the script; structural tests assert the properties independently. |
| IV. Stable module contracts | No module contract touched. `provider.options` keys are documented in contracts/options.md and validated by the providers. |
| V. Secure and private by default | Default-deny host firewall, 6443 never opened, tunnel bound to loopback, secrets only by reference, redaction tested end to end, state 0600 and secret-free, input allow-lists against shell injection, tarballs checksum-verified. |
| VI. Open-source hygiene | No new dependency; Apache-2.0 headers unchanged; commits signed off. |
| VII. Simplicity and measured claims | Everything needing a real host is marked unverified; the 5-minute claim is not made here. Defaults that are guesses (k3s pin, Hetzner server type) are flagged in code and docs. |

**Result**: pass, no violations to justify.

## Project Structure

```text
core/provider/
  provider.go      Provider interface, Deps, factory, stubs (local, static, digitalocean)
  state.go         State, Store (flock, atomic write)
  redact.go        Redactor + redacting writer
  options.go       option reader, secret references
  runner.go        Runner interface, SSHRunner, argv construction and validation
  tunnel.go        SSH -L tunnel, kubeconfig rewrite and fetch
  bootstrap.go     bootstrap script + cloud-init generators, uninstall script
  preflight.go     probe script and parser
  ssh.go           ssh provider, status probe
  hetzner_api.go   API client
  hetzner.go       hetzner provider
  dns.go           DNS plugin interface, manual plugin, record builder
  sslip.go         instant URL planning, ACME-limit-aware fallback
  testdata/        golden scripts, Hetzner fixtures
core/cli/up.go     up, down, status commands (root.go registers them; stubs.go untouched)
docs/providers.md, docs/make-up.md
```

## Complexity Tracking

None.
