// Package events embeds the JSON Schemas of platform events.
package events

import _ "embed"

// LeadCreatedV1 is the JSON Schema of the lead.created.v1 event.
//
//go:embed lead.created.v1.json
var LeadCreatedV1 []byte
