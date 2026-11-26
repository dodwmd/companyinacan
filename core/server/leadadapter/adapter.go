// Package leadadapter hands website leads to the modules company.yaml names and takes them back
// when a lead is erased (specs/067-lead-adapters). It is driven by the event outbox: a delivery
// that fails is retried with backoff, dead-lettered, shown in the admin UI and can be requeued.
//
// The events carry ids only. The adapter reads the lead from the forms module when it delivers, and
// keeps nothing of it: lead_links holds ids. Every error it returns is shown to operators and kept
// in the delivery log, so each is a fixed text; no response body and no transport error is quoted.
package leadadapter

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Event types handled.
const (
	EventCreated = "lead.created.v1"
	EventErased  = "lead.erased.v1"
)

// The modules an adapter exists for.
const (
	Support    = "support"
	Newsletter = "newsletter"
)

// consentField is the consent checkbox that lets a lead be subscribed (core/forms.NewsletterField).
const consentField = "newsletter"

// ErrNoLead is what Leads returns for a lead that does not exist (any more).
var ErrNoLead = errors.New("no such lead")

// Lead is what the adapters read of a submission. It is never stored.
type Lead struct {
	FormID, Email, Name string
	Fields              map[string]string
	Consents            []string
}

// Credentials of the Listmonk API user (Secret can-system/can-newsletter-api).
type Credentials struct{ Username, Token string }

// Adapter takes the deliveries of the subscriptions marked `via: can-core`.
type Adapter struct {
	DB    *store.DB
	Audit *audit.Log
	// Intake reads company.yaml as it is now.
	Intake func(ctx context.Context) (config.LeadIntake, error)
	// Lead reads a lead from the `leads` provider.
	Lead func(ctx context.Context, id string) (Lead, error)
	// Newsletter reads the API user; an error means it is not configured.
	Newsletter func() (Credentials, error)
	// Client must not follow redirects (webhook.NewHTTPClient).
	Client *http.Client
	Log    *slog.Logger
	Now    func() time.Time
}

// failure is an error whose text is safe to record.
type failure string

func (f failure) Error() string { return string(f) }

var leadIDRe = regexp.MustCompile(`^ld_[0-9a-f]{24}$`)

type eventData struct {
	LeadID string `json:"lead_id"`
	FormID string `json:"form_id"`
}

// Deliver implements webhook.Dispatcher.Adapter.
func (a *Adapter) Deliver(ctx context.Context, del store.Delivery, url string) (int, string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := a.deliver(ctx, del, url)
	if err == nil {
		return http.StatusOK, ""
	}
	var f failure
	if !errors.As(err, &f) {
		// Not one of ours (the state database): its text is logged, not recorded.
		a.Log.Error("lead adapter", "module", del.ModuleID, "event", del.Event.ID, "error", err)
		f = "can-core could not do its own part; see its log"
	}
	return 0, string(f)
}

func (a *Adapter) deliver(ctx context.Context, del store.Delivery, url string) error {
	if del.ModuleID != Support && del.ModuleID != Newsletter {
		return failure("can-core has no adapter for this module")
	}
	var d eventData
	// The publisher is the forms module; another module cannot pose as it to have data handed on
	// or erased.
	if del.Event.Source != "forms" || json.Unmarshal([]byte(del.Event.DataJSON), &d) != nil || !leadIDRe.MatchString(d.LeadID) {
		return nil
	}
	switch del.Event.Type {
	case EventCreated:
		return a.created(ctx, del, url, d)
	case EventErased:
		return a.erased(ctx, del, url, d)
	}
	return nil
}

// wanted reports whether company.yaml has module take the leads of a form.
func (a *Adapter) wanted(ctx context.Context, module, form string) (config.LeadIntake, bool, error) {
	in, err := a.Intake(ctx)
	if err != nil {
		return in, false, failure("company.yaml cannot be read")
	}
	forms := in.SupportForms
	if module == Newsletter {
		forms = in.NewsletterForms
	}
	return in, slices.Contains(forms, form), nil
}

