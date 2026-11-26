package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// LedgerObjectKey is where the sealed ledger lives in the bucket: outside the
// restic prefix, so `restic forget`/`prune` never touch it.
const LedgerObjectKey = "ledger/erasure-ledger.v1.enc"

// Errors from the ledger.
var (
	ErrNoLedger = errors.New("no erasure ledger in the bucket")
	ErrNotFound = errors.New("object not found")
)

// Entry is one erasure request: an opaque subject id, a data class and a date.
// It deliberately carries no PII.
type Entry struct {
	Seq     int    `json:"seq"`
	Subject string `json:"subject"`
	Class   string `json:"class"`
	Date    string `json:"date"` // YYYY-MM-DD, UTC
	Prev    string `json:"prev"`
	Hash    string `json:"hash"`
}

func (e Entry) key() string { return e.Subject + "\x00" + e.Class }

func (e Entry) computeHash() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s", e.Prev, e.Seq, e.Subject, e.Class, e.Date)))
	return hex.EncodeToString(h[:])
}

var (
	subjectRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	classRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
)

// VerifyChain checks sequence numbers and the hash chain.
func VerifyChain(es []Entry) error {
	prev := ""
	for i, e := range es {
		if e.Seq != i+1 || e.Prev != prev || e.Hash != e.computeHash() {
			return fmt.Errorf("erasure ledger: entry %d fails verification (tampered, reordered or truncated at the head)", i+1)
		}
		prev = e.Hash
	}
	return nil
}

// Ledger is the append-only erasure tombstone file, written today only by `can backup
// ledger-add` and by a restore (no service records erasures by itself) and read by the host timer and by restore.
type Ledger struct {
	Path string
	Now  func() time.Time // default time.Now
	mu   sync.Mutex
}

// Read returns all entries after verifying the chain. A missing file is empty.
// A torn final line (a crash mid-append: no trailing newline, not valid JSON)
// is ignored; any other malformed line is an error.
func (l *Ledger) Read() ([]Entry, error) {
	es, _, err := l.read()
	return es, err
}

// read also returns the byte length of the valid prefix.
func (l *Ledger) read() ([]Entry, int, error) {
	raw, err := os.ReadFile(l.Path) //nolint:gosec // can-core's own path
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var es []Entry
	valid := 0
	rest := string(raw)
	for rest != "" {
		line, tail, hasNL := strings.Cut(rest, "\n")
		var e Entry
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if strings.TrimSpace(line) != "" {
			if err := dec.Decode(&e); err != nil {
				if !hasNL { // torn last line
					break
				}
				return nil, 0, fmt.Errorf("erasure ledger %s: %w", l.Path, err)
			}
			es = append(es, e)
		}
		valid += len(line)
		if hasNL {
			valid++
		}
		rest = tail
	}
	return es, valid, VerifyChain(es)
}

// Append records an erasure. Subject ids are only checked against a character
// pattern: the ledger cannot know that an id is not personal data, so callers
// must pass opaque ids.
// It is idempotent per (subject, class): added is
// false when the pair is already recorded. The file is only ever appended to.
func (l *Ledger) Append(subject, class string) (e Entry, added bool, err error) {
	if !subjectRe.MatchString(subject) {
		return e, false, fmt.Errorf("erasure ledger: subject %q is not an opaque id (letters, digits, . _ : -; no PII)", subject)
	}
	if !classRe.MatchString(class) {
		return e, false, fmt.Errorf("erasure ledger: bad data class %q", class)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	es, valid, err := l.read()
	if err != nil {
		return e, false, err
	}
	if st, serr := os.Stat(l.Path); serr == nil && int(st.Size()) > valid {
		if err := os.Truncate(l.Path, int64(valid)); err != nil { // drop a torn tail before appending
			return e, false, err
		}
	}
	for _, x := range es {
		if x.Subject == subject && x.Class == class {
			return x, false, nil
		}
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	e = Entry{Seq: len(es) + 1, Subject: subject, Class: class, Date: now().UTC().Format("2006-01-02")}
	if len(es) > 0 {
		e.Prev = es[len(es)-1].Hash
	}
	e.Hash = e.computeHash()
	line, _ := json.Marshal(e)
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // can-core's own path
	if err != nil {
		return e, false, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return e, false, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return e, false, err
	}
	return e, true, f.Close()
}

// MergeLedgers unions entries by (subject, class), keeps the earliest date,
// orders by date then subject then class, and rebuilds the sequence and chain.
func MergeLedgers(a, b []Entry) []Entry {
	byKey := map[string]Entry{}
	for _, e := range append(append([]Entry{}, a...), b...) {
		if old, ok := byKey[e.key()]; !ok || e.Date < old.Date {
			byKey[e.key()] = e
		}
	}
	out := make([]Entry, 0, len(byKey))
	for _, e := range byKey {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].key() < out[j].key()
	})
	prev := ""
	for i := range out {
		out[i].Seq, out[i].Prev = i+1, prev
		out[i].Hash = out[i].computeHash()
		prev = out[i].Hash
	}
	return out
}

