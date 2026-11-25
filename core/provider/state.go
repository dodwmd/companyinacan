package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// StateVersion is the schema version of .can/state.json.
const StateVersion = 1

// State is persisted in .can/state.json. It never holds secrets.
type State struct {
	Version  int    `json:"version"`
	Provider string `json:"provider"`

	// Server is the machine the company runs on.
	Server *ServerState `json:"server,omitempty"`
	// Provider-side objects created by `can up` (Hetzner ids).
	SSHKeyID int64 `json:"ssh_key_id,omitempty"`
	// SSHKeyOwned is true when can uploaded the key (and so may delete it).
	SSHKeyOwned bool  `json:"ssh_key_owned,omitempty"`
	FirewallID  int64 `json:"firewall_id,omitempty"`

	// Steps records completed idempotent steps by Action.ID (value: RFC3339 time).
	Steps map[string]string `json:"steps,omitempty"`

	// K3sVersion is the version the bootstrap installed.
	K3sVersion string `json:"k3s_version,omitempty"`
	// Issued records ACME issuance attempts for the instant URL (see sslip.go).
	Issued []Issuance `json:"issued,omitempty"`
}

// ServerState identifies the server.
type ServerState struct {
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	IP   string `json:"ip"`
	User string `json:"user,omitempty"`
	Port int    `json:"port,omitempty"`
}

// Issuance is one certificate issuance attempt for a set of hostnames.
type Issuance struct {
	At    time.Time `json:"at"`
	Hosts []string  `json:"hosts"`
	OK    bool      `json:"ok"`
}

// Done reports whether a step has been recorded.
func (s *State) Done(id string) bool { _, ok := s.Steps[id]; return ok }

// Mark records a step as complete.
func (s *State) Mark(id string, now time.Time) {
	if s.Steps == nil {
		s.Steps = map[string]string{}
	}
	s.Steps[id] = now.UTC().Format(time.RFC3339)
}

// Store reads and writes the state file under an exclusive lock, so two
// concurrent `can up` runs cannot interleave their updates.
type Store struct {
	Path string
	Now  func() time.Time
}

// NewStore returns a store for the company directory dir (state in dir/.can).
func NewStore(dir string) *Store { return &Store{Path: StatePath(dir), Now: time.Now} }

// Time returns the store's clock reading.
func (s *Store) Time() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ErrLocked is returned by TryMutate when another process holds the lock.
var ErrLocked = errors.New("state is locked by another can process")

func (s *Store) lock(block bool) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path derived from the company dir
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if !block {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil { //nolint:gosec // fd fits in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil
}

func unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // fd fits in int
	_ = f.Close()
}

// Load reads the state without locking (a missing file is an empty state).
func (s *Store) Load() (*State, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{Version: StateVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	if st.Version > StateVersion {
		return nil, fmt.Errorf("%s: written by a newer can (state version %d)", s.Path, st.Version)
	}
	st.Version = StateVersion
	return &st, nil
}

// Mutate loads the state under the lock, applies fn and saves atomically.
// If fn fails nothing is written. The lock is held only while fn runs, so fn
// must be short; long remote work belongs outside it.
func (s *Store) Mutate(fn func(*State) error) error { return s.mutate(true, fn) }

// TryMutate is Mutate that fails fast with ErrLocked instead of waiting.
func (s *Store) TryMutate(fn func(*State) error) error { return s.mutate(false, fn) }

func (s *Store) mutate(block bool, fn func(*State) error) error {
	lf, err := s.lock(block)
	if err != nil {
		return err
	}
	defer unlock(lf)
	st, err := s.Load()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// Remove deletes the state file (used after a successful destroy).
func (s *Store) Remove() error {
	err := os.Remove(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// LockOperation takes an exclusive, non-blocking lock for a whole `can up` or
// `can down` run (separate from the short state-update lock), so two
// concurrent runs cannot race on the same company. Call the returned func to
// release it.
func (s *Store) LockOperation() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.Path+".op.lock", os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path derived from the company dir
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another `can up` or `can down` is already running for this company: %w", ErrLocked)
		}
		return nil, err
	}
	return func() { unlock(f) }, nil
}
