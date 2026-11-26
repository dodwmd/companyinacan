#!/bin/sh
# Optional flaky-test hunter. Runs the unit tests repeatedly, with shuffled order and CPU contention,
# and prints every test that failed at least once. It does not fix anything and it is not part of
# `make test`; see CONTRIBUTING.md ("Flaky tests") and docs/measurements/ci-stability.md.
#
#   sh scripts/flaky-hunt.sh                       # 10 iterations of the whole unit suite
#   ITER=20 sh scripts/flaky-hunt.sh               # more iterations
#   PKGS=./core/server/audit/... ITER=50 sh scripts/flaky-hunt.sh
#   LOAD=0 sh scripts/flaky-hunt.sh                # no background CPU load
#
# Environment:
#   ITER   iterations (default 10)
#   PKGS   go packages (default ./cmd/... ./core/... ./test/wiring/... ./test/e2e/harness/... ./dev/...)
#   LOAD   number of busy-loop background processes (default: half the CPUs, at least 1)
#   RACE   1 (default) runs with -race, 0 without
#   OUT    output directory (default flaky-hunt-out); failing-tests.txt lists "package test count"
#
# Exit status: 0 when no test failed in any iteration, 1 when at least one did, 2 on a usage error.
# Uses only POSIX sh and the Go toolchain; busy loops are plain shell loops, killed on exit.
set -eu

ITER=${ITER:-10}
PKGS=${PKGS:-"./cmd/... ./core/... ./test/wiring/... ./test/e2e/harness/... ./dev/..."}
RACE=${RACE:-1}
OUT=${OUT:-flaky-hunt-out}

cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)
LOAD=${LOAD:-$((cpus / 2 > 0 ? cpus / 2 : 1))}

case $ITER in '' | *[!0-9]*) echo "flaky-hunt: ITER must be a number" >&2; exit 2 ;; esac
case $LOAD in '' | *[!0-9]*) echo "flaky-hunt: LOAD must be a number" >&2; exit 2 ;; esac
# Zero iterations would run no test and report "no failures".
[ "$ITER" -ge 1 ] || { echo "flaky-hunt: ITER must be at least 1" >&2; exit 2; }

here=$(cd "$(dirname "$0")" && pwd)
cd "$here/.."
mkdir -p "$OUT"
: >"$OUT/failing-tests.txt"
: >"$OUT/raw-failures.txt"

pids=""
cleanup() {
	# shellcheck disable=SC2086
	[ -z "$pids" ] || kill $pids 2>/dev/null || true
}
trap cleanup EXIT
# A signal must end the run (128 + signal number), not just clean up and carry on; exiting runs the
# EXIT trap.
trap 'exit 130' INT
trap 'exit 143' TERM

i=0
while [ "$i" -lt "$LOAD" ]; do
	(while :; do :; done) &
	pids="$pids $!"
	i=$((i + 1))
done
echo "flaky-hunt: $ITER iterations, $LOAD busy loops on $cpus CPUs, race=$RACE, packages: $PKGS"

flags="-count=1 -shuffle=on -json"
[ "$RACE" = 1 ] && flags="-race $flags"

n=1
while [ "$n" -le "$ITER" ]; do
	log="$OUT/iter-$n.json"
	status=0
	# shellcheck disable=SC2086
	go test $flags $PKGS >"$log" 2>"$OUT/iter-$n.err" || status=$?
	before=$(wc -l <"$OUT/raw-failures.txt")
	# A failing test is a JSON line with "Action":"fail" and a "Test" field.
	grep '"Action":"fail"' "$log" | grep '"Test":"' |
		sed -e 's/.*"Package":"\([^"]*\)".*"Test":"\([^"]*\)".*/\1 \2/' >>"$OUT/raw-failures.txt" || true
	# Packages that failed to build or that crashed have a package-level fail and no test line.
	grep '"Action":"fail"' "$log" | grep -v '"Test":"' |
		sed -e 's/.*"Package":"\([^"]*\)".*/\1 (package)/' >>"$OUT/raw-failures.txt" || true
	# go test failed but nothing above says why (bad package pattern, setup or build error, killed):
	# that is a failure too, never a clean iteration.
	if [ "$status" -ne 0 ] && [ "$(wc -l <"$OUT/raw-failures.txt")" -eq "$before" ]; then
		echo "go-test (exit-status-$status)" >>"$OUT/raw-failures.txt"
		cat "$OUT/iter-$n.err" >&2
	fi
	echo "flaky-hunt: iteration $n/$ITER done ($(wc -l <"$OUT/raw-failures.txt" | tr -d ' ') failures so far)"
	rm -f "$log"
	[ -s "$OUT/iter-$n.err" ] || rm -f "$OUT/iter-$n.err"
	n=$((n + 1))
done

if [ -s "$OUT/raw-failures.txt" ]; then
	sort "$OUT/raw-failures.txt" | uniq -c | sort -rn | awk '{c=$1; $1=""; print substr($0,2), c}' >"$OUT/failing-tests.txt"
	echo "flaky-hunt: FAILING TESTS (package test failures-in-$ITER-iterations):"
	cat "$OUT/failing-tests.txt"
	exit 1
fi
echo "flaky-hunt: no failures in $ITER iterations"