// --- sealing: key derived from the restic password -------------------------

const ledgerMagic = "CANLEDGER1\n"

func ledgerAEAD(password string) (aeadX, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, []byte(password), []byte("can-erasure-ledger"), []byte("v1")), key); err != nil {
		return nil, err
	}
	return chacha20poly1305.NewX(key)
}

type aeadX interface {
	NonceSize() int
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}

// SealLedger encrypts entries with a key derived from the restic password, so
// that a restore (which has only the kit) can read it.
func SealLedger(es []Entry, password string) ([]byte, error) {
	var plain bytes.Buffer
	enc := json.NewEncoder(&plain)
	for _, e := range es {
		if err := enc.Encode(e); err != nil {
			return nil, err
		}
	}
	a, err := ledgerAEAD(password)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	aad := []byte(ledgerMagic + LedgerObjectKey)
	return append(append([]byte(ledgerMagic), nonce...), a.Seal(nil, nonce, plain.Bytes(), aad)...), nil
}

// OpenLedger decrypts and verifies a sealed ledger.
func OpenLedger(b []byte, password string) ([]Entry, error) {
	a, err := ledgerAEAD(password)
	if err != nil {
		return nil, err
	}
	if len(b) < len(ledgerMagic)+a.NonceSize() || string(b[:len(ledgerMagic)]) != ledgerMagic {
		return nil, errors.New("erasure ledger object: bad format")
	}
	rest := b[len(ledgerMagic):]
	plain, err := a.Open(nil, rest[:a.NonceSize()], rest[a.NonceSize():], []byte(ledgerMagic+LedgerObjectKey))
	if err != nil {
		return nil, errors.New("erasure ledger object: cannot decrypt (wrong restic password or tampered)")
	}
	var es []Entry
	for _, l := range strings.Split(string(plain), "\n") {
		if l == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			return nil, fmt.Errorf("erasure ledger object: %w", err)
		}
		es = append(es, e)
	}
	return es, VerifyChain(es)
}

// --- object store ----------------------------------------------------------

// ObjectStore is the bucket, as far as the ledger is concerned.
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte) error
	// Get returns ErrNotFound (wrapped or not) for a missing object.
	Get(ctx context.Context, key string) ([]byte, error)
}

// MemStore is an in-memory ObjectStore for tests.
type MemStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore { return &MemStore{m: map[string][]byte{}} }

// Put implements ObjectStore.
func (s *MemStore) Put(_ context.Context, k string, d []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = append([]byte(nil), d...)
	return nil
}

// Get implements ObjectStore.
func (s *MemStore) Get(_ context.Context, k string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.m[k]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), d...), nil
}

// FetchLedger downloads and opens the ledger; ErrNoLedger when none exists.
func FetchLedger(ctx context.Context, st ObjectStore, password string) ([]Entry, error) {
	b, err := st.Get(ctx, LedgerObjectKey)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrNoLedger
	}
	if err != nil {
		return nil, err
	}
	return OpenLedger(b, password)
}

