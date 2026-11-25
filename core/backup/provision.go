package backup

import "fmt"

// The restic the host backup runs: the upstream release binary, verified against the digests of
// that release's SHA256SUMS. Distribution packages are older than the 0.17 the tool relies on
// (exit code 10 for "no repository"). Bump version and both digests together in a reviewed change.
const ResticVersion = "0.18.0"

// ResticSHA256 maps the release architecture to the digest of restic_<version>_linux_<arch>.bz2.
var ResticSHA256 = map[string]string{
	"amd64": "98f6dd8bf5b59058d04bfd8dab58e196cc2a680666ccee90275a3b722374438e",
	"arm64": "ce18179c25dc5f2e33e3c233ba1e580f9de1a4566d2977e8d9600210363ec209",
}

// HostToolsScript is the shell script that makes sure the backup script's tools exist on the
// server: sqlite3 (for the k3s datastore copy) and restic 0.17 or newer, installed to dest from
// the pinned release when missing or older. It exits non-zero (CAN_NO_RESTIC) if it cannot.
func HostToolsScript(dest string) string {
	return fmt.Sprintf(`set -eu
if ! command -v sqlite3 >/dev/null 2>&1 || ! command -v bzip2 >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq && apt-get install -y -qq sqlite3 bzip2 >/dev/null || true
  fi
fi
command -v sqlite3 >/dev/null 2>&1 || { echo "CAN_NO_RESTIC: sqlite3 is missing" >&2; exit 4; }
have=$(restic version 2>/dev/null | awk '{print $2; exit}' || true)
case "$have" in
  0.1[7-9].*|0.[2-9][0-9].*|[1-9]*) exit 0 ;;
esac
case "$(uname -m)" in
  x86_64) arch=amd64 sum=%[3]s ;;
  aarch64|arm64) arch=arm64 sum=%[4]s ;;
  *) echo "CAN_NO_RESTIC: no pinned restic for $(uname -m)" >&2; exit 4 ;;
esac
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL --retry 5 -o "$tmp/restic.bz2" "https://github.com/restic/restic/releases/download/v%[2]s/restic_%[2]s_linux_$arch.bz2"
echo "$sum  $tmp/restic.bz2" | sha256sum -c --status - || { echo "CAN_NO_RESTIC: checksum mismatch for restic %[2]s" >&2; exit 4; }
bzip2 -dc "$tmp/restic.bz2" >"$tmp/restic"
install -m 0755 "$tmp/restic" %[1]s
`, shq(dest), ResticVersion, ResticSHA256["amd64"], ResticSHA256["arm64"])
}
