#!/bin/sh
# Offline check that every relative Markdown link in the documentation resolves to a file or
# directory in the repository. Usage:
#   sh scripts/check-doc-links.sh            # check the default set
#   sh scripts/check-doc-links.sh FILE...    # check these files only
# External links (http, https, mailto), pure #anchors and links inside fenced code blocks are skipped;
# a #fragment or ?query on a relative link is ignored (the file must exist, headings are not checked).
# The default set is every tracked or untracked *.md except the generated and tool-owned trees
# (.git, .claude, .specify, node_modules) and the per-feature working notes under specs/, whose
# historical links point at files that moved. Exit status 1 and one line per broken link on failure.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

if [ "$#" -gt 0 ]; then
  files=$(printf '%s\n' "$@")
else
  files=$(find . \( -name .git -o -name .claude -o -name .specify -o -name node_modules -o -name specs -o -name testdata \) -prune -o -type f -name '*.md' -print | sed 's|^\./||' | sort)
fi

bad=0
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

for f in $files; do
  dir=$(dirname "$f")
  # file:line:target for every inline link outside fenced code blocks. Inline code spans are blanked first.
  awk -v F="$f" '
    /^[ \t]*(```|~~~)/ { fence = !fence; next }
    fence { next }
    {
      line = $0
      gsub(/`[^`]*`/, "", line)
      while (match(line, /\]\([^)]*\)/)) {
        t = substr(line, RSTART + 2, RLENGTH - 3)
        sub(/[ \t]+".*$/, "", t)           # [x](url "title")
        gsub(/^<|>$/, "", t)
        print F ":" NR ":" t
        line = substr(line, RSTART + RLENGTH)
      }
    }' "$f" >"$tmp"
  while IFS= read -r rec; do
    target=${rec#*:*:}
    loc=${rec%:"$target"}
    case "$target" in
      '' | '#'* | http://* | https://* | mailto:* | tel:*) continue ;;
    esac
    path=${target%%#*}
    path=${path%%\?*}
    [ -n "$path" ] || continue
    case "$path" in
      /*) full="$root$path" ;;
      *) full="$dir/$path" ;;
    esac
    if [ ! -e "$full" ]; then
      printf '%s: broken link %s\n' "$loc" "$target"
      bad=$((bad + 1))
    fi
  done <"$tmp"
done

if [ "$bad" -gt 0 ]; then
  printf '%d broken link(s)\n' "$bad" >&2
  exit 1
fi
echo "doc links ok"
