package installer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// releaseMAC authenticates the release record with the module's signing secret (the one can-core
// keeps in its own database and never writes into the module namespace's chart objects), so someone
// who can write Secrets in the namespace cannot forge a record that rollback would replay.
func releaseMAC(s reconcile.Spec, raw []byte) (string, error) {
	if s.HMACSecret == "" {
		return "", errors.New("no module signing secret to authenticate the release record")
	}
	m := hmac.New(sha256.New, []byte(s.HMACSecret))
	m.Write([]byte("can-release/v1\x00" + s.ID + "\x00"))
	m.Write(raw)
	return hex.EncodeToString(m.Sum(nil)), nil
}
