package launch

import (
	"context"
	"fmt"
	"strings"

	"github.com/dodwmd/companyinacan/core/provider"
)

// ImageLoader puts the images where the cluster can run them. progress receives one human line
// per step (already free of secrets).
type ImageLoader interface {
	Load(ctx context.Context, set ImageSet, progress func(string)) error
}

// ImagesDir is where k3s imports container image archives from when it starts, so uploaded
// tarballs also survive a reboot.
const ImagesDir = "/var/lib/rancher/k3s/agent/images"

// SSHImageLoader ships local tarballs to the server over the provider Runner, checks their sha256
// again on the server, and imports them into k3s's containerd without restarting it. A release
// manifest has nothing to upload: the server pulls those images by digest.
type SSHImageLoader struct{ Host provider.Runner }

// Load implements ImageLoader.
func (l SSHImageLoader) Load(ctx context.Context, set ImageSet, progress func(string)) error {
	up := set.Upload()
	if len(up) == 0 {
		return nil
	}
	if _, err := l.Host.Run(ctx, "install -d -m 0755 "+ImagesDir+"\n"); err != nil {
		return fmt.Errorf("prepare %s: %w", ImagesDir, err)
	}
	for _, img := range up {
		b, err := img.Read()
		if err != nil {
			return err
		}
		dest := ImagesDir + "/" + img.File
		progress(fmt.Sprintf("uploading %s (%d MiB) as %s", img.Name, len(b)>>20, img.Ref))
		if err := l.Host.Copy(ctx, dest, b, "0600"); err != nil {
			return fmt.Errorf("upload %s: %w", img.Name, err)
		}
		script := fmt.Sprintf(`set -eu
got=$(sha256sum '%[1]s' | cut -d' ' -f1)
if [ "$got" != '%[2]s' ]; then echo "CAN_SHA_MISMATCH $got"; exit 3; fi
k3s ctr -n k8s.io images import '%[1]s'
echo CAN_IMAGE_OK
`, dest, img.SHA256)
		out, err := l.Host.Run(ctx, script)
		if strings.Contains(out, "CAN_SHA_MISMATCH") {
			return fmt.Errorf("image %s: the file arrived on the server with a different sha256 (%s, expected %s); the upload was corrupted, nothing was imported", img.Name, strings.TrimSpace(strings.TrimPrefix(lineWith(out, "CAN_SHA_MISMATCH"), "CAN_SHA_MISMATCH")), img.SHA256)
		}
		if err != nil || !strings.Contains(out, "CAN_IMAGE_OK") {
			return fmt.Errorf("import %s into k3s: %v: %s", img.Name, err, strings.TrimSpace(out))
		}
		progress(fmt.Sprintf("imported %s (sha256 verified on the server)", img.Name))
	}
	return nil
}

func lineWith(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}