func (a *Adapter) created(ctx context.Context, del store.Delivery, url string, d eventData) error {
	in, wanted, err := a.wanted(ctx, del.ModuleID, d.FormID)
	if err != nil || !wanted {
		return err
	}
	link, err := a.DB.GetLeadLink(ctx, del.ModuleID, d.LeadID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		link = store.LeadLink{ModuleID: del.ModuleID, LeadID: d.LeadID}
	case err != nil:
		return err
	case link.Done:
		return nil
	}
	lead, err := a.Lead(ctx, d.LeadID)
	if errors.Is(err, ErrNoLead) {
		return nil // erased or expired before it could be handed on
	}
	if err != nil {
		return failure("the lead cannot be read from the forms module")
	}
	if lead.FormID != d.FormID {
		return nil
	}
	if del.ModuleID == Newsletter {
		// Never without consent: the checkbox, recorded as such by forms when the lead was stored.
		if !slices.Contains(lead.Consents, consentField) || lead.Email == "" {
			return nil
		}
		err = a.subscribe(ctx, url, in.NewsletterList, lead, &link)
	} else {
		err = a.openConversation(ctx, del, url, lead, &link)
	}
	if err != nil {
		return err
	}
	link.Done = true
	if err := a.DB.PutLeadLink(ctx, link); err != nil {
		return err
	}
	return a.record(ctx, "lead.forward", d.LeadID, del.ModuleID)
}

func (a *Adapter) erased(ctx context.Context, del store.Delivery, url string, d eventData) error {
	link, err := a.DB.GetLeadLink(ctx, del.ModuleID, d.LeadID)
	known := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	switch del.ModuleID {
	case Newsletter:
		if !known {
			return nil
		}
		kept, err := a.unsubscribe(ctx, url, link)
		if err != nil {
			return err
		}
		if kept {
			if err := a.record(ctx, "lead.copy_kept", d.LeadID, del.ModuleID); err != nil {
				return err
			}
		} else if err := a.record(ctx, "lead.erase_copy", d.LeadID, del.ModuleID); err != nil {
			return err
		}
	case Support:
		// Without a link the module is still asked when the form is one it takes: the link can be
		// missing after a failure between its answer and the record of it.
		if !known {
			if _, wanted, err := a.wanted(ctx, del.ModuleID, d.FormID); err != nil || !wanted {
				return err
			}
		}
		noted, err := a.noteErasure(ctx, del, url, d.LeadID)
		if err != nil {
			return err
		}
		if noted {
			// The conversation keeps its copy: an operator decides whether the request covers it.
			if err := a.record(ctx, "lead.copy_kept", d.LeadID, del.ModuleID); err != nil {
				return err
			}
		}
	}
	return a.DB.DeleteLeadLink(ctx, del.ModuleID, d.LeadID)
}

func (a *Adapter) record(ctx context.Context, action, lead, module string) error {
	if a.Audit == nil {
		return nil
	}
	_, err := a.Audit.Append(ctx, audit.Entry{Actor: "can-core", Action: action, Target: lead, Detail: map[string]any{"module": module}})
	return err
}

// Targets implements events.Bus.Extra: an erasure also goes to every module that holds something
// made from the lead, even while that module is disabled; the delivery waits until it is enabled.
func (a *Adapter) Targets(ctx context.Context, eventType string, data json.RawMessage) []store.DeliveryTarget {
	var d eventData
	if eventType != EventErased || json.Unmarshal(data, &d) != nil {
		return nil
	}
	ids, err := a.DB.LeadLinkModules(ctx, d.LeadID)
	if err != nil {
		a.Log.Error("lead erasure: cannot list what was made from the lead", "lead", strconv.Quote(d.LeadID), "error", err)
		return nil
	}
	var out []store.DeliveryTarget
	for _, id := range ids {
		m, err := a.DB.GetModule(ctx, id)
		if err != nil {
			continue
		}
		for _, s := range m.Manifest.Subscribes {
			if s.Event != EventErased || s.Via != config.ViaCanCore {
				continue
			}
			if u, err := registry.ServiceURL(m.Namespace, s.Service, s.Port); err == nil {
				out = append(out, store.DeliveryTarget{ModuleID: id, URL: events.AdapterURL + u + s.Webhook})
			}
		}
	}
	return out
}
