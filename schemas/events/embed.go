// Package events embeds the JSON Schemas of platform events.
package events

import _ "embed"

// LeadCreatedV1 is the JSON Schema of the lead.created.v1 event.
//
//go:embed lead.created.v1.json
var LeadCreatedV1 []byte

// TicketCreatedV1 is the JSON Schema of the ticket.created.v1 event.
//
//go:embed ticket.created.v1.json
var TicketCreatedV1 []byte

// SubscriberAddedV1 is the JSON Schema of the subscriber.added.v1 event.
//
//go:embed subscriber.added.v1.json
var SubscriberAddedV1 []byte
