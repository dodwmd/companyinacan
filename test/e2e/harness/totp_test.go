package harness

import (
	"testing"
	"time"
)

// RFC 6238 appendix B: the SHA-1 secret "12345678901234567890" (base32 below). The 8-digit codes
// are 94287082 at T=59, 07081804 at T=1111111109, 14050471 at 1111111111, 89005924 at
// 1234567890 and 69279037 at 2000000000; the 6-digit code is their last six digits.
func TestTOTPRFC6238Vectors(t *testing.T) {
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	for _, c := range []struct {
		at   int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}} {
		got, err := TOTP(secret, time.Unix(c.at, 0))
		if err != nil || got != c.want {
			t.Errorf("TOTP at %d = %q, %v; want %q", c.at, got, err, c.want)
		}
	}
}

func TestTOTPAcceptsSpacedLowercaseSecret(t *testing.T) {
	a, err := TOTP("gezd gnbv gy3t qojq gezd gnbv gy3t qojq", time.Unix(59, 0))
	if err != nil || a != "287082" {
		t.Fatalf("got %q, %v", a, err)
	}
}

func TestParseTOTPSecret(t *testing.T) {
	out := "Password reset.\nNew TOTP secret (shown once): B4WPZVVUV4MF7JWQIQOJARCCQUPKI57R\nProvisioning URI: otpauth://x?secret=B4WPZVVUV4MF7JWQIQOJARCCQUPKI57R\n"
	if got := ParseTOTPSecret(out); got != "B4WPZVVUV4MF7JWQIQOJARCCQUPKI57R" {
		t.Fatalf("got %q", got)
	}
	if got := ParseTOTPSecret("nothing here"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestTOTPRejectsGarbage(t *testing.T) {
	if _, err := TOTP("not*base32!", time.Now()); err == nil {
		t.Fatal("expected an error")
	}
}
