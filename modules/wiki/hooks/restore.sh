#!/bin/sh
# Restore hook (SPEC 6.5 step "restore databases"): GET the storage archive from can-core and unpack
# it onto the module's (new, empty) data volume, replacing whatever is there. can-core scales the app
# to zero first, so nothing writes while the files are replaced. Same environment as backup.sh.
set -eu

: "${CAN_API_URL:?}" "${CAN_MODULE:?}"
data="${WIKI_DATA:-/storage}"
token_file="${CAN_TOKEN_FILE:-/var/run/secrets/can/token}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The bearer token goes to curl through a 0600 header file, never through argv (visible in /proc).
umask 077
printf 'Authorization: Bearer %s\n' "$(cat "$token_file")" >"$tmp/auth"

curl --fail --silent --show-error --retry 3 \
	-H @"$tmp/auth" --connect-timeout 10 --max-time 600 \
	-o "$tmp/storage.tar.gz" \
	"$CAN_API_URL/v1/dumps/$CAN_MODULE/storage.tar.gz"
gzip -t "$tmp/storage.tar.gz"
tar -tzf "$tmp/storage.tar.gz" >/dev/null

# The data volume must really be mounted: unpacking into the Job's own filesystem would restore
# nothing and report success. (The Job pod needs fsGroup 1000 to write it, contracts/hooks.md.)
mountpoint -q "$data" || { echo "restore: $data is not a mounted volume" >&2; exit 1; }
# Extract into a staging directory ON THE SAME VOLUME first, so a failed or truncated extraction
# (full disk, corrupt member) leaves the live data untouched instead of an emptied volume. Only
# after it succeeded is the old content moved aside and the new content moved in (renames within
# one filesystem); the old content is deleted last, and put back if the swap itself fails.
stage="$data/.restore-new"
old="$data/.restore-old"
rm -rf "$stage" "$old"
mkdir "$stage"
if ! tar -C "$stage" -xzf "$tmp/storage.tar.gz"; then
	rm -rf "$stage"
	echo "restore: extraction failed; $data was left unchanged" >&2
	exit 1
fi
mkdir "$old"
# replace-all: stale files from an earlier life must not survive next to the restored ones
for f in "$data"/* "$data"/.[!.]* "$data"/..?*; do
	{ [ -e "$f" ] || [ -L "$f" ]; } || continue
	case "$f" in "$stage" | "$old") continue ;; esac
	mv "$f" "$old"/
done
for f in "$stage"/* "$stage"/.[!.]* "$stage"/..?*; do
	{ [ -e "$f" ] || [ -L "$f" ]; } || continue
	if ! mv "$f" "$data"/; then
		echo "restore: could not move $f into place; putting the previous data back" >&2
		for g in "$data"/* "$data"/.[!.]* "$data"/..?*; do
			{ [ -e "$g" ] || [ -L "$g" ]; } || continue
			case "$g" in "$stage" | "$old") continue ;; esac
			rm -rf "$g"
		done
		for g in "$old"/* "$old"/.[!.]* "$old"/..?*; do
			{ [ -e "$g" ] || [ -L "$g" ]; } || continue
			mv "$g" "$data"/
		done
		rm -rf "$stage" "$old"
		exit 1
	fi
done
rm -rf "$stage" "$old"
echo "restore: $CAN_MODULE storage restored"
