package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func noSleep(context.Context, time.Duration) error { return nil }

func init() {
	// Cheap parameters keep the suite fast; production uses DefaultParams.
	TestingParams()
}

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=") {
		t.Fatalf("not a PHC argon2id string: %s", h)
	}
	if ok, _ := VerifyPassword("correct horse battery staple", h); !ok {
		t.Fatal("right password rejected")
	}
	if ok, _ := VerifyPassword("wrong", h); ok {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery staple")
	if h == h2 {
		t.Fatal("salt must differ between hashes")
	}
	for _, bad := range []string{"", "plain", "$argon2id$v=19$m=x,t=1,p=1$AAAA$AAAA", "$bcrypt$x"} {
		if ok, err := VerifyPassword("x", bad); ok || err == nil {
			t.Fatalf("malformed hash %q: ok=%v err=%v", bad, ok, err)
		}
	}
	if _, err := HashPassword("short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("short password allowed: %v", err)
	}
}

func TestDefaultParamsMeetOWASPMinimum(t *testing.T) {
	if DefaultParams.Memory < 19*1024 || DefaultParams.Time < 2 || DefaultParams.Threads < 1 {
		t.Fatalf("%+v", DefaultParams)
	}
}

func TestNeedsRehash(t *testing.T) {
	h, _ := HashPassword("correct horse battery staple")
	if NeedsRehash(h) {
		t.Fatal("fresh hash flagged")
	}
	old := currentParams
	currentParams.Time++
	defer func() { currentParams = old }()
	if !NeedsRehash(h) {
		t.Fatal("weaker-than-policy hash not flagged")
	}
}

// RFC 6238 Appendix B test vectors (8 digits).
func TestTOTPRFC6238Vectors(t *testing.T) {
	keys := map[Algorithm]string{
		SHA1:   "12345678901234567890",
		SHA256: "12345678901234567890123456789012",
		SHA512: "1234567890123456789012345678901234567890123456789012345678901234",
	}
	vec := []struct {
		unix int64
		alg  Algorithm
		code string
	}{
		{59, SHA1, "94287082"}, {1111111109, SHA1, "07081804"}, {1111111111, SHA1, "14050471"},
		{1234567890, SHA1, "89005924"}, {2000000000, SHA1, "69279037"}, {20000000000, SHA1, "65353130"},
		{59, SHA256, "46119246"}, {1111111109, SHA256, "68084774"}, {1111111111, SHA256, "67062674"},
		{1234567890, SHA256, "91819424"}, {2000000000, SHA256, "90698825"}, {20000000000, SHA256, "77737706"},
		{59, SHA512, "90693936"}, {1111111109, SHA512, "25091201"}, {1111111111, SHA512, "99943326"},
		{1234567890, SHA512, "93441116"}, {2000000000, SHA512, "38618901"}, {20000000000, SHA512, "47863826"},
	}
	for _, v := range vec {
		got := Code([]byte(keys[v.alg]), v.alg, 8, 30, time.Unix(v.unix, 0))
		if got != v.code {
			t.Errorf("%s T=%d: got %s want %s", v.alg, v.unix, got, v.code)
		}
	}
}

func TestTOTPValidateWindowAndStep(t *testing.T) {
	secret := NewTOTPSecret()
	key, err := DecodeTOTPSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_015, 0)
	cur := Code(key, SHA1, 6, 30, now)
	if step, ok := ValidateTOTP(secret, cur, now); !ok || step != now.Unix()/30 {
		t.Fatalf("current code: step=%d ok=%v", step, ok)
	}
	prev := Code(key, SHA1, 6, 30, now.Add(-30*time.Second))
	if _, ok := ValidateTOTP(secret, prev, now); !ok {
		t.Fatal("previous step (clock skew) rejected")
	}
	next := Code(key, SHA1, 6, 30, now.Add(30*time.Second))
	if _, ok := ValidateTOTP(secret, next, now); !ok {
		t.Fatal("next step (clock skew) rejected")
	}
	old := Code(key, SHA1, 6, 30, now.Add(-90*time.Second))
	if _, ok := ValidateTOTP(secret, old, now); ok {
		t.Fatal("code three steps old accepted")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", cur + "0"} {
		if _, ok := ValidateTOTP(secret, bad, now); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestTOTPURI(t *testing.T) {
	u := TOTPURI("Company in a Can", "admin@example.com", "JBSWY3DPEHPK3PXP")
	for _, want := range []string{"otpauth://totp/", "secret=JBSWY3DPEHPK3PXP", "issuer=Company+in+a+Can", "digits=6", "period=30"} {
		if !strings.Contains(u, want) {
			t.Errorf("%s missing %q", u, want)
		}
	}
}

// --- service -----------------------------------------------------------------------------

type env struct {
	svc *Service
	db  *store.DB
	now *time.Time
	pw  string
	sec string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Unix(1_700_000_000, 0)
	db.Now = func() time.Time { return now }
	e := &env{db: db, now: &now, pw: "correct horse battery staple"}
	e.sec = NewTOTPSecret()
	h, _ := HashPassword(e.pw)
	if _, err := db.CreateAdmin(context.Background(), "root", h, e.sec); err != nil {
		t.Fatal(err)
	}
	pol := DefaultPolicy
	pol.AttemptsPerMin = 1000 // the per-minute limiter has its own test
	pol.HardLock = true
	e.svc = NewService(db, audit.New(db), Options{Now: func() time.Time { return now }, Policy: pol, MinLoginTime: time.Nanosecond, Sleep: noSleep})
	return e
}

func (e *env) code(t *testing.T) string {
	key, _ := DecodeTOTPSecret(e.sec)
	return Code(key, SHA1, 6, 30, *e.now)
}

func TestLoginSuccessCreatesSessionAndAudits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Token == "" || res.Session.CSRF == "" {
		t.Fatalf("%+v", res)
	}
	sess, user, err := e.svc.Authenticate(ctx, res.Token)
	if err != nil || user.Username != "root" || sess.CSRF != res.Session.CSRF {
		t.Fatalf("%+v %+v %v", sess, user, err)
	}
	if st, _ := e.db.SessionByHash(ctx, res.Token); st.IDHash != "" {
		t.Fatal("raw token must not be the stored key")
	}
	recs, _ := audit.New(e.db).List(ctx, 0, 10)
	if len(recs) != 1 || recs[0].Action != "admin.login" {
		t.Fatalf("%+v", recs)
	}
}

