// Package erasure collects the modules' erasure tombstones into the ledger the backup takes
// (specs/048-erasure-collection), so that a restore applies erasures again without anyone having
// written them down by hand. Subjects are opaque ids; they are never logged.
package erasure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// feeds are the modules with an erasure feed (GET /v1/erasures) and the key of the module's
// `can-module` Secret that may read it. A module that is not here records nothing in the ledger.
var feeds = map[string]string{
	"people": "CAN_PEOPLE_API_TOKEN",
	"equity": "CAN_EQUITY_API_TOKEN",
	"board":  "CAN_BOARD_ADMIN_TOKEN",
	"entity": "CAN_BOARD_ADMIN_TOKEN",
	"files":  "CAN_FILES_API_TOKEN",
	"jobs":   "CAN_JOBS_ADMIN_TOKEN",
}

const (
	maxPage  = 4 << 20
	maxPages = 10000
)

// Collector reads the feeds and appends to the ledger under Dir (the dumps directory).
type Collector struct {
	Modules func(context.Context) ([]store.Module, error)
	API     kube.API
	Dir     string
	// ServiceURL builds the in-cluster address of a module Service; nil means cluster DNS.
	ServiceURL func(namespace, service string, port int) string
	HTTP       *http.Client
	Now        func() time.Time
	Log        *slog.Logger
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Collector) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type tombstone struct {
	Seq     int64  `json:"seq"`
	Subject string `json:"subject"`
	Class   string `json:"class"`
	Date    string `json:"date"`
}

// Collect reads every feed from the start (a restored module numbers its tombstones afresh, so no
// cursor is kept), appends what is new and records what could not be read. An error means the
// ledger or the status could not be written.
func (c *Collector) Collect(ctx context.Context) (backup.CollectStatus, error) {
	st := backup.CollectStatus{Failed: map[string]string{}}
	mods, err := c.Modules(ctx)
	if err != nil {
		return st, fmt.Errorf("list modules: %w", err)
	}
	var found []backup.Entry
	for _, m := range mods {
		key, ok := feeds[m.ID]
		switch {
		case !ok:
			if len(m.Manifest.DataClasses) > 0 {
				st.NoFeed = append(st.NoFeed, m.ID)
			}
			continue
		case m.State == store.StateDisabled:
			st.NotAsked = append(st.NotAsked, m.ID)
			continue
		}
		es, rejected, err := c.feed(ctx, m, key)
		// What was read before a failure is still an erasure.
		found = append(found, es...)
		st.Rejected += rejected
		if rejected > 0 {
			c.log().Warn("erasure feed: tombstones refused, their subject or class is not an opaque id", "module", m.ID, "count", rejected)
		}
		switch {
		case err == nil:
			st.Collected = append(st.Collected, m.ID)
		case m.State == store.StateFailed: // it may never have run
			st.NotAsked = append(st.NotAsked, m.ID)
		default:
			st.Failed[m.ID] = err.Error()
		}
	}
	sort.Strings(st.Collected)
	sort.Strings(st.NoFeed)
	sort.Strings(st.NotAsked)
	dir := filepath.Join(c.Dir, backup.CollectedDir)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return st, err
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return st, fmt.Errorf("%s is not a plain directory", dir)
	}
	added, err := (&backup.Ledger{Path: filepath.Join(dir, backup.CollectedLedger), Now: c.Now}).AppendMany(found)
	if err != nil {
		return st, err
	}
	if added > 0 {
		c.log().Info("erasures added to the ledger", "count", added)
	}
	st.At = c.now().UTC()
	return st, writeStatus(filepath.Join(dir, backup.CollectedStatusFile), st)
}

func writeStatus(p string, st backup.CollectStatus) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// feed reads one module's tombstones, page by page.
func (c *Collector) feed(ctx context.Context, m store.Module, tokenKey string) (out []backup.Entry, rejected int, _ error) {
	if len(m.Manifest.Provides) == 0 {
		return nil, 0, errors.New("the manifest names no service to call")
	}
	sec, err := c.API.Get(ctx, "Secret", m.Namespace, "can-module")
	if err != nil {
		return nil, 0, fmt.Errorf("read the module Secret: %w", err)
	}
	data, _ := sec["data"].(map[string]any)
	enc, _ := data[tokenKey].(string)
	token, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(token) == 0 {
		return nil, 0, fmt.Errorf("the module Secret has no %s", tokenKey)
	}
	url := c.ServiceURL
	if url == nil {
		url = func(ns, service string, port int) string {
			return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", service, ns, port)
		}
	}
	base := strings.TrimRight(url(m.Namespace, m.Manifest.Provides[0].Service, m.Manifest.Provides[0].Port), "/")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	after := int64(0)
	for range maxPages {
		page, err := get(ctx, client, fmt.Sprintf("%s/v1/erasures?after=%d", base, after), string(token))
		if err != nil {
			return out, rejected, err
		}
		if len(page) == 0 {
			return out, rejected, nil
		}
		next := after
		for _, t := range page {
			next = max(next, t.Seq)
			if !backup.ValidTombstone(t.Subject, t.Class) {
				rejected++
				continue
			}
			if _, err := time.Parse("2006-01-02", t.Date); err != nil {
				t.Date = c.now().UTC().Format("2006-01-02")
			}
			out = append(out, backup.Entry{Subject: t.Subject, Class: t.Class, Date: t.Date}) // duplicates are dropped on append
		}
		if next <= after {
			return out, rejected, errors.New("the feed does not advance")
		}
		after = next
	}
	return out, rejected, errors.New("the feed has too many pages")
}

func get(ctx context.Context, client *http.Client, url, token string) ([]tombstone, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("the module did not answer")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the module answered HTTP %d", resp.StatusCode)
	}
	var body struct {
		Items []tombstone `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPage)).Decode(&body); err != nil {
		return nil, errors.New("the module's answer is not an erasure feed")
	}
	return body.Items, nil
}

// Loop collects at once and then every interval until ctx ends. Standing gaps are logged when
// they change, not on every run.
func (c *Collector) Loop(ctx context.Context, interval time.Duration) {
	said := ""
	for {
		st, err := c.Collect(ctx)
		if ctx.Err() != nil {
			return
		}
		failed := make([]string, 0, len(st.Failed))
		for m := range st.Failed {
			failed = append(failed, m)
		}
		sort.Strings(failed)
		switch now := fmt.Sprint(err, failed, st.NoFeed); {
		case now == said:
		case err != nil:
			c.log().Error("erasure collection failed: erasures are NOT reaching the backup ledger", "error", err)
			said = now
		default:
			if len(failed) > 0 {
				c.log().Warn("erasure feeds could not be read; their newest erasures are not in the backup ledger yet", "modules", failed)
			}
			if len(st.NoFeed) > 0 {
				c.log().Warn("modules with personal data classes and no erasure feed: what is erased in them is not recorded for restores", "modules", st.NoFeed)
			}
			said = now
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
