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

// WorkIssueCreatedV1 is the JSON Schema of the work.issue.created.v1 event.
//
//go:embed work.issue.created.v1.json
var WorkIssueCreatedV1 []byte

// WorkIssueUpdatedV1 is the JSON Schema of the work.issue.updated.v1 event.
//
//go:embed work.issue.updated.v1.json
var WorkIssueUpdatedV1 []byte

// WorkIssueTransitionedV1 is the JSON Schema of the work.issue.transitioned.v1 event.
//
//go:embed work.issue.transitioned.v1.json
var WorkIssueTransitionedV1 []byte

// WorkCommentCreatedV1 is the JSON Schema of the work.comment.created.v1 event.
//
//go:embed work.comment.created.v1.json
var WorkCommentCreatedV1 []byte
