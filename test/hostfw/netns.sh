#!/usr/bin/env bash
# Exercises core/provider/hostfw/can-firewall.sh with real nftables in throw-away network
# namespaces: a "node" (bridge cni0, kube-proxy-like DNAT, ufw-like accepts), a system pod, a
# module pod, an outside operator and an upstream with a fake cloud metadata address.
# Every BLOCKED verdict has a REACHED control with the same probe, so a broken probe cannot pass.
#
# Needs root, nft, ip, socat and unshare. Run it only in a disposable environment (CI runner or a
# container with its own network namespace); CAN_HOSTFW_THROWAWAY=1 confirms that.
set -euo pipefail

[ "${CAN_HOSTFW_THROWAWAY:-}" = 1 ] || {
  echo "refusing to run: set CAN_HOSTFW_THROWAWAY=1 in a disposable environment" >&2
  exit 2
}
[ "$(id -u)" -eq 0 ] || { echo "needs root" >&2; exit 2; }
for c in nft ip socat timeout nsenter pkill unshare; do
  command -v "$c" >/dev/null || { echo "needs $c" >&2; exit 2; }
done

here=$(cd "$(dirname "$0")" && pwd)
FW="$here/../../core/provider/hostfw/can-firewall.sh"
work=$(mktemp -d)
NODE=cfw-node SYS=cfw-sys MOD=cfw-mod EXT=cfw-ext UP=cfw-up
pids=()

drop_namespaces() {
  for ns in $NODE $SYS $MOD $EXT $UP; do
    ip netns pids "$ns" 2>/dev/null | xargs -r kill -9 2>/dev/null || true
    ip netns del "$ns" 2>/dev/null || true
  done
}
cleanup() {
  for p in ${pids[@]+"${pids[@]}"}; do kill "$p" 2>/dev/null || true; done
  drop_namespaces
  rm -rf "$work"
}
trap cleanup EXIT
drop_namespaces

in_ns() {
  local ns=$1
  shift
  nsenter --net="/var/run/netns/$ns" "$@"
}

# ---- topology -------------------------------------------------------------------------------
for ns in $NODE $SYS $MOD $EXT $UP; do
  ip netns add "$ns"
  in_ns "$ns" ip link set lo up
done
in_ns $NODE ip link add cni0 type bridge
in_ns $NODE ip addr add 10.42.0.1/24 dev cni0
in_ns $NODE ip link set cni0 up
pod() { # ns address host-side-name
  ip link add "$3" netns $NODE type veth peer name eth0 netns "$1"
  in_ns $NODE ip link set "$3" master cni0 up
  in_ns "$1" ip addr add "$2/24" dev eth0
  in_ns "$1" ip link set eth0 up
  in_ns "$1" ip route add default via 10.42.0.1
}
pod $SYS 10.42.0.10 vsys
pod $MOD 10.42.0.20 vmod
ip link add eth0 netns $NODE type veth peer name eth0 netns $EXT
in_ns $NODE ip addr add 192.0.2.1/24 dev eth0
in_ns $NODE ip link set eth0 up
in_ns $EXT ip addr add 192.0.2.50/24 dev eth0
in_ns $EXT ip link set eth0 up
# An operator whose own address happens to be inside the pod CIDR (a VPN, say) must still get in.
in_ns $EXT ip addr add 10.42.99.1/32 dev eth0
in_ns $NODE ip route add 10.42.99.1/32 dev eth0
ip link add up0 netns $NODE type veth peer name eth0 netns $UP
in_ns $NODE ip addr add 198.51.100.1/24 dev up0
in_ns $NODE ip link set up0 up
in_ns $UP ip addr add 198.51.100.2/24 dev eth0
in_ns $UP ip link set eth0 up
in_ns $UP ip addr add 169.254.169.254/32 dev lo
in_ns $UP ip addr add 203.0.113.9/32 dev lo
in_ns $NODE ip route add 169.254.169.254/32 via 198.51.100.2
in_ns $NODE ip route add 203.0.113.9/32 via 198.51.100.2
in_ns $UP ip route add 10.42.0.0/24 via 198.51.100.1
in_ns $NODE sh -c 'echo 1 > /proc/sys/net/ipv4/ip_forward'
# IPv6 from the pods to an upstream holding the cloud metadata address fd00:ec2::254.
in_ns $NODE ip addr add fd42::1/64 dev cni0 nodad
in_ns $SYS ip addr add fd42::10/64 dev eth0 nodad
in_ns $SYS ip route add default via fd42::1 dev eth0
in_ns $MOD ip addr add fd42::20/64 dev eth0 nodad
in_ns $MOD ip route add default via fd42::1 dev eth0
in_ns $NODE ip addr add fd99::1/64 dev up0 nodad
in_ns $UP ip addr add fd99::2/64 dev eth0 nodad
in_ns $UP ip addr add fd00:ec2::254/128 dev lo nodad
in_ns $UP ip route add fd42::/64 via fd99::1 dev eth0
in_ns $NODE ip route add fd00:ec2::254/128 via fd99::2 dev up0
in_ns $NODE sh -c 'echo 1 > /proc/sys/net/ipv6/conf/all/forwarding'

