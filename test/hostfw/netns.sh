#!/usr/bin/env bash
# Exercises core/provider/hostfw/can-firewall.sh with real nftables in throw-away network
# namespaces: a "node" (bridge cni0, kube-proxy-like DNAT, ufw-like accepts), a system pod, a
# module pod, an outside operator and an upstream with a fake cloud metadata address.
# Every BLOCKED verdict has a REACHED control with the same probe, so a broken probe cannot pass.
#
# Needs root, nft, ip and socat. Run it only in a disposable environment (CI runner or a
# container with its own network namespace); CAN_HOSTFW_THROWAWAY=1 confirms that.
set -euo pipefail

[ "${CAN_HOSTFW_THROWAWAY:-}" = 1 ] || {
  echo "refusing to run: set CAN_HOSTFW_THROWAWAY=1 in a disposable environment" >&2
  exit 2
}
[ "$(id -u)" -eq 0 ] || { echo "needs root" >&2; exit 2; }
for c in nft ip socat timeout nsenter pkill; do
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

# What the other parties on a k3s host load: kube-proxy's DNAT of the kubernetes Service, an
# accept EARLIER than ours and a ufw-like input chain after it. None may override our drop.
in_ns $NODE nft -f - <<'NFT'
table ip proxy_like {
  chain prerouting {
    type nat hook prerouting priority -100;
    ip daddr 10.43.0.1 tcp dport 443 dnat to 192.0.2.1:6443
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
for p in 22 53 6443 10250; do serve $NODE "$p"; done
nsenter --net="/var/run/netns/$NODE" socat -T2 UDP4-RECVFROM:53,fork SYSTEM:'echo can-ok' >/dev/null 2>&1 &
pids+=("$!")
serve $MOD 8080
serve $UP 80
serve $UP 443

# The cluster the script asks: one system pod, no can-core; readyz answers unless told otherwise.
cat >"$work/kubectl" <<STUB
#!/usr/bin/env bash
case "\$*" in
  *--raw=/readyz*) [ ! -e "$work/readyz.fail" ] ;;
  *"get pods"*)
    [ ! -e "$work/list.fail" ] || exit 1
    case "\$*" in *"-n kube-system"*) cat "$work/kube-system.pods" ;; esac
    ;;
esac
STUB
cat >"$work/systemctl" <<STUB
#!/usr/bin/env bash
echo "\$*" >>"$work/systemctl.log"
case "\$1" in restart) [ ! -e "$work/systemctl.fail" ] ;; esac
STUB
chmod +x "$work/kubectl" "$work/systemctl"
# The hostNetwork and address-less entries must never be trusted.
printf 'false|10.42.0.10\ntrue|10.42.0.20\n|\n' >"$work/kube-system.pods"

fw() {
  in_ns $NODE env CAN_FW_KUBECTL="$work/kubectl" CAN_FW_SYSTEMCTL="$work/systemctl" CAN_FW_UNIT_DIR="$work/units" \
    CAN_FW_SELF="$work/can-firewall" CAN_FW_CONF_DIR="$work/conf" CAN_FW_CNI_STATE="$work/cni" CAN_FW_WAIT=6 "$@"
}
can() { fw bash "$FW" "$@"; }

# ---- probes ---------------------------------------------------------------------------------
verdict() { # ns proto host port
  local out=''
  case $2 in
    tcp) out=$(in_ns "$1" timeout 4 socat -T2 - "TCP4:$3:$4,connect-timeout=2" </dev/null 2>/dev/null || true) ;;
    tcp6) out=$(in_ns "$1" timeout 4 socat -T2 - "TCP6:[$3]:$4,connect-timeout=2" </dev/null 2>/dev/null || true) ;;
    udp) out=$(echo q | in_ns "$1" timeout 4 socat -t1 - "UDP4:$3:$4" 2>/dev/null || true) ;;
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
  expect $m "module pod -> node IPv6 link-local :22" $MOD tcp6 "$LL%eth0" 22
  expect $m "module pod -> cloud metadata" $MOD tcp 169.254.169.254 80
  expect $m "system pod -> cloud metadata" $SYS tcp 169.254.169.254 80
  expect $m "system pod -> node SSH" $SYS tcp 10.42.0.1 22
  expect $m "system pod -> kubelet" $SYS tcp 192.0.2.1 10250
  # never affected
  expect REACHED "system pod -> kubernetes Service" $SYS tcp 10.43.0.1 443
  expect REACHED "system pod -> API server" $SYS tcp 192.0.2.1 6443
  expect REACHED "system pod -> node UDP :53" $SYS udp 10.42.0.1 53
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
printf 'false|10.42.0.20\n' >"$work/kube-system.pods"
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
printf 'false|10.42.0.10\n' >"$work/kube-system.pods"
touch "$work/cni/10.42.0.10"
sleep 4
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
touch "$work/systemctl.fail"
refute "install fails when systemd fails" can install
refute "rolled back: no table" in_ns $NODE nft list table inet can_isolation
rm "$work/systemctl.fail"
printf '' >"$work/kube-system.pods"
refute "install refuses to run with no system pods" can install
refute "nothing loaded" in_ns $NODE nft list table inet can_isolation
printf 'false|10.42.0.10\n' >"$work/kube-system.pods"

echo "== check notices a stale trusted address and a loosened chain"
assert "install" can install
in_ns $NODE nft add element inet can_isolation trusted4 '{ 10.42.0.20 }'
refute "stale address fails check" can check
assert "sync" can sync
assert "check ok again" can check
in_ns $NODE nft insert rule inet can_isolation input accept
refute "an inserted accept fails check" can check
assert "apply repairs it" can apply
assert "check ok after repair" can check
printf 'mod-evil\nops\n' >"$work/conf/trusted-namespaces"
assert "a module namespace listed as trusted is ignored" can check

echo
if [ "$fails" -ne 0 ]; then
  echo "FAILED: $fails"
  [ -f "$work/run.log" ] && cat "$work/run.log"
  exit 1
fi
echo "all node isolation checks passed"
