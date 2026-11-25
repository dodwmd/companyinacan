//go:build race

package installer

// raceEnabled is true when the binary is built with the race detector.
const raceEnabled = true

// The race runtime sleeps atexit_sleep_ms (default 1 s) before every exit so that late reports can
// be flushed; each render child would pay that second. The child is short-lived and its stderr is
// not interesting, so the sleep is dropped in race builds (never in production builds).
var raceChildEnv = []string{"GORACE=atexit_sleep_ms=0"}
