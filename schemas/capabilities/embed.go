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

// IdentityProvisionV1 is the contract of the `identity.provision` capability (in-house provisioning module).
//
//go:embed identity.provision.v1.json
var IdentityProvisionV1 []byte

// FilesV1 is the contract of the `files` capability (in-house files module).
//
//go:embed files.v1.json
var FilesV1 []byte

// InvoicesV1 is the contract of the `invoices` capability (in-house finance module).
//
//go:embed invoices.v1.json
var InvoicesV1 []byte

// ContractsV1 is the contract of the `contracts` capability (in-house finance module).
//
//go:embed contracts.v1.json
var ContractsV1 []byte

// TimesheetsV1 is the contract of the `timesheets` capability (in-house finance module).
//
//go:embed timesheets.v1.json
var TimesheetsV1 []byte

// WikiV1 is the contract of the `wiki` capability (DokuWiki module).
//
//go:embed wiki.v1.json
var WikiV1 []byte

// CrmV1 is the contract of the `crm` capability (EspoCRM module).
//
//go:embed crm.v1.json
var CrmV1 []byte

// StatusV1 is the contract of the `status` capability (Uptime Kuma module).
//
//go:embed status.v1.json
var StatusV1 []byte

// EquityV1 is the contract of the `equity` capability (in-house equity module).
//
//go:embed equity.v1.json
var EquityV1 []byte

// BoardV1 is the contract of the `board` capability (in-house board module).
//
//go:embed board.v1.json
var BoardV1 []byte

// ResolutionsV1 is the contract of the `resolutions` capability (in-house board module).
//
//go:embed resolutions.v1.json
var ResolutionsV1 []byte

// EntityV1 is the contract of the `entity` capability (in-house entity module).
//
//go:embed entity.v1.json
var EntityV1 []byte