# What the other parties on a k3s host load: kube-proxy's DNAT of the kubernetes Service, an
# accept EARLIER than ours and a ufw-like input chain after it. None may override our drop.
in_ns $NODE nft -f - <<'NFT'
table ip proxy_like {
  chain prerouting {
    type nat hook prerouting priority -100;
    ip daddr 10.43.0.1 tcp dport 443 dnat to 192.0.2.1:6443
    # A resolver of the node reached through DNAT on another port (k3d does this; so can a host).
    ip daddr 198.18.0.53 udp dport 53 dnat to 192.0.2.1:5353
    ip daddr 198.18.0.53 tcp dport 53 dnat to 192.0.2.1:5353
  }
}
table inet router_like {
  chain input {
    type filter hook input priority -20;
    ip saddr 10.42.0.0/16 accept
  }
}
table inet ufw_like {
  chain input {
    type filter hook input priority 0; policy drop;
    iifname "lo" accept
    ct state established,related accept
    ip saddr 10.42.0.0/16 accept
    meta l4proto 58 accept
    iifname "cni0" accept
    tcp dport { 22, 80, 443 } accept
  }
}
NFT

serve() { # ns tcp-port
  nsenter --net="/var/run/netns/$1" socat "TCP6-LISTEN:$2,fork,reuseaddr,ipv6only=0" SYSTEM:'echo can-ok' >/dev/null 2>&1 &
  pids+=("$!")
}
for p in 22 53 5353 6443 10250; do serve $NODE "$p"; done
nsenter --net="/var/run/netns/$NODE" socat -T2 UDP4-RECVFROM:5353,bind=192.0.2.1,fork SYSTEM:'echo can-ok' >/dev/null 2>&1 &
pids+=("$!")
nsenter --net="/var/run/netns/$NODE" socat -T2 UDP4-RECVFROM:53,fork SYSTEM:'echo can-ok' >/dev/null 2>&1 &
pids+=("$!")
serve $MOD 8080
serve $UP 80
serve $UP 443

# The cluster the script asks: one system pod, no can-core; readyz answers unless told otherwise.
# $work/pods holds namespace|app name|hostNetwork|podIP|deletionTimestamp, one pod per line.
cat >"$work/kubectl" <<STUB
#!/usr/bin/env bash
case "\$*" in
  *--raw=/readyz*) [ ! -e "$work/readyz.fail" ] ;;
  *"get pods --all-namespaces"*)
    [ ! -e "$work/list.fail" ] || exit 1
    cat "$work/pods"
    ;;
  *) exit 1 ;;