// SyncLedger merges the local ledger into the bucket's and uploads the result.
// It never shrinks the remote ledger and aborts (without overwriting) if the
// remote object cannot be read. It returns the number of entries now stored.
func SyncLedger(ctx context.Context, st ObjectStore, l *Ledger, password string) (int, error) {
	local, err := l.Read()
	if err != nil {
		return 0, err
	}
	remote, err := FetchLedger(ctx, st, password)
	if err != nil && !errors.Is(err, ErrNoLedger) {
		return 0, fmt.Errorf("remote ledger unreadable, not overwriting: %w", err)
	}
	merged := MergeLedgers(remote, local)
	if len(merged) == len(remote) && len(remote) > 0 {
		return len(merged), nil
	}
	sealed, err := SealLedger(merged, password)
	if err != nil {
		return 0, err
	}
	return len(merged), st.Put(ctx, LedgerObjectKey, sealed)
}

// S3Store is a minimal path-style S3 client (PUT and GET of single objects,
// AWS Signature V4), enough for the ledger without an SDK dependency.
type S3Store struct {
	Endpoint, Bucket, Region string
	AccessKeyID              string
	SecretAccessKey          string
	HTTP                     *http.Client
	Now                      func() time.Time
	// AllowInsecure permits an http:// endpoint (tests, local MinIO). The default
	// refuses it: the signed request would otherwise be sent in clear.
	AllowInsecure bool
}

func hmacSHA(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func (s *S3Store) do(ctx context.Context, method, key string, body []byte) ([]byte, error) {
	base, err := url.Parse(strings.TrimRight(s.Endpoint, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("s3: bad endpoint %q", s.Endpoint)
	}
	if base.Scheme != "https" && (base.Scheme != "http" || !s.AllowInsecure) {
		return nil, fmt.Errorf("s3: endpoint %q must be https", s.Endpoint)
	}
	uri := (&url.URL{Path: strings.TrimRight(base.Path, "/") + "/" + s.Bucket + "/" + key}).EscapedPath()
	u := base.Scheme + "://" + base.Host + uri
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	t := now().UTC()
	amzDate, day := t.Format("20060102T150405Z"), t.Format("20060102")
	ph := sha256.Sum256(body)
	payload := hex.EncodeToString(ph[:])
	region := s.Region
	if region == "" {
		region = "auto"
	}
	canon := strings.Join([]string{
		method, uri, "",
		"host:" + base.Host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + amzDate + "\n",
		"host;x-amz-content-sha256;x-amz-date", payload,
	}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	ch := sha256.Sum256([]byte(canon))
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(ch[:])
	k := hmacSHA(hmacSHA(hmacSHA(hmacSHA([]byte("AWS4"+s.SecretAccessKey), day), region), "s3"), "aws4_request")
	sig := hex.EncodeToString(hmacSHA(k, sts))
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.AccessKeyID+"/"+scope+
		", SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature="+sig)
	c := s.HTTP
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && bytes.Contains(data, []byte("NoSuchKey")):
		return nil, ErrNotFound
	case resp.StatusCode/100 != 2:
		return nil, fmt.Errorf("s3 %s %s: HTTP %d", method, key, resp.StatusCode)
	}
	return data, nil
}

// Put implements ObjectStore.
func (s *S3Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.do(ctx, http.MethodPut, key, data)
	return err
}

// Get implements ObjectStore.
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) {
	return s.do(ctx, http.MethodGet, key, nil)
}

// CheckNotBehind refuses a bucket ledger that lacks entries the snapshot's copy
// has: that is a rolled-back or truncated bucket object. It returns the union
// of both, which is what restore must apply.
func CheckNotBehind(bucket, snapshot []Entry) ([]Entry, error) {
	have := map[string]bool{}
	for _, e := range bucket {
		have[e.key()] = true
	}
	var missing []string
	for _, e := range snapshot {
		if !have[e.key()] {
			missing = append(missing, e.Subject+"/"+e.Class)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the bucket's erasure ledger lacks %d entries that the snapshot's copy has (%s): the bucket object was rolled back or truncated; refusing to go live", len(missing), strings.Join(missing, ", "))
	}
	return MergeLedgers(bucket, snapshot), nil
}
