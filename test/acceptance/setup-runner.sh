#!/usr/bin/env bash
# Prepares a THROWAWAY machine (a CI runner VM) for the acceptance journey (specs/058-acceptance-journey):
# the machine is both the operator's computer and, over ssh, the server `can up` installs onto.
#
# IT CHANGES THE MACHINE: root ssh-reachable key, a test CA in the system trust store, /etc/hosts,
# firewall rules, and the journey that follows installs k3s, destroys it and installs it again.
# Never run it on a computer you care about.
#
#   CAN_ACCEPTANCE_DESTROYS_THIS_MACHINE=1 test/acceptance/setup-runner.sh DIR
#
# DIR must be outside the repository. It receives:
#   dist/     what a user has after building: can and the image tarballs
#   company/  the empty directory the journey starts from
#   standins/ keys, certificates and the images of the stand-ins (SMTP sink)
#   out/      the report and logs
set -euo pipefail

[ "${CAN_ACCEPTANCE_DESTROYS_THIS_MACHINE:-}" = 1 ] || {
	echo "setup-runner: refusing: this installs and destroys a server on THIS machine." >&2
	echo "Set CAN_ACCEPTANCE_DESTROYS_THIS_MACHINE=1 on a throwaway VM only." >&2
	exit 2
}
dir=${1:?usage: setup-runner.sh DIR}
repo=$(cd "$(dirname "$0")/../.." && pwd)
mkdir -p "$dir"
dir=$(cd "$dir" && pwd)
case "$dir/" in "$repo"/*) echo "setup-runner: $dir is inside the repository" >&2; exit 2 ;; esac

DOMAIN=acceptance.test
SINK_HOST=smtp-sink.acceptance-sink.svc.cluster.local
# An S3 gateway over a local directory stands in for the backup bucket.
S3_IMAGE=docker.io/versity/versitygw:v1.11.0@sha256:8319a530d08d537426e2f855bae5280709795dc73a3f0c347973ef915d6343c5

mkdir -p "$dir/dist" "$dir/company" "$dir/standins/s3/certs" "$dir/standins/s3/data" "$dir/standins/sink" "$dir/out"
chmod 700 "$dir/standins"
cd "$dir/standins"

ip=$(ip -4 route get 1.1.1.1 | awk '{for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit }}')
[ -n "$ip" ] || { echo "setup-runner: no primary IPv4 address" >&2; exit 1; }
echo "$ip" >host

echo "--- ssh: the ssh provider reaches this machine as $USER with sudo"
command -v sshd >/dev/null || [ -x /usr/sbin/sshd ] || { sudo apt-get update -qq && sudo apt-get install -y -qq openssh-server; }
ssh-keygen -q -t ed25519 -N '' -f id_ed25519
mkdir -p "$HOME/.ssh"
cat id_ed25519.pub >>"$HOME/.ssh/authorized_keys"
chmod 700 "$HOME/.ssh"
chmod 600 "$HOME/.ssh/authorized_keys"
chmod go-w "$HOME" # sshd refuses a key in a group-writable home
sudo systemctl start ssh || sudo systemctl start sshd
ssh -i id_ed25519 -o BatchMode=yes -o StrictHostKeyChecking=accept-new "$USER@$ip" sudo -n true

echo "--- a test CA, trusted by this machine only"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 -subj "/CN=acceptance test CA" \
	-addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" -keyout ca.key -out ca.crt
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj "/CN=$DOMAIN" -keyout tls.key -out tls.csr
printf 'subjectAltName=DNS:%s,DNS:*.%s,DNS:%s,IP:%s\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n' \
	"$DOMAIN" "$DOMAIN" "$SINK_HOST" "$ip" >tls.ext
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 2 -extfile tls.ext -out tls.crt
sudo install -m 0644 ca.crt /usr/local/share/ca-certificates/can-acceptance.crt
sudo update-ca-certificates >/dev/null

echo "--- DNS stand-in: /etc/hosts"
names="$DOMAIN"
for h in admin work support help s3; do names="$names $h.$DOMAIN"; done
echo "$ip $names" | sudo tee -a /etc/hosts >/dev/null

echo "--- ACME stand-in: no certificate request leaves this machine (Traefik keeps the test certificate)"
sudo nft add table inet can_acceptance_noacme
sudo nft add chain inet can_acceptance_noacme forward '{ type filter hook forward priority -20; policy accept; }'
for a in $(getent ahosts acme-v02.api.letsencrypt.org | awk '{print $1}' | sort -u); do
	case "$a" in
	*:*) sudo nft add rule inet can_acceptance_noacme forward ip6 daddr "$a" counter reject ;;
	*) sudo nft add rule inet can_acceptance_noacme forward ip daddr "$a" counter reject ;;
	esac
done
sudo nft list table inet can_acceptance_noacme

echo "--- backup bucket stand-in: S3 over TLS on $ip:9000"
s3_key="ACCEPT$(openssl rand -hex 7)"
s3_secret=$(openssl rand -hex 20)
printf 'AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\n' "$s3_key" "$s3_secret" >s3.env
chmod 600 s3.env
cp tls.crt tls.key s3/certs/
# --region auto: the region `can up` signs with when none is given.
docker run -d --name can-acceptance-s3 --network host \
	-e "ROOT_ACCESS_KEY=$s3_key" -e "ROOT_SECRET_KEY=$s3_secret" \
	-v "$PWD/s3/certs:/certs:ro" -v "$PWD/s3/data:/data" \
	"$S3_IMAGE" --port :9000 --region auto --cert /certs/tls.crt --key /certs/tls.key posix /data >/dev/null
for _ in $(seq 1 60); do
	curl -sS -o /dev/null --max-time 2 "https://s3.$DOMAIN:9000/" && break
	sleep 1
done
curl -sS -o /dev/null --max-time 5 "https://s3.$DOMAIN:9000/" || { docker logs can-acceptance-s3; exit 1; }

echo "--- SMTP sink image"
(cd "$repo" && CGO_ENABLED=0 go build -trimpath -o "$dir/standins/sink/smtpsink" ./dev/compose/smtpsink)
printf 'FROM scratch\nCOPY smtpsink /smtpsink\nUSER 65532:65532\nENTRYPOINT ["/smtpsink"]\n' >sink/Dockerfile
docker build -q -t docker.io/library/can-acceptance-sink:test sink >/dev/null
docker save -o sink.tar docker.io/library/can-acceptance-sink:test

echo "--- what the user has: can (make build) and the images (make images)"
cd "$repo"
start=$(date +%s)
make build >/dev/null
echo "build_binaries_seconds=$(($(date +%s) - start))" >"$dir/out/build.env"
install -m 0755 bin/can "$dir/dist/can"
start=$(date +%s)
# The images this journey needs; `make images` without ONLY builds every module's.
ENGINE=docker ONLY="can-core can-static can-forms can-work" sh scripts/build-images.sh "$dir/dist/images"
echo "build_images_seconds=$(($(date +%s) - start))" >>"$dir/out/build.env"
# The preflight of `can up` wants free disk; the build cache is not needed again.
docker builder prune -af >/dev/null
df -h /var/lib
echo "setup-runner: ready in $dir (server $ip)"
