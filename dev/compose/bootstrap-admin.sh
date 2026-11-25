#!/usr/bin/env bash
# DEV ONLY. (Re)sets the can-core admin account of the docker-compose.yml stack to the public
# dev password and prints a NEW TOTP secret. Use it when the TOTP secret printed by the `init`
# service has scrolled away or was lost. The stack must be running (`make compose-up`).
#
#   dev/compose/bootstrap-admin.sh
#
# The username is `admin`, the password comes from dev/compose/secrets/core.env. These are fake,
# public credentials: never use them anywhere that is reachable by anyone else.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
project="${COMPOSE_PROJECT_NAME:-companyinacan}"
password="$(sed -n 's/^CAN_ADMIN_PASSWORD=//p' "$here/secrets/core.env" | head -n1)"
compose=(docker compose -p "$project")

run_core() { "${compose[@]}" exec -T -e "CAN_ADMIN_PASSWORD=$password" core /can-core "$@"; }

# can-core may still be starting after `up`: retry instead of relying on depends_on conditions.
out=""
for i in $(seq 1 30); do
  if out="$(run_core admin reset-password --rotate-totp 2>&1)"; then break; fi
  # Account missing (first run, init skipped): create it instead.
  if printf '%s' "$out" | grep -qi 'not found\|no such\|no admin'; then
    if out="$(run_core admin bootstrap 2>&1)"; then break; fi
  fi
  [ "$i" = 30 ] && { printf '%s\n' "$out" >&2; echo "bootstrap-admin: gave up; is the stack running? (docker compose -p $project ps)" >&2; exit 1; }
  sleep 2
done

cat <<EOF
==================================================================================
  DEV ONLY credentials. Never reuse them, never expose this stack.
  Admin UI : http://admin.localhost:8080
  Username : admin
  Password : $password
$(printf '%s\n' "$out" | grep -i 'totp secret\|provisioning uri' | sed 's/^/  /')
  Add the TOTP secret to an authenticator app (or: oathtool --totp -b <secret>).
==================================================================================
EOF