func TestLoginRejectsBadPasswordBadCodeUnknownUserWithSameError(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cases := []LoginRequest{
		{Username: "root", Password: "wrong password!", Code: e.code(t), IP: "1.1.1.1"},
		{Username: "root", Password: e.pw, Code: "000000", IP: "1.1.1.1"},
		{Username: "ghost", Password: e.pw, Code: e.code(t), IP: "1.1.1.1"},
		{Username: "root", Password: e.pw, Code: "", IP: "1.1.1.1"},
	}
	for i, c := range cases {
		if _, err := e.svc.Login(ctx, c); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

func TestTOTPCodeCannotBeReplayed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	code := e.code(t)
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: code, IP: "1.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: code, IP: "1.1.1.1"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("replayed code accepted: %v", err)
	}
}

func TestLockoutAfterRepeatedFailuresThenExpires(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	bad := LoginRequest{Username: "root", Password: "wrong password!", Code: "000000", IP: "9.9.9.9"}
	for i := 0; i < DefaultPolicy.MaxUserFailures; i++ {
		if _, err := e.svc.Login(ctx, bad); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Now even the right credentials are refused, and the credentials are not evaluated.
	good := LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "9.9.9.9"}
	_, err := e.svc.Login(ctx, good)
	var le *LockedError
	if !errors.As(err, &le) || le.RetryAfter <= 0 {
		t.Fatalf("want lockout, got %v", err)
	}
	*e.now = e.now.Add(DefaultPolicy.LockFor + time.Second)
	good.Code = e.code(t)
	if _, err := e.svc.Login(ctx, good); err != nil {
		t.Fatalf("lock should have expired: %v", err)
	}
	// success clears the counter
	for i := 0; i < DefaultPolicy.MaxUserFailures-1; i++ {
		_, _ = e.svc.Login(ctx, bad)
	}
	*e.now = e.now.Add(31 * time.Second)
	good.Code = e.code(t)
	if _, err := e.svc.Login(ctx, good); err != nil {
		t.Fatalf("counter not reset by success: %v", err)
	}
}

func TestLockoutPerIPAcrossUsernames(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < DefaultPolicy.MaxIPFailures; i++ {
		_, _ = e.svc.Login(ctx, LoginRequest{Username: "u" + string(rune('a'+i%20)), Password: "whatever-long", Code: "000000", IP: "7.7.7.7"})
	}
	_, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "7.7.7.7"})
	var le *LockedError
	if !errors.As(err, &le) {
		t.Fatalf("ip should be locked: %v", err)
	}
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "6.6.6.6"}); err != nil {
		t.Fatalf("other ip must still work: %v", err)
	}
}

func TestSessionExpiryLogoutAndCSRF(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res, _ := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "1.1.1.1"})
	if !CSRFValid(res.Session, res.Session.CSRF) || CSRFValid(res.Session, "nope") || CSRFValid(res.Session, "") {
		t.Fatal("csrf comparison wrong")
	}
	if err := e.svc.Logout(ctx, res.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Authenticate(ctx, res.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("logged-out session valid: %v", err)
	}
	*e.now = e.now.Add(31 * time.Second)
	res, _ = e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "1.1.1.1"})
	*e.now = e.now.Add(DefaultSessionTTL + time.Second)
	if _, _, err := e.svc.Authenticate(ctx, res.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired session valid: %v", err)
	}
	if _, _, err := e.svc.Authenticate(ctx, "garbage"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("garbage token accepted")
	}
}

func TestCookieAttributes(t *testing.T) {
	c := SessionCookie("tok", time.Unix(2_000_000_000, 0), true)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Name != CookieName {
		t.Fatalf("%+v", c)
	}
	if SessionCookie("t", time.Now(), false).Secure {
		t.Fatal("secure must be configurable for plain-HTTP dev")
	}
	if cc := ClearCookie(true); cc.MaxAge >= 0 || cc.Value != "" {
		t.Fatalf("%+v", cc)
	}
}