esac
STUB
# A systemd that remembers what is enabled, active and failed. $work/systemctl.fail makes the next
# restart of both units fail once, leaving them inactive.
mkdir -p "$work/enabled" "$work/active" "$work/failed"
cat >"$work/systemctl" <<STUB
#!/usr/bin/env bash
echo "\$*" >>"$work/systemctl.log"
verb=\$1
shift
[ "\${1:-}" != --quiet ] || shift
case \$verb in
  enable) for u; do touch "$work/enabled/\$u"; done ;;
  disable) for u; do [ "\$u" = --now ] || rm -f "$work/enabled/\$u" "$work/active/\$u"; done ;;
  stop) for u; do rm -f "$work/active/\$u"; done ;;
  restart)
    if [ -e "$work/systemctl.fail" ] && [ \$# -gt 1 ]; then
      rm -f "$work/systemctl.fail"
      for u; do rm -f "$work/active/\$u"; done
      exit 1
    fi
    for u; do touch "$work/active/\$u"; rm -f "$work/failed/\$u"; done
    # With $work/systemctl.execs, starting a unit also runs its command: the installed script.
    [ ! -e "$work/systemctl.execs" ] || bash "\$FW_UNDER_TEST" apply
    ;;
  is-enabled) [ -e "$work/enabled/\$1" ] ;;
  is-active)
    if [ -e "$work/active/\$1" ]; then echo active; else echo inactive; exit 3; fi
    ;;
  is-failed) [ -e "$work/failed/\$1" ] ;;
esac
STUB
# nft that can be told to refuse new rules while still listing the loaded ones.
mkdir -p "$work/bin"
cat >"$work/bin/nft" <<STUB
#!/usr/bin/env bash
if [ -e "$work/nft.fail" ] && [ "\$1" = -c ]; then
  echo "injected nft failure" >&2
  exit 1
fi
exec "$(command -v nft)" "\$@"
STUB
chmod +x "$work/kubectl" "$work/systemctl" "$work/bin/nft"
pods() { printf '%s\n' "$@" >"$work/pods"; }
SYS_POD='kube-system||false|10.42.0.10|'
# The hostNetwork and address-less entries must never be trusted.
pods "$SYS_POD" 'kube-system||true|10.42.0.20|' 'kube-system||||'

fw() {
  in_ns $NODE env PATH="$work/bin:$PATH" CAN_FW_KUBECTL="$work/kubectl" CAN_FW_SYSTEMCTL="$work/systemctl" CAN_FW_UNIT_DIR="$work/units" \
    CAN_FW_SELF="$work/can-firewall" CAN_FW_CONF_DIR="$work/conf" CAN_FW_CNI_STATE="$work/cni" CAN_FW_WAIT=6 "$@"
}
can() { fw bash "$FW" "$@"; }

# ---- probes ---------------------------------------------------------------------------------
verdict() { # ns proto host port
  local out=''
  case $2 in
    tcp) out=$(in_ns "$1" timeout 4 socat -T2 - "TCP4:$3:$4,connect-timeout=2" </dev/null 2>/dev/null || true) ;;
    tcp6) out=$(in_ns "$1" timeout 4 socat -T2 - "TCP6:[$3]:$4,connect-timeout=2" </dev/null 2>/dev/null || true) ;;
    udp) # a datagram can be lost on a busy runner: only repeated silence counts as blocked
      for _ in 1 2 3; do
        out=$(echo q | in_ns "$1" timeout 4 socat -t1 - "UDP4:$3:$4" 2>/dev/null || true)
        case $out in *can-ok*) break ;; esac
      done
      ;;
  esac
  case $out in *can-ok*) echo REACHED ;; *) echo BLOCKED ;; esac
}
fails=0
pass() { echo "ok   $*"; }
fail() {
  echo "FAIL $*"
  fails=$((fails + 1))
}
expect() { # REACHED|BLOCKED description ns proto host port
  local want=$1 what=$2 got
  shift 2
  got=$(verdict "$@")
  if [ "$got" = "$want" ]; then pass "$want  $what"; else fail "want $want got $got: $what ($*)"; fi
}
assert() { # description command...
  local what=$1
  shift
  if "$@" >"$work/last.log" 2>&1; then
    pass "$what"
  else
    fail "$what"
    cat "$work/last.log"
  fi
}
refute() {
  local what=$1
  shift
  if "$@" >"$work/last.log" 2>&1; then
    fail "$what"
    cat "$work/last.log"
  else
    pass "$what"
  fi
}

sleep 3 # listeners and IPv6 duplicate address detection
LL=$(in_ns $NODE ip -6 -o addr show dev cni0 scope link | awk '{sub("/.*","",$4); print $4; exit}')
[ -n "$LL" ] || { echo "FAIL no link-local address on cni0"; exit 1; }

