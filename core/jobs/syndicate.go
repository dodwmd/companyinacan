package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrPartnerAccess means a board connector exists but the operator has no approved partner
// access to it. No request leaves the process.
var ErrPartnerAccess = errors.New("this board needs approved partner access; see docs/recruiting.md")

// BoardStatus is what the UI shows per board (SPEC.md section 5, Recruiting).
type BoardStatus string

// Board statuses.
const (
	BoardFeed         BoardStatus = "feed"                 // pulled by the board or crawler from our feeds; nothing is pushed
	BoardConnected    BoardStatus = "connected"            // an API connector with working credentials
	BoardNeedsPartner BoardStatus = "needs_partner_access" // connector stub: approval from the board is required first
)

// BoardCaps describes a board connector.
type BoardCaps struct {
	Name          string      `json:"name"`
	Status        BoardStatus `json:"status"`
	SupportsApply bool        `json:"supports_apply"`
	Note          string      `json:"note"`
}

// ExternalRef identifies a posting on a board.
type ExternalRef string

// Syndicator is the connector interface for job boards (SPEC.md section 5). The three layers
// map onto it: the careers page and feeds are FeedBoards (nothing to push), API connectors
// are real implementations that need partner credentials, and PartnerStub documents the
// ones this project cannot ship working. Implementations must not block for long: calls run
// with a timeout and a failure is recorded, never fatal to the job lifecycle.
type Syndicator interface {
	Name() string
	Caps() BoardCaps
	Publish(ctx context.Context, site SiteInfo, j *Job) (ExternalRef, error)
	Update(ctx context.Context, site SiteInfo, ref ExternalRef, j *Job) error
	Close(ctx context.Context, ref ExternalRef) error
}

// InboundApplication is what a pull-model connector hands back; the intake path de-duplicates
// it like any other application. No built-in connector produces these in this slice.
type InboundApplication struct {
	ExternalID string
	JobID      string
	Name       string
	Email      string
	ReceivedAt time.Time
}

// InboundPuller is an optional extension of Syndicator for boards that return applications.
type InboundPuller interface {
	Applications(ctx context.Context, since time.Time) ([]InboundApplication, error)
}

// FeedBoard is a board that reads our public feeds; publishing is a no-op because the feed
// reflects the job's state on the next pull.
type FeedBoard struct {
	name, note string
}

// NewFeedBoard returns a feed-only board entry.
func NewFeedBoard(name, note string) *FeedBoard { return &FeedBoard{name: name, note: note} }

// Name implements Syndicator.
func (f *FeedBoard) Name() string { return f.name }

// Caps implements Syndicator.
func (f *FeedBoard) Caps() BoardCaps {
	return BoardCaps{Name: f.name, Status: BoardFeed, Note: f.note}
}

// Publish implements Syndicator.
func (f *FeedBoard) Publish(context.Context, SiteInfo, *Job) (ExternalRef, error) {
	return ExternalRef("feed"), nil
}

// Update implements Syndicator.
func (f *FeedBoard) Update(context.Context, SiteInfo, ExternalRef, *Job) error { return nil }

// Close implements Syndicator.
func (f *FeedBoard) Close(context.Context, ExternalRef) error { return nil }

// PartnerStub stands for LinkedIn, Indeed, Glassdoor and similar boards whose posting APIs are
// open to approved partners only. It never touches the network: every call returns
// ErrPartnerAccess. To build a real connector, implement Syndicator against the board's
// partner documentation (and hold the approval) and pass it in Options.Syndicators in place
// of the stub.
type PartnerStub struct{ name string }

// NewPartnerStub returns the stub for a board name.
func NewPartnerStub(name string) *PartnerStub { return &PartnerStub{name: name} }

// Name implements Syndicator.
func (p *PartnerStub) Name() string { return p.name }

// Caps implements Syndicator.
func (p *PartnerStub) Caps() BoardCaps {
	return BoardCaps{Name: p.name, Status: BoardNeedsPartner, Note: "the board's posting API is open to approved partners only; apply to its programme, then replace this stub with a connector"}
}

// Publish implements Syndicator.
func (p *PartnerStub) Publish(context.Context, SiteInfo, *Job) (ExternalRef, error) {
	return "", ErrPartnerAccess
}

// Update implements Syndicator.
func (p *PartnerStub) Update(context.Context, SiteInfo, ExternalRef, *Job) error {
	return ErrPartnerAccess
}

// Close implements Syndicator.
func (p *PartnerStub) Close(context.Context, ExternalRef) error { return ErrPartnerAccess }

// FakeSyndicator records calls and reports itself connected; for tests.
type FakeSyndicator struct {
	name string
	mu   sync.Mutex
	log  []string
	// FailPublish makes Publish return this error.
	FailPublish error
}

// NewFakeSyndicator returns a recording connector.
func NewFakeSyndicator(name string) *FakeSyndicator { return &FakeSyndicator{name: name} }

// Name implements Syndicator.
func (f *FakeSyndicator) Name() string { return f.name }

// Caps implements Syndicator.
func (f *FakeSyndicator) Caps() BoardCaps {
	return BoardCaps{Name: f.name, Status: BoardConnected, SupportsApply: true, Note: "test double"}
}

func (f *FakeSyndicator) rec(s string) {
	f.mu.Lock()
	f.log = append(f.log, s)
	f.mu.Unlock()
}

// Calls returns the recorded calls in order ("publish:<job id>", "update:<ref>", "close:<ref>").
func (f *FakeSyndicator) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

// Publish implements Syndicator.
func (f *FakeSyndicator) Publish(_ context.Context, _ SiteInfo, j *Job) (ExternalRef, error) {
	f.rec("publish:" + j.ID)
	if f.FailPublish != nil {
		return "", f.FailPublish
	}
	return ExternalRef(fmt.Sprintf("%s-%s", f.name, j.ID)), nil
}

// Update implements Syndicator.
func (f *FakeSyndicator) Update(_ context.Context, _ SiteInfo, ref ExternalRef, _ *Job) error {
	f.rec("update:" + string(ref))
	return nil
}

// Close implements Syndicator.
func (f *FakeSyndicator) Close(_ context.Context, ref ExternalRef) error {
	f.rec("close:" + string(ref))
	return nil
}

// DefaultSyndicators is the board list a deployment starts with: the layers that work
// without anyone's permission, and honest stubs for the ones that do not.
func DefaultSyndicators() []Syndicator {
	return []Syndicator{
		NewFeedBoard("google-jobs", "Google for Jobs indexes the JobPosting data on the careers pages; no agreement needed"),
		NewFeedBoard("generic-feed", "XML and JSON feeds for aggregators that accept them; check each aggregator's terms"),
		NewFeedBoard("indeed-feed", "Indeed-format XML feed; Indeed accepts feeds only from approved partners, so this is a ready file, not an integration"),
		NewPartnerStub("linkedin"),
		NewPartnerStub("indeed"),
		NewPartnerStub("glassdoor"),
	}
}
