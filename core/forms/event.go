package forms

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// EventLeadCreated is the event type published for every stored submission. Its payload
// schema is schemas/events/lead.created.v1.json.
const EventLeadCreated = "lead.created.v1"

// Event is the platform event envelope (SPEC.md section 4.3).
type Event struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Time   time.Time       `json:"time"`
	Source string          `json:"source"`
	Data   LeadCreatedData `json:"data"`
}

// LeadCreatedData is the payload of lead.created.v1.
type LeadCreatedData struct {
	LeadID      string            `json:"lead_id"`
	FormID      string            `json:"form_id"`
	SubmittedAt time.Time         `json:"submitted_at"`
	Email       string            `json:"email,omitempty"`
	Name        string            `json:"name,omitempty"`
	Fields      map[string]string `json:"fields"`
}

// NewLeadCreated builds the event for a stored lead. now stamps the envelope.
func NewLeadCreated(l *Lead, now time.Time) Event {
	var b [12]byte
	_, _ = rand.Read(b[:])
	fields := l.Fields
	if fields == nil {
		fields = map[string]string{}
	}
	return Event{
		ID:     "evt_" + hex.EncodeToString(b[:]),
		Type:   EventLeadCreated,
		Time:   now.UTC(),
		Source: "forms",
		Data: LeadCreatedData{
			LeadID:      l.ID,
			FormID:      l.FormID,
			SubmittedAt: l.CreatedAt.UTC(),
			Email:       l.Email,
			Name:        l.Name,
			Fields:      fields,
		},
	}
}