matrix() { # open|enforced: what the restricted paths should look like
  local m=BLOCKED
  [ "$1" = open ] && m=REACHED
  for port in 22 6443 10250; do expect $m "module pod -> node cni0 address :$port" $MOD tcp 10.42.0.1 "$port"; done
  for port in 22 6443 10250; do expect $m "module pod -> node public address :$port" $MOD tcp 192.0.2.1 "$port"; done
  expect $m "module pod -> kubernetes Service (DNAT to node:6443)" $MOD tcp 10.43.0.1 443
  expect $m "module pod -> node TCP :53" $MOD tcp 10.42.0.1 53
  expect $m "module pod -> node UDP :53" $MOD udp 10.42.0.1 53
  expect $m "module pod -> DNAT'd host resolver (UDP)" $MOD udp 198.18.0.53 53
  expect $m "module pod -> the DNAT target port directly" $MOD tcp 192.0.2.1 5353
  expect $m "system pod -> the DNAT target port directly" $SYS tcp 192.0.2.1 5353
  expect $m "module pod -> node IPv6 link-local :22" $MOD tcp6 "$LL%eth0" 22
  expect $m "module pod -> cloud metadata" $MOD tcp 169.254.169.254 80
  expect $m "system pod -> cloud metadata" $SYS tcp 169.254.169.254 80
  expect $m "module pod -> cloud metadata over IPv6" $MOD tcp6 fd00:ec2::254 80
  expect $m "system pod -> cloud metadata over IPv6" $SYS tcp6 fd00:ec2::254 80
  expect $m "system pod -> node SSH" $SYS tcp 10.42.0.1 22
  expect $m "system pod -> kubelet" $SYS tcp 192.0.2.1 10250
  # never affected
  expect REACHED "system pod -> kubernetes Service" $SYS tcp 10.43.0.1 443
  expect REACHED "system pod -> API server" $SYS tcp 192.0.2.1 6443
  expect REACHED "system pod -> node UDP :53" $SYS udp 10.42.0.1 53
  expect REACHED "system pod -> DNAT'd host resolver (UDP)" $SYS udp 198.18.0.53 53
  expect REACHED "system pod -> DNAT'd host resolver (TCP)" $SYS tcp 198.18.0.53 53
  expect REACHED "module pod -> internet" $MOD tcp 203.0.113.9 443
  expect REACHED "system pod -> module pod" $SYS tcp 10.42.0.20 8080
  expect REACHED "node -> pod (kubelet probes)" $NODE tcp 10.42.0.20 8080
  expect REACHED "operator SSH from outside" $EXT tcp 192.0.2.1 22
  expect REACHED "node loopback -> API server (the ssh tunnel)" $NODE tcp 127.0.0.1 6443
  local got
  got=$(in_ns $EXT timeout 4 socat -T2 - "TCP4:192.0.2.1:22,connect-timeout=2,bind=10.42.99.1" </dev/null 2>/dev/null || true)
  case $got in
    *can-ok*) pass "operator SSH from a pod-CIDR address on the outside interface" ;;
    *) fail "operator SSH from a pod-CIDR address on the outside interface" ;;
  esac
}

echo "== control: before the rules everything is reachable"
matrix open
expect BLOCKED "probe sanity: a closed TCP port" $MOD tcp 10.42.0.1 9
expect BLOCKED "probe sanity: a closed UDP port" $MOD udp 10.42.0.1 9
[ "$fails" -eq 0 ] || { echo "controls failed: nothing below would mean anything"; exit 1; }

echo "== install"
mkdir -p "$work/conf" "$work/cni"
assert "install succeeds" can install
assert "units written" test -s "$work/units/can-firewall.service" -a -s "$work/units/can-firewall-sync.service"
assert "units enabled" grep -q '^enable can-firewall.service can-firewall-sync.service' "$work/systemctl.log"
refute "sync unit does not wait for k3s" grep -q '^After=.*k3s' "$work/units/can-firewall-sync.service"
matrix enforced
assert "check reports ok" can check

