package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMarkerRoundTripAndFreshness(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "COMPLETE")
	now := time.Unix(1_800_000_000, 0)
	if _, err := ReadMarker(p); err == nil {
		t.Fatal("missing marker must error")
	}
	if err := WriteMarker(p, Marker{RunID: "r1", FinishedAt: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMarker(p)
	if err != nil || m.RunID != "r1" {
		t.Fatalf("%v %+v", err, m)
	}
	if err := m.Check(now, 24*time.Hour); err != nil {
		t.Errorf("fresh marker rejected: %v", err)
	}
	if err := m.Check(now.Add(30*time.Hour), 24*time.Hour); err == nil {
		t.Error("stale marker accepted")
	}
	if err := (Marker{RunID: "x", FinishedAt: now.Add(2 * time.Hour)}).Check(now, 24*time.Hour); err == nil {
		t.Error("future marker accepted")
	}
}

func TestMarkerMalformed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "COMPLETE")
	for _, body := range []string{"", "run_id=a\n", "finished_at=abc\nrun_id=a\n", "run_id=\nfinished_at=5\n"} {
		_ = os.WriteFile(p, []byte(body), 0o600)
		if _, err := ReadMarker(p); err == nil {
			t.Errorf("body %q accepted", body)
		}
	}
}
