#!/bin/sh
# Self-test for measure.sh's cluster ownership, with stand-ins for k3d, kubectl and can: nothing
# real is touched. measure.sh must never delete a cluster it did not create.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
cat >"$tmp/bin/k3d" <<'EOF'
#!/bin/sh
echo "k3d $*" >>"$CALLS"
if [ "$1 $2" = "cluster list" ]; then
	[ "$LIST" = fail ] && { echo "cannot reach docker" >&2; exit 1; }
	[ "$LIST" = exists ] && echo "fp-mine   1/1   0/0   true"
	echo "fp-mine-other   1/1   0/0   true"
fi
exit 0
EOF
# can and kubectl fail, which ends the run right after the cluster would have been created.
# shellcheck disable=SC2016 # $* and $CALLS are for the stand-in to expand
printf '#!/bin/sh\necho "can $*" >>"$CALLS"\nexit 1\n' >"$tmp/bin/can"
printf '#!/bin/sh\nexit 1\n' >"$tmp/bin/kubectl"
chmod +x "$tmp/bin/k3d" "$tmp/bin/can" "$tmp/bin/kubectl"

# run LIST-BEHAVIOUR MODE-ARGS...
run() {
	list=$1
	shift
	: >"$tmp/calls"
	PATH="$tmp/bin:$PATH" CALLS="$tmp/calls" LIST="$list" CAN=can \
		bash "$here/measure.sh" --label t --name fp-mine --out "$tmp/out" "$@" >"$tmp/log" 2>&1
}
fail() {
	echo "FAIL: $1"
	cat "$tmp/log" "$tmp/calls"
	exit 1
}

for mode in "--use-can-dev" "--k3d-config $here/k3d-lean.yaml"; do
	for list in exists fail; do
		# shellcheck disable=SC2086
		if run "$list" $mode; then fail "$mode, list $list: succeeded"; fi
		! grep -q "cluster delete" "$tmp/calls" || fail "$mode, list $list: deleted a cluster it did not create"
		! grep -qE "cluster create|dev up" "$tmp/calls" || fail "$mode, list $list: went on to create"
	done
	# No such cluster (one with a longer name does not count): created, and cleaned up on failure.
	# shellcheck disable=SC2086
	if run absent $mode; then fail "$mode: the stand-ins should have failed the run"; fi
	grep -qE "cluster create|dev up" "$tmp/calls" || fail "$mode: nothing was created"
	grep -q "cluster delete fp-mine" "$tmp/calls" || fail "$mode: its own cluster was left behind"
done
echo "measure_test: ok"