echo "== idempotence: loading again changes nothing"
listing() { in_ns $NODE nft list table inet can_isolation | sed 's/counter packets [0-9]* bytes [0-9]*/counter/'; }
before=$(listing)
assert "apply again" can apply
assert "apply a third time" can apply
assert "same ruleset" test "$before" = "$(listing)"
assert "install again" can install

echo "== a boot (empty kernel ruleset, API not up yet) already blocks pods"
in_ns $NODE nft delete table inet can_isolation
refute "check fails with the rules gone" can check
cp "$work/last.log" "$work/check.out"
assert "and says they are missing" grep -qx rules=missing "$work/check.out"
touch "$work/list.fail"
assert "apply without an API" can apply
expect BLOCKED "module pod -> API server after boot-time apply" $MOD tcp 192.0.2.1 6443
expect BLOCKED "system pod not yet trusted at boot" $SYS tcp 192.0.2.1 6443
expect REACHED "operator SSH after boot-time apply" $EXT tcp 192.0.2.1 22
rm "$work/list.fail"

echo "== the sync loop follows pods and restores a deleted table"
fw env CAN_FW_INTERVAL=3600 bash "$FW" run >"$work/run.log" 2>&1 &
loop=$!
pids+=("$loop")
sleep 12 # past the first burst
expect REACHED "system pod trusted by the loop" $SYS tcp 192.0.2.1 6443
pods 'kube-system||false|10.42.0.20|'
touch "$work/cni/10.42.0.20" # an address allocation is the trigger, long before the interval
sleep 4
expect REACHED "new system pod address trusted within seconds" $MOD tcp 192.0.2.1 6443
expect BLOCKED "old address no longer trusted" $SYS tcp 192.0.2.1 6443
sleep 10
touch "$work/list.fail"
rm "$work/cni/10.42.0.20"
sleep 4
expect REACHED "set kept while pods cannot be listed" $MOD tcp 192.0.2.1 6443
rm "$work/list.fail"
pods "$SYS_POD"
touch "$work/cni/10.42.0.10"
sleep 4
touch "$work/nft.fail"
touch "$work/cni/10.42.0.12"
sleep 4
assert "an nft failure is reported as such" grep -q 'nft could not update the trusted set' "$work/run.log"
assert "only the real listing failure was reported as one" test "$(grep -c 'cannot list pods' "$work/run.log")" -eq 1
rm "$work/nft.fail"
sleep 10
in_ns $NODE nft add table inet can_isolation '{ flags dormant; }'
expect REACHED "control: a dormant table filters nothing" $MOD tcp 192.0.2.1 6443
sleep 7
expect BLOCKED "dormant table woken by the loop" $MOD tcp 192.0.2.1 6443
in_ns $NODE nft delete table inet can_isolation
sleep 8
touch "$work/cni/10.42.0.11"
sleep 4
expect BLOCKED "table restored by the loop" $MOD tcp 192.0.2.1 6443
expect REACHED "system pod trusted again" $SYS tcp 192.0.2.1 6443
pkill -P "$loop" 2>/dev/null || true
ip netns pids $NODE | while read -r p; do
  if tr '\0' ' ' <"/proc/$p/cmdline" 2>/dev/null | grep -q 'can-firewall.sh run'; then kill "$p" 2>/dev/null || true; fi
done

echo "== a ruleset that does not load changes nothing"
before=$(listing)
refute "apply with a broken ruleset fails" fw env CAN_FW_IFACES='"cni0", }{' bash "$FW" apply
assert "previous rules intact" test "$before" = "$(listing)"
expect BLOCKED "still enforcing" $MOD tcp 192.0.2.1 6443

echo "== disable, then a failing install rolls back"
assert "disable" can disable
matrix open
touch "$work/readyz.fail"
refute "install fails when verification fails" can install
refute "rolled back: no table" in_ns $NODE nft list table inet can_isolation
rm "$work/readyz.fail"
expect REACHED "module pod reaches the API again after rollback" $MOD tcp 192.0.2.1 6443
assert "uninstall" can uninstall
touch "$work/systemctl.fail"
refute "install fails when systemd fails" can install
refute "rolled back: no table" in_ns $NODE nft list table inet can_isolation
refute "rolled back: sync not enabled" test -e "$work/enabled/can-firewall-sync.service"
refute "rolled back: sync not running" test -e "$work/active/can-firewall-sync.service"
refute "rolled back: no unit files" test -e "$work/units/can-firewall.service"
pods
refute "install refuses to run with no system pods" can install
refute "nothing loaded" in_ns $NODE nft list table inet can_isolation
pods "$SYS_POD"

