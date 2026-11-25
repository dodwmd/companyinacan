// Package audit implements can-core's hash-chained, tamper-evident audit log (SPEC §3.7).
//
// Each record's hash covers its own fields and the previous record's hash, so modifying,
// deleting or reordering any record breaks the chain at that point. Dropping the tail of the log
// cannot be seen from the chain alone; callers can record the head (seq:hash) elsewhere and pass
// it to Verify as an anchor.
//
// Entries carry opaque ids (party ids, module ids), never raw personal data, so the log can be
// retained while the PII it refers to is erased.
package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/store"
)

// Genesis is the prev_hash of the first record.
var Genesis = strings.Repeat("0", 64)

// Violation kinds.
const (
	KindHashMismatch = "hash-mismatch" // a record's content does not match its hash
	KindChainBreak   = "chain-break"   // prev_hash does not equal the previous record's hash
	KindSeqGap       = "seq-gap"       // a record is missing or sequence numbers are out of order
	KindHeadMismatch = "head-mismatch" // the log no longer contains the anchored head
)

// Entry is what callers append.
type Entry struct {
	Actor  string
	Action string
	Target string
	Detail map[string]any
}

// Record is a stored, chained entry.
type Record struct {
	Seq      int64     `json:"seq"`
	Time     time.Time `json:"time"`
	Actor    string    `json:"actor"`
	Action   string    `json:"action"`
	Target   string    `json:"target"`
	Detail   string    `json:"detail"`
	PrevHash string    `json:"prev_hash"`
	Hash     string    `json:"hash"`
}

// Head identifies the newest record.
type Head struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

func (h Head) String() string { return fmt.Sprintf("%d:%s", h.Seq, h.Hash) }

// ParseHead parses "seq:hash" as printed by Head.String.
func ParseHead(s string) (Head, error) {
	a, b, ok := strings.Cut(s, ":")
	seq, err := strconv.ParseInt(a, 10, 64)
	if !ok || err != nil || seq < 1 || len(b) != 64 {
		return Head{}, fmt.Errorf("invalid head %q: want <seq>:<64 hex chars>", s)
	}
	if _, err := hex.DecodeString(b); err != nil {
		return Head{}, fmt.Errorf("invalid head %q: hash is not hex", s)
	}
	return Head{Seq: seq, Hash: b}, nil
}

// Log is the audit log over the state store.
type Log struct{ db *store.DB }

// New returns the audit log backed by db.
func New(db *store.DB) *Log { return &Log{db: db} }

// Append adds a chained record. Appends are serialised by the store's single connection and a
// transaction, so concurrent callers cannot fork the chain.
func (l *Log) Append(ctx context.Context, e Entry) (Record, error) {
	detail := "{}"
	if len(e.Detail) > 0 {
		b, err := json.Marshal(e.Detail) // map keys are sorted: deterministic
		if err != nil {
			return Record{}, err
		}
		detail = string(b)
	}
	var rec Record
	err := l.db.InTx(ctx, func(tx *sql.Tx) error {
		seq, prev := int64(0), Genesis
		err := tx.QueryRowContext(ctx, `SELECT seq, hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&seq, &prev)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		rec = Record{Seq: seq + 1, Time: l.db.Now().Truncate(time.Second), Actor: e.Actor, Action: e.Action, Target: e.Target, Detail: detail, PrevHash: prev}
		rec.Hash = hashOf(rec)
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_log(seq,time,actor,action,target,detail,prev_hash,hash) VALUES (?,?,?,?,?,?,?,?)`,
			rec.Seq, rec.Time.Unix(), rec.Actor, rec.Action, rec.Target, rec.Detail, rec.PrevHash, rec.Hash)
		return err
	})
	return rec, err
}

