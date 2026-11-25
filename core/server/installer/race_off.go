//go:build !race

package installer

// raceEnabled is true when the binary is built with the race detector.
const raceEnabled = false

var raceChildEnv []string
