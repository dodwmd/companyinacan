//go:build !candevstack

package runtime

// devBuild is false in every release build: the development stack mode cannot be switched on.
var devBuild = false
