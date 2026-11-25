package provider

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"text/template"
)

// DefaultK3sVersion is the pinned k3s release the bootstrap installs.
// UNVERIFIED on a real host: E00 (Phase 0 measurements) confirms the pin and
// the flag set. Change it here only together with the golden file.
const DefaultK3sVersion = "v1.33.5+k3s1"

// Default k3s network CIDRs (SPEC.md section 3.1).
const (
	DefaultPodCIDR     = "10.42.0.0/16"
	DefaultServiceCIDR = "10.43.0.0/16"
)

// ImageTarball is a container image archive pre-seeded into k3s's
// agent/images directory so containerd imports it at start.
type ImageTarball struct {
	Name   string // file name, e.g. can-core.tar.zst
	URL    string // https URL
	SHA256 string // 64 hex chars; verified before use
}

// BootstrapOptions parameterise the generated host bootstrap script.
type BootstrapOptions struct {
	K3sVersion    string
	NodeName      string
	SSHPort       int
	OperatorCIDR  string // optional: restrict SSH to this IP or CIDR
	PodCIDR       string
	ServiceCIDR   string
	SwapMB        int // 0 = 1024
	ImageTarballs []ImageTarball
}

var (
	reVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+\+k3s[0-9]+$`)
	reNode    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reTarName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,80}\.(tar|tar\.gz|tar\.zst)$`)
	reURL     = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~%/+-]*)?$`)
	reSHA     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func (o *BootstrapOptions) normalise() error {
	if o.K3sVersion == "" {
		o.K3sVersion = DefaultK3sVersion
	}
	if o.PodCIDR == "" {
		o.PodCIDR = DefaultPodCIDR
	}
	if o.ServiceCIDR == "" {
		o.ServiceCIDR = DefaultServiceCIDR
	}
	if o.SSHPort == 0 {
		o.SSHPort = 22
	}
	if o.SwapMB == 0 {
		o.SwapMB = 1024
	}
	if o.NodeName == "" {
		o.NodeName = "can"
	}
	// Every value below is interpolated into a shell script: allow-list only.
	switch {
	case !reVersion.MatchString(o.K3sVersion):
		return fmt.Errorf("k3s version %q: want a pinned release like v1.33.5+k3s1", o.K3sVersion)
	case !reNode.MatchString(o.NodeName):
		return fmt.Errorf("node name %q is not a valid hostname label", o.NodeName)
	case o.SSHPort < 1 || o.SSHPort > 65535:
		return fmt.Errorf("ssh port %d out of range", o.SSHPort)
	case o.SwapMB < 128 || o.SwapMB > 16384:
		return fmt.Errorf("swap size %d MB out of range 128..16384", o.SwapMB)
	}
	for _, c := range []struct{ name, v string }{{"pod CIDR", o.PodCIDR}, {"service CIDR", o.ServiceCIDR}} {
		if _, err := netip.ParsePrefix(c.v); err != nil {
			return fmt.Errorf("%s %q: %w", c.name, c.v, err)
		}
	}
	if o.OperatorCIDR != "" {
		if _, err := netip.ParsePrefix(o.OperatorCIDR); err != nil {
			a, aerr := netip.ParseAddr(o.OperatorCIDR)
			if aerr != nil {
				return fmt.Errorf("operator address %q is not an IP or CIDR", o.OperatorCIDR)
			}
			o.OperatorCIDR = netip.PrefixFrom(a, a.BitLen()).String()
		}
	}
	seen := map[string]bool{}
	for _, t := range o.ImageTarballs {
		switch {
		case !reTarName.MatchString(t.Name):
			return fmt.Errorf("image tarball name %q is not allowed", t.Name)
		case !reURL.MatchString(t.URL):
			return fmt.Errorf("image tarball URL %q must be a plain https URL", t.URL)
		case !reSHA.MatchString(t.SHA256):
			return fmt.Errorf("image tarball %s: sha256 must be 64 lowercase hex characters", t.Name)
		case seen[t.Name]:
			return fmt.Errorf("duplicate image tarball %q", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}

// Bootstrap generates the idempotent host bootstrap script as text.
func Bootstrap(o BootstrapOptions) (string, error) {
	if err := o.normalise(); err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := bootstrapTmpl.Execute(&b, o); err != nil {
		return "", err
	}
	return b.String(), nil
}

// MaxCloudInitBytes is the user-data size limit of the supported clouds
// (Hetzner: 32 KiB).
const MaxCloudInitBytes = 32 * 1024

// CloudInit wraps the bootstrap script as #cloud-config user-data. The script
// is base64-encoded so no quoting issue can reach YAML.
func CloudInit(o BootstrapOptions) (string, error) {
	script, err := Bootstrap(o)
	if err != nil {
		return "", err
	}
	ci := "#cloud-config\n" +
		"write_files:\n" +
		"  - path: /usr/local/sbin/can-bootstrap.sh\n" +
		"    permissions: '0700'\n" +
		"    owner: root:root\n" +
		"    encoding: b64\n" +
		"    content: " + base64.StdEncoding.EncodeToString([]byte(script)) + "\n" +
		"runcmd:\n" +
		"  - [bash, -c, \"/usr/local/sbin/can-bootstrap.sh >>/var/log/can-bootstrap.log 2>&1\"]\n"
	if len(ci) > MaxCloudInitBytes {
		return "", fmt.Errorf("cloud-init user-data is %d bytes, limit %d (use fewer image tarballs)", len(ci), MaxCloudInitBytes)
	}
	return ci, nil
}

// BootstrapDoneMarker is written last by the bootstrap script. Its presence
// is how `can up` (and status) knows a cloud-init run finished.
const BootstrapDoneMarker = "/var/lib/can/bootstrap.done"

// UninstallScript removes k3s and can-core state but leaves the box (and its
// firewall and swap) alone, as `make down` promises for the ssh provider.
func UninstallScript() string {
	return `#!/usr/bin/env bash
