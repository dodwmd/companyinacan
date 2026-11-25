#!/bin/sh
# Backup hook (SPEC 6.5): archive the DokuWiki storage directory (pages, media, config, plugins,
# page history) and PUT it to can-core, which writes it into the host dumps directory. DokuWiki is
# flat files with no database, so this archive is the whole dump. The raw PVC backup also covers the
# directory; this copy is the consistent one the restore hook uses.
# Runs as a Job in this module's namespace with the hooks image (deploy/hooks), the module's data
# volume mounted at WIKI_DATA (default /storage), and:
#   CAN_API_URL  CAN_MODULE  [CAN_TOKEN_FILE]   module API and projected audience-`can` token
set -eu

: "${CAN_API_URL:?}" "${CAN_MODULE:?}"
data="${WIKI_DATA:-/storage}"
token_file="${CAN_TOKEN_FILE:-/var/run/secrets/can/token}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The bearer token goes to curl through a 0600 header file, never through argv (visible in /proc).
umask 077
printf 'Authorization: Bearer %s\n' "$(cat "$token_file")" >"$tmp/auth"

[ -d "$data" ] || { echo "backup: $data not found (is the data volume mounted?)" >&2; exit 1; }

# DokuWiki writes a page by renaming a finished file into place, so a copy taken while the wiki is
# in use is consistent per page. Lock files are runtime state and are skipped.
tar -C "$data" --exclude='./data/locks' --exclude='./data/tmp' -czf "$tmp/storage.tar.gz" .
tar -tzf "$tmp/storage.tar.gz" >/dev/null
# An empty volume must never replace a good dump.
[ "$(wc -c <"$tmp/storage.tar.gz")" -gt 100 ] || { echo "backup: archive is implausibly small" >&2; exit 1; }

curl --fail --silent --show-error --retry 3 \
	-X PUT -H @"$tmp/auth" --connect-timeout 10 --max-time 600 \
	--data-binary @"$tmp/storage.tar.gz" \
	"$CAN_API_URL/v1/dumps/$CAN_MODULE/storage.tar.gz"
echo "backup: $CAN_MODULE storage archived"
