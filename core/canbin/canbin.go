// Package canbin puts a `can` binary on the company's server (specs/060-can-on-server).
//
// The host's backup script and erasure sync timer run /usr/local/bin/can, so `can up`,
// `can backup install` and `can restore` install one that matches the running tool and the
// server's architecture, verify it by checksum on the server and replace it atomically.
package canbin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Build identifies a build of `can`: what `can version` reports.
type Build struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date,omitempty"`
}

var reField = regexp.MustCompile(`^[A-Za-z0-9._+:-]{0,80}$`)

// clean keeps text read from a server short and free of anything but version characters.
func clean(s string) string {
	if !reField.MatchString(s) {
		return "invalid"
	}
	return s
}

// ParseVersion reads the output of `can version --output json`.
func ParseVersion(out string) (Build, error) {
	var b Build
	i := strings.Index(out, "{")
	if i < 0 || json.NewDecoder(strings.NewReader(out[i:])).Decode(&b) != nil || b.Version == "" {
		return Build{}, fmt.Errorf("not the output of `can version --output json`")
	}
	return Build{Version: clean(b.Version), Commit: clean(b.Commit), Date: clean(b.Date)}, nil
}

func (b Build) hasVersion() bool { return b.Version != "" && b.Version != "dev" }
func (b Build) hasCommit() bool  { return b.Commit != "" && b.Commit != "none" }

// String is "v1.2.3 (abc123)".
func (b Build) String() string {
	v := b.Version
	if v == "" {
		v = "unknown"
	}
	if b.hasCommit() {
		return v + " (" + b.Commit + ")"
	}
	return v
}

// Comparison says how the server's build relates to this one.
type Comparison int

// The comparisons.
const (
	Unknown Comparison = iota // neither a version nor a commit to compare
	Same
	Differs
)

// Compare compares what both builds know. Commits are compared by prefix: they are abbreviated
// to different lengths by `make build` and the release pipeline.
func Compare(server, local Build) Comparison {
	res := Unknown
	if server.hasVersion() && local.hasVersion() {
		if server.Version != local.Version {
			return Differs
		}
		res = Same
	}
	if server.hasCommit() && local.hasCommit() {
		if !strings.HasPrefix(server.Commit, local.Commit) && !strings.HasPrefix(local.Commit, server.Commit) {
			return Differs
		}
		res = Same
	}
	return res
}