set -euo pipefail
echo "CAN_STEP: uninstall-k3s"
if [ -x /usr/local/bin/k3s-uninstall.sh ]; then
  /usr/local/bin/k3s-uninstall.sh
else
  echo "k3s is not installed; nothing to uninstall"
fi
echo "CAN_STEP: remove-can-state"
rm -rf /var/lib/can /etc/can
echo "CAN_UNINSTALL_DONE"
`
}

var bootstrapTmpl = template.Must(template.New("bootstrap").Parse(`#!/usr/bin/env bash
# Generated by can (epic E06). Idempotent: running it again converges.
# Hand edits are lost; change the generator in core/provider/bootstrap.go.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

K3S_VERSION='{{.K3sVersion}}'
SSH_PORT='{{.SSHPort}}'
POD_CIDR='{{.PodCIDR}}'
SERVICE_CIDR='{{.ServiceCIDR}}'
SWAP_MB='{{.SwapMB}}'

config_new=''
installer=''
trap 'rm -f "$config_new" "$installer"' EXIT

step() { echo "CAN_STEP: $1"; }
warn() { echo "CAN_WARN: $1"; }

if [ "$(id -u)" -ne 0 ]; then
  echo "CAN_ERROR: must run as root" >&2
  exit 1
fi

step swap
mem_mb=$(awk '/^MemTotal:/ {print int($2/1024)}' /proc/meminfo)
if [ "$mem_mb" -lt 4096 ] && [ -z "$(swapon --show --noheadings)" ]; then
  if [ ! -f /swapfile ]; then
    fallocate -l "${SWAP_MB}M" /swapfile || dd if=/dev/zero of=/swapfile bs=1M count="$SWAP_MB" status=none
    chmod 600 /swapfile
    mkswap /swapfile >/dev/null
  fi
  swapon /swapfile
  grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
  echo 'vm.swappiness=10' > /etc/sysctl.d/90-can-swap.conf
  sysctl -q -p /etc/sysctl.d/90-can-swap.conf
fi

step firewall
if ! command -v ufw >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq
    apt-get install -y -qq ufw
  else
    warn "ufw is not available; configure the host firewall manually (allow 22, 80, 443, ${POD_CIDR}, ${SERVICE_CIDR})"
  fi
fi
if command -v ufw >/dev/null 2>&1; then
  ufw default deny incoming
  ufw default allow outgoing
  ufw default allow routed
{{- if .OperatorCIDR}}
  ufw allow from '{{.OperatorCIDR}}' to any port "$SSH_PORT" proto tcp
{{- else}}
  ufw allow "${SSH_PORT}/tcp"
{{- end}}
  ufw allow 80/tcp
  ufw allow 443/tcp
  # k3s needs pod and service traffic to reach the host (API server, CoreDNS, kubelet)
  ufw allow from "$POD_CIDR"
  ufw allow from "$SERVICE_CIDR"
  ufw allow in on cni0
  ufw allow in on flannel.1
  # 6443 (k3s API) is deliberately not opened: can reaches it through an SSH tunnel
  ufw --force enable