echo "== a failed re-install leaves the working installation as it was"
assert "install" can install
echo '# the previous release' >>"$work/units/can-firewall-sync.service"
cp "$work/units/can-firewall-sync.service" "$work/sync.unit.before"
# A previous table that differs from what this script loads: it must come back as it was.
in_ns $NODE nft add rule inet can_isolation input tcp dport 9999 counter drop
before=$(listing)
states() { (cd "$work" && ls enabled active); }
had=$(states)
touch "$work/readyz.fail"
refute "re-install fails when verification fails" can install
assert "previous table restored" test "$before" = "$(listing)"
rm "$work/readyz.fail"
touch "$work/systemctl.fail"
refute "re-install fails when systemd fails" can install
assert "the units were never disabled" test -z "$(grep '^disable' <(tail -n 12 "$work/systemctl.log"))"
assert "units still enabled and running" test "$had" = "$(states)"
assert "previous unit file restored" cmp "$work/sync.unit.before" "$work/units/can-firewall-sync.service"
expect BLOCKED "still enforcing after the failed re-install" $MOD tcp 192.0.2.1 6443
# The previous script is not kept, so a unit the rollback restarts loads the installed script's rules
# over the table it put back (docs/node-isolation.md says so).
in_ns $NODE nft add rule inet can_isolation input tcp dport 9999 counter drop
touch "$work/systemctl.execs" "$work/systemctl.fail"
refute "re-install fails with units that run their command" fw env FW_UNDER_TEST="$FW" bash "$FW" install
refute "the restarted unit loaded the installed rules" grep -q 'dport 9999' <(listing)
rm "$work/systemctl.execs"
expect BLOCKED "still enforcing after that rollback" $MOD tcp 192.0.2.1 6443
# Enabled but stopped stays enabled and stopped.
rm "$work/active/can-firewall-sync.service"
touch "$work/systemctl.fail"
refute "re-install fails again" can install
assert "sync still enabled" test -e "$work/enabled/can-firewall-sync.service"
refute "sync still stopped" test -e "$work/active/can-firewall-sync.service"
assert "install" can install
assert "check ok" can check

echo "== check notices what is not enforcing"
checkout() { can check >"$work/check.out" 2>&1; }
notices() { # description expected-line
  refute "$1: check fails" checkout
  assert "$1: reports $2" grep -qx "$2" "$work/check.out"
}
rm "$work/active/can-firewall-sync.service"
notices "sync service stopped" service=inactive
touch "$work/active/can-firewall-sync.service"
for u in can-firewall.service can-firewall-sync.service; do
  rm "$work/enabled/$u"
  notices "$u not enabled" service=inactive
  touch "$work/enabled/$u"
done
touch "$work/failed/can-firewall.service"
notices "can-firewall.service failed" service=failed
rm "$work/failed/can-firewall.service"
assert "check ok with the services back" can check
refute "pod bridge missing: check fails" fw env CAN_FW_POD_BRIDGE=nope0 bash "$FW" check
cp "$work/last.log" "$work/check.out"
assert "pod bridge missing: reported" grep -qx bridge=missing "$work/check.out"

