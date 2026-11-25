// Package events is the in-process event bus and the platform event catalogue (SPEC §4.3).
//
// Publish validates the envelope, persists it to the SQLite outbox together with one pending
// delivery per subscribing module (matched on manifest `subscribes`), then runs in-process
// handlers and wakes the webhook dispatcher. Delivery itself is at-least-once and lives in
// package webhook.
package events

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Catalogue is the initial event catalogue (SPEC §4.3). It grows as modules are written; unknown
// but well-formed names are accepted, the catalogue is used for documentation and warnings.
var Catalogue = []string{
	"lead.created.v1",
	"deal.won.v1",
	"invoice.issued.v1", "invoice.paid.v1", "bill.received.v1", "bill.approved.v1", "bill.paid.v1",
	"vendor.created.v1", "po.approved.v1",
	"job.published.v1", "application.received.v1", "offer.accepted.v1",
	"person.hired.v1", "person.left.v1",
	"document.signed.v1",
	"contract.requested.v1", "contract.signed.v1", "contract.expiring.v1",
	"time.approved.v1",
	"resolution.passed.v1",
	"equity.proposed.v1", "equity.approved.v1", "equity.granted.v1",
	"issue.created.v1", "issue.updated.v1", "issue.transitioned.v1", "issue.priority_changed.v1",
	"one_on_one.action_item.created.v1", "review.cycle.opened.v1",
	"approval.requested.v1", "approval.approved.v1", "approval.rejected.v1",
}

var typeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+\.v[1-9][0-9]*$`)

// MaxDataBytes bounds the `data` member of one event.
const MaxDataBytes = 256 << 10

// ValidType reports whether s is a well-formed versioned event name (same rule as module.v1).
func ValidType(s string) bool { return typeRe.MatchString(s) }

// Known reports whether s is in the catalogue.
func Known(s string) bool {
	for _, c := range Catalogue {
		if c == s {
			return true
		}
	}
	return false
}

// Envelope is the JSON event envelope delivered to subscribers.
type Envelope struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Time   time.Time       `json:"time"`
	Source string          `json:"source"`
	Data   json.RawMessage `json:"data"`
}

// JSON renders the envelope (time in UTC, RFC 3339).
func (e Envelope) JSON() []byte {
	e.Time = e.Time.UTC()
	b, _ := json.Marshal(e) // fields are plain strings and valid raw JSON
	return b
}

// FromStored rebuilds an envelope from an outbox row.
func FromStored(s store.Event) Envelope {
	return Envelope{ID: s.ID, Type: s.Type, Time: s.Time, Source: s.Source, Data: json.RawMessage(s.DataJSON)}
}

// Handler is an in-process subscriber.
type Handler func(ctx context.Context, e Envelope)

// Bus publishes events.
type Bus struct {
	db       *store.DB
	log      *slog.Logger
	newID    func() string
	mu       sync.RWMutex
	handlers map[string][]Handler
	wake     chan struct{}
}

// NewBus returns a bus over db.
func NewBus(db *store.DB) *Bus {
	return &Bus{db: db, log: slog.Default(), newID: randomID, handlers: map[string][]Handler{}, wake: make(chan struct{}, 1)}
}

// SetLogger sets the logger used for handler panics.
func (b *Bus) SetLogger(l *slog.Logger) { b.log = l }

func randomID() string {
	var x [16]byte
	if _, err := rand.Read(x[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return "evt_" + hex.EncodeToString(x[:])
}

// Wake is signalled (coalesced) after every publish that created deliveries.
func (b *Bus) Wake() <-chan struct{} { return b.wake }

// Subscribe registers an in-process handler for an event type.
func (b *Bus) Subscribe(eventType string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], h)
}

// Publish validates, persists and fans out an event. source is the publishing module id or can-core.
func (b *Bus) Publish(ctx context.Context, source, eventType string, data json.RawMessage) (Envelope, error) {
	if source == "" {
		return Envelope{}, errors.New("event source is required")
	}
	if !ValidType(eventType) {
		return Envelope{}, fmt.Errorf("invalid event type %q: want <noun>.<verb>.v<N>", eventType)
	}
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	if len(data) > MaxDataBytes {
		return Envelope{}, fmt.Errorf("event data exceeds %d bytes", MaxDataBytes)
	}
	t := bytes.TrimSpace(data)
	if !json.Valid(t) || len(t) == 0 || t[0] != '{' {
		return Envelope{}, errors.New("event data must be a JSON object")
	}
	subs, err := b.db.Subscribers(ctx, eventType)
	if err != nil {
		return Envelope{}, err
	}
	env := Envelope{ID: b.newID(), Type: eventType, Time: b.db.Now().Truncate(time.Second), Source: source, Data: json.RawMessage(t)}
	targets := make([]store.DeliveryTarget, 0, len(subs))
	for _, s := range subs {
		targets = append(targets, store.DeliveryTarget{ModuleID: s.ModuleID, URL: registry.ServiceURL(s.Namespace, s.Service, s.Port) + s.Webhook})
	}
	if err := b.db.InsertEvent(ctx, store.Event{ID: env.ID, Type: env.Type, Time: env.Time, Source: source, DataJSON: string(env.Data)}, targets); err != nil {
		return Envelope{}, err
	}
	b.runHandlers(ctx, env)
	if len(targets) > 0 {
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
	return env, nil
}

func (b *Bus) runHandlers(ctx context.Context, env Envelope) {
	b.mu.RLock()
	hs := append([]Handler(nil), b.handlers[env.Type]...)
	b.mu.RUnlock()
	for _, h := range hs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					b.log.Error("event handler panicked", "event", env.Type, "panic", fmt.Sprint(r))
				}
			}()
			h(ctx, env)
		}()
	}
}
