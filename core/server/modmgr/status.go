package modmgr

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// MaxErrorRunes bounds an error shown in the UI.
const MaxErrorRunes = 240

var (
	reURLCred = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@:]*:[^\s/@]*@`)
	reSecret  = regexp.MustCompile(`(?i)\b(password|passwd|pwd|token|secret|api[_-]?key|authorization)\b\s*[:=]\s*(bearer\s+)?\S+`)
	reBearer  = regexp.MustCompile(`(?i)\bbearer\s+\S+`)
)

// Sanitise makes an error text safe to show an operator: control characters go, credentials in
// URLs and `password=...` style pairs are redacted, and the length is bounded. It is applied to
// everything that reaches a page from the installer or the cluster (last_error).
func Sanitise(s string) string {
	s = reURLCred.ReplaceAllString(s, "${1}[redacted]@")
	s = reSecret.ReplaceAllString(s, "$1=[redacted]")
	s = reBearer.ReplaceAllString(s, "bearer [redacted]")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r) || r == '�':
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if rs := []rune(out); len(rs) > MaxErrorRunes {
		out = string(rs[:MaxErrorRunes]) + "..."
	}
	return out
}

// Readiness values of a module.
const (
	ReadyYes     = "yes"
	ReadyNo      = "no"
	ReadyOff     = "off"
	ReadyUnknown = "unknown"
)

// ModuleHealth is one module on the status page.
type ModuleHealth struct {
	ID, Name, Version, State, Ready, LastError string
}

// ReconcileInfo is the last reconcile result (periodic loop or UI).
type ReconcileInfo struct {
	At      time.Time
	OK      bool
	Actions int
	Failed  int
	Error   string
}

// CertStatus is the state of one ingress host's certificate.
type CertStatus struct {
	Host     string `json:"host"`
	State    string `json:"state"` // valid, pending, failed, unknown
	DaysLeft int    `json:"days_left"`
}

// DiskUsage is one volume's usage.
type DiskUsage struct {
	Name        string `json:"name"`
	UsedBytes   uint64 `json:"used_bytes"`
	TotalBytes  uint64 `json:"total_bytes"`
	UsedPercent int    `json:"used_percent"`
}

// IngressStatus reports certificate and ingress state (backed by the cluster; fake in tests).
type IngressStatus interface {
	Certificates(ctx context.Context) ([]CertStatus, error)
}

// DiskReader reports disk usage (the data volume, PVCs; fake in tests).
type DiskReader interface {
	Usage(ctx context.Context) ([]DiskUsage, error)
}

// ReadyProbe optionally reports workload readiness from the cluster; without it readiness is the
// recorded state.
type ReadyProbe interface {
	Ready(ctx context.Context, module string) (ready bool, err error)
}

// Health is the status page.
type Health struct {
	Modules   []ModuleHealth
	Reconcile *ReconcileInfo
	Certs     []CertStatus
	CertsNote string // set when certificate status is unavailable
	Disk      []DiskUsage
	DiskNote  string
}

// Status gathers the health snapshot. A source that is missing or fails is reported as unavailable,
// never as healthy.
func (s *Service) Status(ctx context.Context) (Health, error) {
	var h Health
	ms, err := s.DB.ListModules(ctx)
	if err != nil {
		return h, err
	}
	for _, m := range ms {
		mh := ModuleHealth{ID: m.ID, Name: m.Manifest.Name, Version: m.Version, State: m.State, LastError: Sanitise(m.LastError), Ready: ReadyUnknown}
		switch {
		case m.State == store.StateDisabled:
			mh.Ready = ReadyOff
		case m.State == store.StateFailed:
			mh.Ready = ReadyNo
		case s.Probe != nil:
			if ok, perr := s.Probe.Ready(ctx, m.ID); perr == nil {
				mh.Ready = ReadyNo
				if ok {
					mh.Ready = ReadyYes
				}
			}
		default:
			mh.Ready = ReadyYes
		}
		h.Modules = append(h.Modules, mh)
	}
	if h.Reconcile, err = s.lastReconcile(ctx); err != nil {
		return h, err
	}
	if s.Ingress == nil {
		h.CertsNote = "unavailable"
	} else if h.Certs, err = s.Ingress.Certificates(ctx); err != nil {
		h.Certs, h.CertsNote = nil, "unavailable"
	}
	if s.Disk == nil {
		h.DiskNote = "unavailable"
	} else if h.Disk, err = s.Disk.Usage(ctx); err != nil {
		h.Disk, h.DiskNote = nil, "unavailable"
	} else {
		for i := range h.Disk {
			if t := h.Disk[i].TotalBytes; t > 0 {
				h.Disk[i].UsedPercent = int(h.Disk[i].UsedBytes * 100 / t)
			}
		}
	}
	return h, nil
}

// RecordReconcile stores the outcome of a reconcile (periodic or from the UI) for the status page.
func (s *Service) RecordReconcile(ctx context.Context, res reconcile.Result, err error) {
	msg := ""
	if err != nil {
		msg = Sanitise(err.Error())
	}
	ok := 0
	if err == nil && len(res.Failed) == 0 {
		ok = 1
	}
	_, _ = s.DB.ExecContext(ctx, `INSERT INTO reconcile_status(id, at, ok, actions, failed, error) VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET at=excluded.at, ok=excluded.ok, actions=excluded.actions, failed=excluded.failed, error=excluded.error`,
		s.now().Unix(), ok, len(res.Actions), len(res.Failed), msg)
}

func (s *Service) lastReconcile(ctx context.Context) (*ReconcileInfo, error) {
	var at int64
	var ok int
	ri := &ReconcileInfo{}
	err := s.DB.QueryRowContext(ctx, `SELECT at, ok, actions, failed, error FROM reconcile_status WHERE id=1`).Scan(&at, &ok, &ri.Actions, &ri.Failed, &ri.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ri.At, ri.OK = time.Unix(at, 0).UTC(), ok == 1
	return ri, nil
}