// hashOf is SHA-256 over a domain tag and the length-prefixed fields, so field boundaries cannot
// be shifted to forge a collision.
func hashOf(r Record) string {
	h := sha256.New()
	put := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	put("can-audit-v1")
	put(strconv.FormatInt(r.Seq, 10))
	put(strconv.FormatInt(r.Time.Unix(), 10))
	put(r.Actor)
	put(r.Action)
	put(r.Target)
	put(r.Detail)
	put(r.PrevHash)
	return hex.EncodeToString(h.Sum(nil))
}

// Head returns the newest record's seq and hash (zero Head for an empty log).
func (l *Log) Head(ctx context.Context) (Head, error) {
	var h Head
	err := l.db.QueryRowContext(ctx, `SELECT seq, hash FROM audit_log ORDER BY seq DESC LIMIT 1`).Scan(&h.Seq, &h.Hash)
	if err == sql.ErrNoRows {
		return Head{}, nil
	}
	return h, err
}

// List returns up to limit records with seq > after, oldest first.
func (l *Log) List(ctx context.Context, after int64, limit int) ([]Record, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT seq,time,actor,action,target,detail,prev_hash,hash FROM audit_log WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scan(rows *sql.Rows) (Record, error) {
	var r Record
	var t int64
	err := rows.Scan(&r.Seq, &t, &r.Actor, &r.Action, &r.Target, &r.Detail, &r.PrevHash, &r.Hash)
	r.Time = time.Unix(t, 0)
	return r, err
}

// Export writes every record as one JSON object per line.
func (l *Log) Export(ctx context.Context, w io.Writer) error {
	enc := json.NewEncoder(w)
	var after int64
	for {
		recs, err := l.List(ctx, after, 500)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			return nil
		}
		for _, r := range recs {
			if err := enc.Encode(r); err != nil {
				return err
			}
			after = r.Seq
		}
	}
}

// Violation describes the first problem found.
type Violation struct {
	Seq     int64  `json:"seq"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// VerifyOptions tunes Verify.
type VerifyOptions struct {
	// Expected is an externally recorded head; the log must still contain that exact record.
	Expected *Head
}

// Result is the outcome of Verify.
type Result struct {
	Entries   int        `json:"entries"`
	Head      Head       `json:"head"`
	Violation *Violation `json:"violation,omitempty"`
}

// OK reports whether the chain is intact.
func (r Result) OK() bool { return r.Violation == nil }

// Verify walks the whole chain and reports the first violation, if any. An error is returned only
// for database failures, never for tampering.
func (l *Log) Verify(ctx context.Context, opt VerifyOptions) (Result, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT seq,time,actor,action,target,detail,prev_hash,hash FROM audit_log ORDER BY seq`)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = rows.Close() }()
	var res Result
	prev, want := Genesis, int64(1)
	anchorSeen := opt.Expected == nil
	fail := func(seq int64, kind, format string, a ...any) (Result, error) {
		res.Violation = &Violation{Seq: seq, Kind: kind, Message: fmt.Sprintf(format, a...)}
		return res, nil
	}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return res, err
		}
		if r.Seq != want {
			return fail(r.Seq, KindSeqGap, "expected record %d but found %d: a record was deleted or reordered", want, r.Seq)
		}
		if r.PrevHash != prev {
			return fail(r.Seq, KindChainBreak, "record %d does not chain to record %d", r.Seq, r.Seq-1)
		}
		if hashOf(r) != r.Hash {
			return fail(r.Seq, KindHashMismatch, "record %d content does not match its hash: it was modified", r.Seq)
		}
		if opt.Expected != nil && r.Seq == opt.Expected.Seq {
			if r.Hash != opt.Expected.Hash {
				return fail(r.Seq, KindHeadMismatch, "record %d differs from the anchored head", r.Seq)
			}
			anchorSeen = true
		}
		prev, want = r.Hash, want+1
		res.Entries++
		res.Head = Head{Seq: r.Seq, Hash: r.Hash}
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	if !anchorSeen {
		return fail(opt.Expected.Seq, KindHeadMismatch, "the log ends at record %d but the anchored head is record %d: the tail was removed", res.Head.Seq, opt.Expected.Seq)
	}
	return res, nil
}
