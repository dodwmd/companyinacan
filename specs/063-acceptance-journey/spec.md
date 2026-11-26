# Spec 063: one test of the whole journey (epic E00b)

Status: built. Results: [docs/measurements/acceptance.md](../../docs/measurements/acceptance.md).

## Problem

The product promise is a journey: `make up` gives a live HTTPS site, forms and a way in within minutes
on a small server, then modules, backups and restore. Each part had its own test, most of them calling
Go code with fakes for the parts around it. Nothing walked the journey with the binary a user runs.

## Requirements

1. Start from the built `can` binary, the built images and an empty directory outside the repository.
2. In order: `can init`; `can up` with images, backups and a mail relay; the site over HTTPS; the
   contact form from the link on the site; the notification at a mail sink; admin sign-in with TOTP;
   enable `work` and one wrapped module (`support`); SSO launch and an issue; backup and verify;
   destroy; `can restore`; the data and the logins present.
3. Time every step and every `can up` phase, record node memory, fail on generous budgets, and publish
   a report (Markdown and JSON) as a CI artifact, also when the journey fails.
4. Advisory: a workflow of its own, not a required check.
5. Never run on a developer's machine: it refuses without two environment variables.

## Decision: the runner VM is the server

| Option | Why |
|---|---|
| `ssh` provider onto the CI runner VM itself (chosen) | the shipped path unchanged: preflight, bootstrap script, k3s, ufw, node firewall, image upload over ssh, SSH tunnel, systemd backup timer, `can restore` on the server, `can down` |
| k3d | `can up` has no k3d provider (`local` is not implemented), so the binary cannot drive it; `make-up-e2e` and `restore-e2e` already run the in-cluster phases on k3d through Go seams |
| a privileged systemd container as the server | k3s, nftables and swap in a container differ from a server; more moving parts than the VM that is already there |

## A step the product does not do

`can up` does not copy `can` to the server, so after the first `can up` the journey installs it by hand, as
docs/backup-restore.md tells the operator to. The erasure ledger, including the empty one that lets a company
without erasures be restored, needs it there. The report lists this with the stand-ins. When `can up` installs it,
the manual step becomes an assertion.

## Stand-ins

Everything that needs the internet or an account is replaced, and the report says so: a test CA and
Traefik's default certificate (Let's Encrypt is blocked), `/etc/hosts` for DNS, `dev/compose/smtpsink`
with STARTTLS and AUTH as the relay (can-core gets `CAN_SMTP_ALLOW_PRIVATE=true` and the CA), and an S3
gateway on the same machine as the bucket.

## Not covered

A fresh cloud image and VM creation, ACME, real DNS, deliverability, a real bucket, a restore onto a
second machine, a browser engine, erasure through a restore (`restore-e2e`), and the modules not named.

## Files

`test/acceptance/` (journey behind the build tag `acceptance`, `report/` unit tested in `make unit`),
`test/acceptance/setup-runner.sh`, `.github/workflows/acceptance.yml`.
