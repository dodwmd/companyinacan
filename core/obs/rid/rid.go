// Package rid is the request-id half of core/obs/log, split out so binaries that only need ids
// (can-static) do not link log/slog and the redaction regular expressions.
package rid

import (
	"context"
	"math/rand/v2"
)

type ctxKey struct{}

// With returns ctx carrying the request id.
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the id stored by With, or "".
func From(ctx context.Context) string { s, _ := ctx.Value(ctxKey{}).(string); return s }

// MaxLen bounds an accepted inbound X-Request-Id.
const MaxLen = 64

// New returns inbound when it is a safe id (1 to 64 characters of A-Z a-z 0-9 . _ -),
// otherwise a fresh random 16-hex-digit id. Anything else a client sends is never echoed into
// logs or response headers.
func New(inbound string) string {
	if n := len(inbound); n > 0 && n <= MaxLen {
		ok := true
		for i := 0; i < n; i++ {
			c := inbound[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
				ok = false
				break
			}
		}
		if ok {
			return inbound
		}
	}
	// Request ids correlate log lines; they are not secrets, so the cheap generator is enough.
	const digits = "0123456789abcdef"
	n := rand.Uint64()
	var b [16]byte
	for i := 15; i >= 0; i-- {
		b[i] = digits[n&0xf]
		n >>= 4
	}
	return string(b[:])
}