handle() { # chain, pattern of one rule
  in_ns $NODE nft -a list chain inet can_isolation "$1" | grep -- "$2" | sed -n 's/.*# handle \([0-9]*\)$/\1/p'
}
loosen() { # description chain pattern replacement-rule
  local h
  h=$(handle "$2" "$3")
  if [ -z "$h" ]; then
    fail "$1: no rule matches $3"
    return
  fi
  in_ns $NODE nft replace rule inet can_isolation "$2" handle "$h" "$4"
  notices "$1" rules=changed
  assert "$1: apply repairs it" can apply
  assert "$1: check ok after repair" can check
}
loosen "reply rule replaced by a bare accept" input 'ct direction reply' 'accept'
loosen "icmpv6 rule swapped" input 'icmpv6 type' 'meta l4proto ipv6-icmp accept'
loosen "IPv6 metadata drop turned into accept" forward 'ip6 daddr' 'ip6 daddr { fd00:ec2::254, fe80::a9fe:a9fe } counter accept'
loosen "flannel.1 removed from the first input rule" input 'iifname' 'iifname != "cni0" accept'

in_ns $NODE nft add table inet can_isolation '{ flags dormant; }'
expect REACHED "control: dormant table" $MOD tcp 192.0.2.1 6443
notices "dormant table" rules=dormant
assert "apply clears the dormant flag" can apply
expect BLOCKED "enforcing again" $MOD tcp 192.0.2.1 6443
assert "check ok after waking" can check
# The addresses already trusted survive an apply while pods cannot be listed.
touch "$work/list.fail"
assert "apply without an API on a loaded table" can apply
expect REACHED "system pod still trusted" $SYS tcp 192.0.2.1 6443
rm "$work/list.fail"

echo "== addresses claimed by two pods, and terminating pods"
trusted() { in_ns $NODE nft list set inet can_isolation trusted4 | tr -d '\n' | grep -q "[ {]$1[ ,}]"; }
pods "$SYS_POD" 'mod-x|app|false|10.42.0.10|'
assert "sync" can sync
refute "an address a module pod also reports is not trusted" trusted 10.42.0.10
expect BLOCKED "contested address" $SYS tcp 192.0.2.1 6443
refute "a contested address fails check" can check
cp "$work/last.log" "$work/check.out"
assert "and is reported" grep -q '^problem=1 system pod address.*also reported by another pod' "$work/check.out"
pods "$SYS_POD" 'kube-system||true|10.42.0.10|' 'mod-x|app|true|10.42.0.10|'
assert "sync" can sync
assert "a host network pod claims nothing" trusted 10.42.0.10
pods "$SYS_POD" 'can-system|other|false|10.42.0.10|'
assert "sync" can sync
refute "another can-system pod contests the address" trusted 10.42.0.10
pods 'can-system|can-core|false|10.42.0.10|' 'mod-x|app|false|10.42.0.20|2000-01-01T00:00:00Z'
assert "sync" can sync
assert "can-core is trusted" trusted 10.42.0.10
refute "a module pod is not" trusted 10.42.0.20
pods 'kube-system||false|10.42.0.10|2999-01-01T00:00:00Z' 'kube-system||false|10.42.0.11|2000-01-01T00:00:00Z'
assert "sync" can sync
assert "terminating within its grace period: trusted" trusted 10.42.0.10
refute "terminating past its grace period: not trusted" trusted 10.42.0.11
pods "$SYS_POD" 'mod-x|app|false|10.42.0.10|2000-01-01T00:00:00Z'
assert "sync" can sync
refute "a listed terminating module pod still contests" trusted 10.42.0.10
pods "$SYS_POD"
assert "sync" can sync

echo "== check notices a stale trusted address and an inserted accept"
in_ns $NODE nft add element inet can_isolation trusted4 '{ 10.42.0.20 }'
refute "stale address fails check" can check
assert "sync" can sync
assert "check ok again" can check
in_ns $NODE nft insert rule inet can_isolation input accept
refute "an inserted accept fails check" can check
assert "apply repairs it" can apply
assert "check ok after repair" can check
printf 'mod-evil\nops\n' >"$work/conf/trusted-namespaces"
pods "$SYS_POD" 'mod-evil|app|false|10.42.0.20|' 'ops|x|false|10.42.0.30|'
assert "sync" can sync
refute "a module namespace listed as trusted is ignored" trusted 10.42.0.20
assert "a listed namespace is trusted" trusted 10.42.0.30

echo
if [ "$fails" -ne 0 ]; then
  echo "FAILED: $fails"
  [ -f "$work/run.log" ] && cat "$work/run.log"
  exit 1
fi
echo "all node isolation checks passed"
