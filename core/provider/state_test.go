package provider

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStoreRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	s, err := st.Load()
	if err != nil || s.Version != StateVersion || s.Server != nil {
		t.Fatalf("empty load: %+v %v", s, err)
	}
	now := time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC)
	if err := st.Mutate(func(s *State) error {
		s.Provider = "ssh"
		s.Server = &ServerState{IP: "203.0.113.7"}
		s.Mark("ssh.bootstrap", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s, _ = st.Load()
	if s.Server.IP != "203.0.113.7" || !s.Done("ssh.bootstrap") || s.Steps["ssh.bootstrap"] != "2026-11-25T01:02:03Z" {
		t.Fatalf("round trip: %+v", s)
	}
	fi, err := os.Stat(filepath.Join(dir, ".can", "state.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode: %v %v", fi, err)
	}
}

func TestMutateErrorWritesNothing(t *testing.T) {
	st := NewStore(t.TempDir())
	boom := errors.New("boom")
	if err := st.Mutate(func(s *State) error { s.Provider = "x"; return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.Path); !os.IsNotExist(err) {
		t.Fatalf("state written despite error: %v", err)
	}
}

func TestConcurrentMutationsAreSerialised(t *testing.T) {
	st := NewStore(t.TempDir())
	var wg sync.WaitGroup
	const n = 25
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.Mutate(func(s *State) error { s.SSHKeyID++; return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	s, _ := st.Load()
	if s.SSHKeyID != n {
		t.Fatalf("lost updates: %d != %d", s.SSHKeyID, n)
	}
}

func TestTryMutateFailsFastWhileLocked(t *testing.T) {
	st := NewStore(t.TempDir())
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- st.Mutate(func(*State) error { close(entered); <-release; return nil })
	}()
	<-entered
	err := st.TryMutate(func(*State) error { return nil })
	close(release)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsNewerAndCorruptState(t *testing.T) {
	st := NewStore(t.TempDir())
	_ = os.MkdirAll(filepath.Dir(st.Path), 0o700)
	b, _ := json.Marshal(State{Version: StateVersion + 1})
	_ = os.WriteFile(st.Path, b, 0o600)
	if _, err := st.Load(); err == nil {
		t.Fatal("want error for newer state version")
	}
	_ = os.WriteFile(st.Path, []byte("{nope"), 0o600)
	if _, err := st.Load(); err == nil {
		t.Fatal("want error for corrupt state")
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	st := NewStore(t.TempDir())
	_ = st.Mutate(func(s *State) error { s.Provider = "ssh"; return nil })
	if err := st.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := st.Remove(); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}