fi

step k3s-config
install -d -m 0755 /etc/rancher/k3s
rm -f /etc/rancher/k3s/config.yaml.??????   # leftovers of an interrupted earlier run
config_new=$(mktemp /etc/rancher/k3s/config.yaml.XXXXXX)
cat > "$config_new" <<'CAN_K3S_CONFIG'
node-name: {{.NodeName}}
write-kubeconfig-mode: "0600"
secrets-encryption: true
disable:
  - metrics-server
disable-cloud-controller: true
cluster-cidr: {{.PodCIDR}}
service-cidr: {{.ServiceCIDR}}
kubelet-arg:
  - fail-swap-on=false
CAN_K3S_CONFIG
chmod 0644 "$config_new"
mv "$config_new" /etc/rancher/k3s/config.yaml
config_new=''
install -d -m 0700 /var/lib/can
want_config=$(sha256sum /etc/rancher/k3s/config.yaml | awk '{print $1}')
applied_config=$(cat /var/lib/can/k3s-config.applied 2>/dev/null || true)
{{if .ImageTarballs}}
step images
install -d -m 0755 /var/lib/rancher/k3s/agent/images
fetch_image() {
  local name="$1" url="$2" sha="$3" dest tmp
  dest="/var/lib/rancher/k3s/agent/images/${name}"
  if [ -f "$dest" ] && echo "${sha}  ${dest}" | sha256sum -c --status -; then
    return 0
  fi
  tmp="${dest}.part"
  curl -fsSL --retry 5 -o "$tmp" "$url"
  echo "${sha}  ${tmp}" | sha256sum -c --status - || { rm -f "$tmp"; echo "CAN_ERROR: checksum mismatch for ${name}" >&2; return 1; }
  mv "$tmp" "$dest"
}
fetch_pids=()
{{- range .ImageTarballs}}
fetch_image '{{.Name}}' '{{.URL}}' '{{.SHA256}}' &
fetch_pids+=("$!")
{{- end}}
{{else}}
step images
# no image tarballs configured: containerd pulls images when first needed
install -d -m 0755 /var/lib/rancher/k3s/agent/images
fetch_pids=()
{{end}}
step k3s
current=$(k3s --version 2>/dev/null | awk 'NR==1 {print $3}' || true)
if [ "$current" != "$K3S_VERSION" ]; then
  # The installer script is downloaded over TLS but not hash-verified (the
  # version it installs is pinned, the script itself is not).
  installer=$(mktemp)
  curl -fsSL --retry 5 -o "$installer" https://get.k3s.io
  INSTALL_K3S_VERSION="$K3S_VERSION" sh "$installer"
  rm -f "$installer"
  installer=''
elif [ "$applied_config" != "$want_config" ]; then
  # Same version but config.yaml differs from what k3s last started with (or a
  # previous run died before recording it): restart so it takes effect.
  systemctl restart k3s
fi

for pid in ${fetch_pids[@]+"${fetch_pids[@]}"}; do
  wait "$pid"
done

step ready
for _ in $(seq 1 90); do
  nodes=$(k3s kubectl get nodes --no-headers 2>/dev/null || true)
  case "$nodes" in
    *" Ready"*) break ;;
  esac
  sleep 2
done
case "$(k3s kubectl get nodes --no-headers 2>/dev/null || true)" in
  *" Ready"*) ;;
  *) echo "CAN_ERROR: node did not become Ready" >&2; exit 1 ;;
esac

echo "$want_config" > /var/lib/can/k3s-config.applied

step backup-placeholder
install -d -m 0700 /var/lib/can/dumps /etc/can/backup.d
cat > /etc/can/backup.d/README <<'CAN_BACKUP_README'
Placeholder for the restic + systemd timer backup (epic E08).
Dumps are written to /var/lib/can/dumps; E08 installs the timer and restic unit here.
CAN_BACKUP_README

step complete
echo "$K3S_VERSION" > /var/lib/can/bootstrap.done
echo "CAN_BOOTSTRAP_DONE ${K3S_VERSION}"
`))

// String forms used in docs and plan output.
func (o BootstrapOptions) String() string {
	return strings.TrimSpace(fmt.Sprintf("k3s %s node %s ssh %d", o.K3sVersion, o.NodeName, o.SSHPort))
}
