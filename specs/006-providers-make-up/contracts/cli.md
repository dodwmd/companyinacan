# Contract: `can up | down | status` and `make` targets

Global flags as in `specs/001-project-foundation/contracts/cli.md` (`--output text|json`,
`--no-color`). All three take an optional `[dir]` (default `.`) and `--modules-dir`. Progress goes to
stdout in text mode and to stderr in JSON mode (stdout is then one JSON document). Every byte
written passes through the redactor.

Exit codes: `0` success, `1` failure (invalid company, preflight failed, API or SSH error,
status not ready), `2` usage (bad flag, `down` without `--yes`), `3` provider or DNS plugin not
implemented.

## `can up [dir] [--plan] [--dns manual]`

1. Validates `dir/company.yaml` (exit 1 with problems on error).
2. `--plan`: prints `Plan for provider "<type>"` with numbered `[todo]`/`[done]` actions; JSON:
   `{provider, actions[], planned:true}`. No network, no state, no files.
3. Otherwise `Create`, then fetches the kubeconfig into `dir/.can/kubeconfig`, plans the instant
   URL, prints the address, kubeconfig path, instant URL (https or fallback with reason), the DNS
   records via the plugin, and the E03 follow-up note.
   JSON adds `address`, `instant`, `dns_records`, `kubeconfig`, `next`.

## `can down [dir] [--plan] [--yes]`

Prints the provider-specific warning first (`ssh`: uninstall k3s and can-core, server left as is;
`hetzner`: delete server, firewall and uploaded key, data lost). Without `--yes` (and without
`--plan`) exits 2 and does nothing. Removes `.can/kubeconfig` and state on success. Idempotent.

## `can status [dir]`

Prints provider, phase, address and checks; JSON `{provider, phase, address, checks[]}`.
Exit 0 only for phase `ready`.

## Make targets

`make up|down|status` run `go run ./cmd/can <cmd> $(ARGS)`; e.g. `make up ARGS=--plan`,
`make down ARGS=--yes`. `make dev` stays a stub (E02).
