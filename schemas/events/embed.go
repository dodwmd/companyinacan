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

// PersonCreatedV1 is the JSON Schema of the person.created.v1 event.
//
//go:embed person.created.v1.json
var PersonCreatedV1 []byte

// PersonStatusChangedV1 is the JSON Schema of the person.status_changed.v1 event.
//
//go:embed person.status_changed.v1.json
var PersonStatusChangedV1 []byte

// PersonOffboardedV1 is the JSON Schema of the person.offboarded.v1 event.
//
//go:embed person.offboarded.v1.json
var PersonOffboardedV1 []byte

// OneOnOneScheduledV1 is the JSON Schema of the oneonone.scheduled.v1 event.
//
//go:embed oneonone.scheduled.v1.json
var OneOnOneScheduledV1 []byte

// GoalUpdatedV1 is the JSON Schema of the goal.updated.v1 event.
//
//go:embed goal.updated.v1.json
var GoalUpdatedV1 []byte

// ChecklistTaskCompletedV1 is the JSON Schema of the checklist.task_completed.v1 event.
//
//go:embed checklist.task_completed.v1.json
var ChecklistTaskCompletedV1 []byte

// ApprovalRequestedV1 is the JSON Schema of the approval.requested.v1 event.
//
//go:embed approval.requested.v1.json
var ApprovalRequestedV1 []byte

// ApprovalDecidedV1 is the JSON Schema of the approval.decided.v1 event.
//
//go:embed approval.decided.v1.json
var ApprovalDecidedV1 []byte

// NotificationSentV1 is the JSON Schema of the notification.sent.v1 event.
//
//go:embed notification.sent.v1.json
var NotificationSentV1 []byte

// PersonErasedV1 is the JSON Schema of the person.erased.v1 event.
//
//go:embed person.erased.v1.json
var PersonErasedV1 []byte

// JobOpenedV1 is the JSON Schema of the job.opened.v1 event.
//
//go:embed job.opened.v1.json
var JobOpenedV1 []byte

// ApplicationReceivedV1 is the JSON Schema of the application.received.v1 event.
//
//go:embed application.received.v1.json
var ApplicationReceivedV1 []byte

// ApplicationStageChangedV1 is the JSON Schema of the application.stage_changed.v1 event.
//
//go:embed application.stage_changed.v1.json
var ApplicationStageChangedV1 []byte

// CandidateHiredV1 is the JSON Schema of the candidate.hired.v1 event.
//
//go:embed candidate.hired.v1.json
var CandidateHiredV1 []byte
