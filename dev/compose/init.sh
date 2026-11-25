#!/bin/sh
# DEV ONLY. One-shot init for docker-compose.yml (runs as root in the `init` image).
#  1. generates random dev keyrings for can-people and can-jobs into the dev-secrets volume
#     (only if absent, so data stays readable across restarts);
#  2. bootstraps the can-core admin account once and prints its TOTP secret.
# Idempotent: safe to run on every `compose up`.
set -eu

umask 077
SECRETS=/secrets
for svc in people jobs; do
  f="$SECRETS/$svc-keyring"
  if [ ! -s "$f" ]; then
    echo "init: generating dev keyring $f"
    printf 'dev-1 %s\n' "$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')" > "$f"
  fi
  chown 65532:65532 "$f"
  chmod 0400 "$f"
done

export CAN_DB_PATH="${CAN_DB_PATH:-/var/lib/can/state.db}"
if [ -z "${CAN_ADMIN_PASSWORD:-}" ]; then echo "init: CAN_ADMIN_PASSWORD is not set" >&2; exit 1; fi
if out="$(can-core admin bootstrap 2>&1)"; then
  cat <<BANNER
==================================================================================
  DEV ONLY credentials. Never reuse them, never expose this stack.
  Admin UI : http://admin.localhost:8080
  Username : admin
  Password : $CAN_ADMIN_PASSWORD
  $(echo "$out" | grep -i 'totp secret')
  $(echo "$out" | grep -i 'provisioning uri')
  (shown once; run dev/compose/bootstrap-admin.sh to rotate and print a new TOTP secret)
==================================================================================
BANNER
elif echo "$out" | grep -q 'already exists'; then
  echo "init: admin already bootstrapped (run dev/compose/bootstrap-admin.sh for a fresh TOTP secret)"
else
  echo "$out" >&2
  exit 1
fi
# can-core runs as 65532: hand it the database files this root process created.
chown -R 65532:65532 /var/lib/can
