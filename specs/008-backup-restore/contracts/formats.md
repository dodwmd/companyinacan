# Contract: file formats

**Completion marker** (`<dumps>/COMPLETE`): `run_id=<id>\nfinished_at=<unix seconds>\n`.

**Host state directory** (`/var/lib/can/host-state`, root-owned, a restic source): `MANIFEST.sha256` (`sha256sum` output, paths `./relative` to the dumps dir, excluding `COMPLETE`), `COMPLETE` (copy of the marker the backup ran under), `erasure-ledger.jsonl` (copy of the ledger), `k3s-state.db`, `token`, `encryption-config.json`.

**Recovery kit**: text with a human summary and a block between `-----BEGIN CAN RECOVERY KIT-----` and
`-----END CAN RECOVERY KIT-----` holding JSON (see data-model.md). Raw JSON is also accepted. Encrypted form:
`-----BEGIN CAN ENCRYPTED RECOVERY KIT-----`, base64 of `"CANKIT1\n" | salt(16) | t(u32) | m KiB(u32) | p(u8) | nonce(24) | ciphertext`,
`-----END ...-----`; key = argon2id(passphrase, salt, t=3, m=65536, p=4, 32 bytes); cipher XChaCha20-Poly1305,
AAD = everything up to and including `p`. Passphrase at least 12 characters.

**Ledger file** (`erasure-ledger.jsonl`): one JSON object per line, append-only, fields as in data-model.md.
Subject matches `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`, class `^[a-z][a-z0-9-]{0,63}$`.

**Sealed ledger object** (`ledger/erasure-ledger.v1.enc`): `"CANLEDGER1\n" | nonce(24) | ciphertext`; key =
HKDF-SHA256(restic password, salt `can-erasure-ledger`, info `v1`); XChaCha20-Poly1305, AAD = magic + object key;
plaintext is the JSONL entries, merged by (subject, class) with the earliest date, re-sequenced and re-chained.

**Restore state** (`.can/restore.json`): see data-model.md.

**restic invocation**: `RESTIC_REPOSITORY` and `RESTIC_PASSWORD_FILE` in the process environment; bucket credentials
from `restic.env`; the password never appears on a command line.
