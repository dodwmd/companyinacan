#!/bin/sh
# hadolint-style checks on the release Dockerfiles (deploy/*/Dockerfile), with no extra tool (epic E20).
#   scripts/lint-dockerfiles.sh [--release] [root]
# Always an error: a literal FROM image with no digest and no ARG (so ":latest" too), ADD from a URL,
# piping a download into a shell, no USER, or a USER of root/0 as the last USER.
# REPLACE_WITH_ digest placeholders are allowed in development (a notice is printed) and are an
# error with --release (this is what `make release-check` also enforces via check-image-pins.sh).
set -eu
release=0
if [ "${1:-}" = "--release" ]; then release=1; shift; fi
root="${1:-$(dirname "$0")/..}"
fail=0
placeholders=0
err() { echo "$1" >&2; fail=1; }

for f in "$root"/deploy/*/Dockerfile; do
	[ -f "$f" ] || continue
	name=${f#"$root"/}
	stages=""
	last_user=""
	while IFS= read -r line; do
		case "$line" in
		FROM\ *)
			img=$(echo "$line" | awk '{print $2}')
			alias=$(echo "$line" | awk 'tolower($3)=="as"{print $4}')
			case "$img" in
			scratch) ;;
			\$\{*) ;; # an ARG: its default is checked below
			*@*) ;;
			*)
				# a reference to an earlier stage is fine
				case " $stages " in *" $img "*) ;; *) err "$name: FROM $img is not pinned by digest" ;; esac
				;;
			esac
			[ -n "$alias" ] && stages="$stages $alias"
			;;
		ARG\ *=*)
			case "$line" in *REPLACE_WITH_*) placeholders=$((placeholders + 1)) ;; esac
			;;
		ADD\ http*) err "$name: ADD from a URL (use a pinned download with a checksum): $line" ;;
		USER\ *) last_user=$(echo "$line" | awk '{print $2}') ;;
		RUN\ *)
			case "$line" in
			*curl*\|*sh* | *wget*\|*sh*) err "$name: pipes a download into a shell: $line" ;;
			esac
			;;
		esac
	done <"$f"
	[ -n "$last_user" ] || err "$name: no USER instruction (would run as root)"
	case "$last_user" in root | root:* | 0 | 0:*) err "$name: runs as root" ;; esac
done

if [ "$placeholders" -gt 0 ]; then
	if [ "$release" -eq 1 ]; then
		err "$placeholders REPLACE_WITH_ digest placeholder(s) in deploy/*/Dockerfile: resolve them with scripts/resolve-base-digests.sh"
	else
		echo "notice: $placeholders digest placeholder(s) remain (allowed in development, an error with --release)"
	fi
fi
[ "$fail" -eq 0 ] || exit 1
echo "lint-dockerfiles: ok"
