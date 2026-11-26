//go:build !unix

package canbin

// ownedByUser cannot be told apart here: there is no file owner to compare. A seam for tests.
var ownedByUser = func(string) bool { return true }
