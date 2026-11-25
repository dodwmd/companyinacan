#!/bin/sh
# Self-test for flaky-hunt.sh's argument checks, with a stand-in for go so no test is run.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
printf '#!/bin/sh\necho "$*" >>"%s"\n' "$tmp/go.calls" >"$tmp/bin/go"
chmod +x "$tmp/bin/go"

run() { PATH="$tmp/bin:$PATH" OUT="$tmp/out" LOAD=0 ITER="$1" sh "$here/flaky-hunt.sh" >"$tmp/log" 2>&1; }

for bad in 0 00 -1 x; do
	status=0
	run "$bad" || status=$?
	[ "$status" = 2 ] || { echo "FAIL: ITER=$bad exit $status, want 2"; cat "$tmp/log"; exit 1; }
	[ ! -e "$tmp/go.calls" ] || { echo "FAIL: ITER=$bad ran go"; exit 1; }
done
run 1 || { echo "FAIL: ITER=1 refused"; cat "$tmp/log"; exit 1; }
[ "$(wc -l <"$tmp/go.calls")" -eq 1 ] || { echo "FAIL: ITER=1 did not run go once"; exit 1; }
echo "flaky-hunt_test: ok"
