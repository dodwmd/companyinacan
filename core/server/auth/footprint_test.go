package auth

import (
	"runtime"
	"testing"
)

// The timing-equalisation hash must cost what a real one costs (same parameters and lengths)
// without being computed at start-up.
func TestDummyHashMatchesCurrentParams(t *testing.T) {
	saved := currentParams
	t.Cleanup(func() { currentParams = saved })
	currentParams = DefaultParams

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	d := dummyHash()
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("building the dummy hash allocated %d bytes: argon2 must not run at start-up", grew)
	}
	p, err := parse(d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Memory != DefaultParams.Memory || p.Time != DefaultParams.Time || p.Threads != DefaultParams.Threads ||
		p.KeyLen != DefaultParams.KeyLen || len(p.salt) != int(DefaultParams.SaltLen) || NeedsRehash(d) {
		t.Fatalf("dummy hash parameters differ from the current ones: %s", d)
	}
	if ok, err := VerifyPassword("dummy password for timing equalisation", d); err != nil || ok {
		t.Fatalf("the dummy hash must verify nothing: ok=%v err=%v", ok, err)
	}
}

// NewService must install such a dummy: an empty or malformed one would make verifying an unknown
// user name return at once and tell names apart by timing.
func TestNewServiceInstallsAValidDummy(t *testing.T) {
	saved := currentParams
	t.Cleanup(func() { currentParams = saved })
	currentParams = DefaultParams

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s := NewService(nil, nil, Options{})
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("NewService allocated %d bytes: argon2 must not run at start-up", grew)
	}
	p, err := parse(s.dummy)
	if err != nil {
		t.Fatalf("dummy %q: %v", s.dummy, err)
	}
	if p.Memory != DefaultParams.Memory || p.Time != DefaultParams.Time || p.Threads != DefaultParams.Threads ||
		len(p.salt) != int(DefaultParams.SaltLen) || len(p.key) != int(DefaultParams.KeyLen) {
		t.Fatalf("dummy has other parameters than a real hash: %s", s.dummy)
	}
	// Verifying it really runs argon2 with its memory cost, as for a known user.
	runtime.ReadMemStats(&before)
	ok, err := VerifyPassword("any password at all", s.dummy)
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; ok || err != nil || got < uint64(DefaultParams.Memory)*1024 {
		t.Fatalf("verifying the dummy: ok=%v err=%v allocated=%d", ok, err, got)
	}
}

// The security parameters are a floor: memory savings must never come from lowering them.
func TestDefaultParamsNotLowered(t *testing.T) {
	p := DefaultParams
	if p.Memory < 19*1024 || p.Time < 2 || p.Threads < 1 || p.KeyLen < 32 || p.SaltLen < 16 {
		t.Fatalf("argon2id parameters below the OWASP minimum: %+v", p)
	}
}
