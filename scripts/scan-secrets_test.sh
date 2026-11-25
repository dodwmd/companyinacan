#!/bin/sh
# Self-test for scan-secrets.sh on a throwaway directory. Fixtures are assembled at run time so this
# file itself contains no credential-shaped string.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export SECRET_ALLOWLIST="$tmp/allow.txt"
: >"$SECRET_ALLOWLIST"

mkdir -p "$tmp/repo/dev"
printf 'hello\nport = 8080\n' >"$tmp/repo/ok.txt"
sh "$here/scan-secrets.sh" "$tmp/repo" >/dev/null || { echo "FAIL: clean tree rejected"; exit 1; }

key="AKIA""ABCDEFGHIJKLMNOP"
printf 'aws = %s\n' "$key" >"$tmp/repo/dev/a.env"
if sh "$here/scan-secrets.sh" "$tmp/repo" >"$tmp/out" 2>&1; then echo "FAIL: AWS key accepted"; exit 1; fi
grep -q 'dev/a.env' "$tmp/out" || { echo "FAIL: path not reported"; exit 1; }
if grep -q "$key" "$tmp/out"; then echo "FAIL: reported the secret value in full"; exit 1; fi

echo 'dev/a.env fake value for the test' >"$SECRET_ALLOWLIST"
sh "$here/scan-secrets.sh" "$tmp/repo" >/dev/null || { echo "FAIL: allowlisted file rejected"; exit 1; }
: >"$SECRET_ALLOWLIST"

rm "$tmp/repo/dev/a.env"
printf '%s\n' "-----BEGIN RSA PRIV""ATE KEY-----" >"$tmp/repo/k.pem"
if sh "$here/scan-secrets.sh" "$tmp/repo" >/dev/null 2>&1; then echo "FAIL: private key accepted"; exit 1; fi
rm "$tmp/repo/k.pem"

printf 'password = "%s"\n' "abcdefghijklmnopqrstuvwxyz0123456789" >"$tmp/repo/c.conf"
if sh "$here/scan-secrets.sh" "$tmp/repo" >/dev/null 2>&1; then echo "FAIL: quoted password literal accepted"; exit 1; fi
rm "$tmp/repo/c.conf"

# shellcheck disable=SC2016
printf 'password = "%s"\n' '${PASSWORD}' >"$tmp/repo/t.conf"
sh "$here/scan-secrets.sh" "$tmp/repo" >/dev/null || { echo "FAIL: template reference rejected"; exit 1; }
echo "scan-secrets_test: ok"
