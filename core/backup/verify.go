package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// UsableSnapshots keeps the snapshots a backup produced and considers good:
// tagged "can" and not tagged "inconsistent". Status, verify and restore all
// use the same rule.
func UsableSnapshots(in []Snapshot) []Snapshot {
	var out []Snapshot
	for _, s := range in {
		if slices.Contains(s.Tags, "can") && !slices.Contains(s.Tags, "inconsistent") {
			out = append(out, s)
		}
	}
	return out
}

func parseSnapshots(b []byte) ([]Snapshot, error) {
	var s []Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("restic snapshots output: %w", err)
	}
	return s, nil
}

var manifestLineRe = regexp.MustCompile(`^([0-9a-f]{64}) [ *](.+)$`)

// ParseManifest reads MANIFEST.sha256 (sha256sum format, `./relative` paths)
// into relpath -> hex digest. Paths that escape the dumps directory are rejected.
func ParseManifest(b []byte) (map[string]string, error) {
	m := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		g := manifestLineRe.FindStringSubmatch(l)
		if g == nil {
			return nil, fmt.Errorf("manifest: malformed line %q", l)
		}
		p := path.Clean(g[2])
		if p == "." || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
			return nil, fmt.Errorf("manifest: unsafe path %q", g[2])
		}
		m[p] = g[1]
	}
	return m, nil
}

// VerifyOptions configure `can backup verify`.
type VerifyOptions struct {
	DumpsDir     string
	HostStateDir string // holds the manifest (default Settings.HostStateDir)
	Sample       int    // number of dump files to restore and hash (default 3)
	TempDir      string // parent for the scratch restore (default os.TempDir)
	ReadData     bool   // restic check --read-data (slow)
}

// VerifyReport is the outcome of Verify.
type VerifyReport struct {
	Snapshot   string   `json:"snapshot"`
	CheckOK    bool     `json:"check_ok"`
	Sampled    []string `json:"sampled"`
	Mismatched []string `json:"mismatched,omitempty"`
	Missing    []string `json:"missing,omitempty"`
}

// Verify runs restic check, restores a sample of the latest snapshot's dump
// files into a scratch directory and compares their SHA-256 with the manifest
// that was itself taken at backup time. The scratch directory is always removed.
func Verify(ctx context.Context, r ResticAPI, o VerifyOptions) (*VerifyReport, error) {
	rep := &VerifyReport{}
	if o.Sample < 1 {
		o.Sample = 3
	}
	if err := r.Check(ctx, o.ReadData); err != nil {
		return rep, fmt.Errorf("restic check failed: %w", err)
	}
	rep.CheckOK = true
	snaps, err := r.Snapshots(ctx)
	if err != nil {
		return rep, err
	}
	if snaps = UsableSnapshots(snaps); len(snaps) == 0 {
		return rep, errors.New("repository has no usable snapshots (tagged can and not inconsistent)")
	}
	rep.Snapshot = snaps[0].ID
	tmp, err := os.MkdirTemp(o.TempDir, "can-verify-")
	if err != nil {
		return rep, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	if o.HostStateDir == "" {
		o.HostStateDir = DefaultSettings().HostStateDir
	}
	manPath := path.Join(o.HostStateDir, ManifestName)
	if err := r.Restore(ctx, rep.Snapshot, tmp, []string{manPath}); err != nil {
		return rep, fmt.Errorf("restoring manifest: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, filepath.FromSlash(manPath))) //nolint:gosec // inside our scratch dir
	if err != nil {
		return rep, fmt.Errorf("snapshot %s has no %s: %w", rep.Snapshot, ManifestName, err)
	}
	man, err := ParseManifest(raw)
	if err != nil {
		return rep, err
	}
	if len(man) == 0 {
		return rep, errors.New("manifest lists no files")
	}
	names := make([]string, 0, len(man))
	for n := range man {
		names = append(names, n)
	}
	sort.Strings(names)
	step := float64(len(names)) / float64(min(o.Sample, len(names)))
	var includes []string
	for i := 0; i < min(o.Sample, len(names)); i++ {
		n := names[int(float64(i)*step)]
		rep.Sampled = append(rep.Sampled, n)
		includes = append(includes, path.Join(o.DumpsDir, n))
	}
	if err := r.Restore(ctx, rep.Snapshot, tmp, includes); err != nil {
		return rep, fmt.Errorf("restoring sample: %w", err)
	}
	for _, n := range rep.Sampled {
		sum, err := hashFile(filepath.Join(tmp, filepath.FromSlash(path.Join(o.DumpsDir, n))))
		switch {
		case err != nil:
			rep.Missing = append(rep.Missing, n)
		case sum != man[n]:
			rep.Mismatched = append(rep.Mismatched, n)
		}
	}
	if len(rep.Missing)+len(rep.Mismatched) > 0 {
		return rep, fmt.Errorf("verify failed: %d missing, %d mismatched of %d sampled", len(rep.Missing), len(rep.Mismatched), len(rep.Sampled))
	}
	return rep, nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // inside our scratch dir
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// StatusReport is what `can backup status` prints.
type StatusReport struct {
	Enabled   bool      `json:"enabled"`
	RunID     string    `json:"run_id,omitempty"`
	MarkerAt  time.Time `json:"marker_at,omitempty"`
	Snapshots int       `json:"snapshots"`
	Latest    string    `json:"latest,omitempty"`
	LatestAt  time.Time `json:"latest_at,omitempty"`
	Problems  []string  `json:"problems,omitempty"`
}

// Status inspects the completion marker and the repository. r == nil means no
// repository is configured: backups are off.
func Status(ctx context.Context, r ResticAPI, dumpsDir string, now time.Time, maxAge time.Duration) StatusReport {
	var rep StatusReport
	if r == nil {
		rep.Problems = append(rep.Problems, "backups are OFF: no bucket configured (set backup options in company.yaml and run make up)")
		return rep
	}
	rep.Enabled = true
	if m, err := ReadMarker(filepath.Join(dumpsDir, MarkerName)); err != nil {
		rep.Problems = append(rep.Problems, err.Error())
	} else {
		rep.RunID, rep.MarkerAt = m.RunID, m.FinishedAt
		if err := m.Check(now, maxAge); err != nil {
			rep.Problems = append(rep.Problems, err.Error())
		}
	}
	snaps, err := r.Snapshots(ctx)
	if err != nil {
		rep.Problems = append(rep.Problems, "cannot list snapshots: "+err.Error())
		return rep
	}
	if snaps = UsableSnapshots(snaps); len(snaps) == 0 {
		rep.Problems = append(rep.Problems, "repository has no usable snapshots (tagged can and not inconsistent)")
		return rep
	}
	rep.Snapshots = len(snaps)
	rep.Latest, rep.LatestAt = snaps[0].ID, snaps[0].Time
	if age := now.Sub(snaps[0].Time); age > maxAge+2*time.Hour {
		rep.Problems = append(rep.Problems, fmt.Sprintf("latest snapshot is %s old", age.Round(time.Minute)))
	}
	return rep
}
