# Feature Specification: Real-server verification (Epic E06c)

**Branch**: `epic/E06c-real-verify` | **Date**: 2026-11-26 | **Status**: built; tested with fakes and, for the
cluster queries, the client-address probe and the rate-limit probe, the advisory `make-up-e2e` k3d job. Never run
against a real server.

Nothing in this project has run on a real server or cloud account. The owner will supply one once; that visit
must find as much as it can, safely. SPEC.md references: 3.2, 3.4, 6.5, 10.

## User scenarios

1. An operator runs `can verify --server` after `can up` and sees what passed, failed and was not checked.
2. They add probe flags, knowing what each sends.
3. They attach the JSON report to a public issue without leaking who they are or any secret.
4. They follow [the runbook](../../docs/real-server-verification.md) for what only a person can check.

## Requirements

- **FR-001** `can verify --server` checks the server recorded in `.can/state.json` over SSH. Each check has a
  stable id and one of `pass`, `fail`, `skip`. Exit 0 with no failure, 1 with any, 2 on usage.
- **FR-002** Without probe flags it runs only read-only commands over that SSH connection. It copies nothing to the
  server and reads no Secret value.
- **FR-003** Anything that sends traffic elsewhere needs a flag: `--probe-public`, `--probe-rate-limit`,
  `--probe-client-ip`, `--probe-backup-repo`, `--probe-smtp`, `--probe-sso`. A probe not asked for is listed as
  `skip` naming the flag.
- **FR-004** Nothing is destructive. No mail is sent and no sign-in succeeds. The one write is the audit entry (and
  failure count) of `--probe-client-ip`'s sign-in for a random user name that does not exist.
- **FR-005** Public probes connect to the recorded server address only, whatever DNS answers.
- **FR-006** The rate-limit probe only sends GET requests, at most 90, and says `skip` when the link is too slow for
  a verdict.
- **FR-007** The report (`can-verify/v1`; `--output json`, `--out FILE`, mode 0600, never overwritten) holds no
  secret, IP address, domain, company, server or user name, and no output of a remote command that could name the
  bucket, relay or identity provider. Module and workload names, counts and sizes are in it.
- **FR-008** A check that cannot be decided is `skip`; a server that cannot be asked is `fail`, and nothing else is
  then probed.

## Design

`core/verify` holds the checks; `core/cli/verify.go` loads the company and picks them. One POSIX script prints
`key=value` facts, judged in Go; the node isolation check is the existing `can-firewall check`. `can doctor
--server` is unchanged. A separate command keeps `doctor` about prerequisites and gives the report its own format.

Rejected: sending a test mail (needs a recipient and spends reputation: manual, through the contact form);
testing a restore (destructive: manual, on a second server); logging in as the administrator (needs the password
and TOTP).

## Not verified

- Anything on a real server. The host facts (systemd, ufw, timedatectl, journal, `acme.json`, backup files) are
  tested against stand-in commands and files; their real output formats are from memory.
- `--probe-backup-repo`, `--probe-smtp` and `--probe-sso` have only met fakes.
- `--probe-client-ip` cannot compare the recorded address with the operator's own; it rejects cluster-internal and,
  for a public server, private ones. The runbook has the comparison.
- k3d hides client addresses, so the CI job only checks that the probe reaches a verdict.
- `--probe-smtp` and `--probe-sso` connect from the host, not from a pod, so NetworkPolicy is not exercised.
