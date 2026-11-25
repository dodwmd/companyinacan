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

// WorkV1 is the contract of the `work` capability (in-house work tracker; Plane adapter later).
//
//go:embed work.v1.json
var WorkV1 []byte

// ProjectsV1 is the contract of the `projects` capability (read-only project directory of the work tracker).
//
//go:embed projects.v1.json
var ProjectsV1 []byte

// DirectoryV1 is the contract of the `directory` capability (in-house people module).
//
//go:embed directory.v1.json
var DirectoryV1 []byte

// PerformanceV1 is the contract of the `performance` capability (in-house people module).
//
//go:embed performance.v1.json
var PerformanceV1 []byte

// JobsV1 is the contract of the `jobs` capability (in-house jobs module: requisitions, careers pages, feeds).
//
//go:embed jobs.v1.json
var JobsV1 []byte

// AtsV1 is the contract of the `ats` capability (in-house jobs module: applications, candidates, pipeline).
//
//go:embed ats.v1.json
var AtsV1 []byte
