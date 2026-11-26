# Feature 060: `can` on the server (E08g)

Status: built. Unit tested with SSH fakes and a real shell; the install runs against a k3d node in the
`make-up-e2e` workflow. Not run on a real server over SSH, never on an arm64 server, and no release (so no
release archive) exists yet.

## Why

The host backup script and the erasure sync timer run `/usr/local/bin/can`, and nothing copied it there. On a
real install erasures never reached the backup bucket, so a restore could bring erased people back, and the
backup and restore guidance assumed a binary the operator had to place by hand.

## Requirements

- `can up` (also `--provision-only`), `can backup install` and `can restore` install `can`; no manual step.
- The binary matches the server's OS and architecture (linux/amd64, linux/arm64) and the running tool: a named
  file, the release manifest's archive, the running binary, or a build of the source checkout, in that order.
- Verified by sha256 on the server and run there before it replaces anything; replaced by rename.
- The installed build is recorded (`.can/state.json` `cli`); a later `can up` upgrades it.
- With backups configured, no binary means the `backup` phase fails. It is never skipped silently.
- `can status` and `can verify --server` report the server's binary and version skew.

## Behaviour

Operator-facing: [docs/backup-restore.md](../../docs/backup-restore.md#the-can-on-the-server).

- `core/canbin` holds the sources, the probe, the install and the version comparison.
- "Backups configured" is: `--backup-*` on this run, a timer recorded in `.can/state.json`, or a timer found on
  the server (a server that cannot be asked counts as having one).
- Without `--backup-*` the install is its own phase, named `backup` when the state records a timer and `cli`
  otherwise. With them it is the first thing the `backup` phase does, so a failed install leaves no new timer.
- Builds are compared by version and by commit prefix; `dev` and `none` are not compared. An unstamped `can` in a
  checkout (`go run`, as `make up`) is identified by the checkout's `HEAD`.
- A release manifest gains `cli`: `{"linux/<arch>": {"file": "can_<version>_linux_<arch>.tar.gz",
  "sha256": "<hex>"}}`, written by `scripts/gen-release-images.sh` from the release's `SHA256SUMS`. A manifest
  without it still loads; the other sources apply. An older `can` refuses a manifest that has it.

## Limits

- The release archive is trusted as far as the manifest is: `can` does not verify the Sigstore signature.
- A cross-compile ships whatever is in the checkout, including uncommitted changes, which the recorded commit
  does not show.
- An older operator `can` replaces a newer server binary (the images it deploys are its own too); the run says
  which build it replaced.
- `can down` leaves `/usr/local/bin/can` on an `ssh` server.
- The release images are linux/amd64 only; on an arm64 server use `--images-from` with images built for it.
  This feature does not change that.
- `can restore` installs the binary after a successful restore, not before.
