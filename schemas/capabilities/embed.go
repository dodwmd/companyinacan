// Package capabilities embeds the JSON Schemas of capability contracts (SPEC.md section 4.3).
package capabilities

import _ "embed"

// TicketsV1 is the contract of the `tickets` capability (helpdesk: FreeScout, in-house support).
//
//go:embed tickets.v1.json
var TicketsV1 []byte

// NewsletterV1 is the contract of the `newsletter` capability (Listmonk).
//
//go:embed newsletter.v1.json
var NewsletterV1 []byte

// AnalyticsV1 is the contract of the `analytics` capability (GoatCounter).
//
//go:embed analytics.v1.json
var AnalyticsV1 []byte
