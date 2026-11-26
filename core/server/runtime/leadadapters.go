package runtime

// The lead adapters' connections to can-core (specs/069-lead-adapters).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/adminui"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/leadadapter"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/webhook"
)

// defaultNewsletterAPIDir is where deploy/can-core mounts the optional Secret can-newsletter-api.
const defaultNewsletterAPIDir = "/etc/can/newsletter-api"

func leadIntake(data []byte) (config.LeadIntake, error) {
	doc, _ := config.LoadBytes("company.yaml", data)
	if doc == nil {
		return config.LeadIntake{}, errors.New("company.yaml cannot be parsed")
	}
	co, err := config.DecodeCompany(doc)
	if err != nil {
		return config.LeadIntake{}, err
	}
	in, _ := config.LeadIntakeOf(co)
	return in, nil
}

// installedIntake narrows what company.yaml asks for to what the installed modules were given. A
// form that was added reaches a module only once its upgrade is applied, which for an installed
// module waits for approval (specs/064-module-settings); a form that was taken out stops at once.
func installedIntake(ctx context.Context, db *store.DB, asked config.LeadIntake) (config.LeadIntake, error) {
	installed := func(id, key string) (any, error) {
		m, err := db.GetModule(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return reconcile.ParseApplied(m.Applied).Values[key], nil
	}
	narrow := func(asked []string, id, key string) ([]string, error) {
		v, err := installed(id, key)
		if err != nil {
			return nil, err
		}
		given, _ := config.FormList(v)
		return slices.DeleteFunc(slices.Clone(asked), func(f string) bool { return !slices.Contains(given, f) }), nil
	}
	out := config.LeadIntake{NewsletterList: config.DefaultNewsletterList}
	var err error
	if out.SupportForms, err = narrow(asked.SupportForms, leadadapter.Support, "app.ticketsFromForms"); err != nil {
		return config.LeadIntake{}, err
	}
	if out.NewsletterForms, err = narrow(asked.NewsletterForms, leadadapter.Newsletter, "app.subscribeFromForms"); err != nil {
		return config.LeadIntake{}, err
	}
	list, err := installed(leadadapter.Newsletter, "app.defaultList")
	if err != nil {
		return config.LeadIntake{}, err
	}
	if v, _ := list.(string); strings.TrimSpace(v) != "" {
		out.NewsletterList = strings.TrimSpace(v)
	}
	return out, nil
}

// newLeadAdapter builds the adapters. company holds the store of company.yaml once module
// management has made it; before that, and without it, CAN_COMPANY_FILE is read.
func (c Config) newLeadAdapter(db *store.DB, al *audit.Log, leads adminui.LeadsClient, company *atomic.Pointer[modmgr.CompanyStore], log *slog.Logger) *leadadapter.Adapter {
	return &leadadapter.Adapter{
		DB: db, Audit: al, Log: log, Now: time.Now,
		Client: webhook.NewHTTPClient(15 * time.Second),
		Intake: func(ctx context.Context) (config.LeadIntake, error) {
			var data []byte
			if cs := company.Load(); cs != nil {
				snap, err := (*cs).Get(ctx)
				if errors.Is(err, modmgr.ErrNoCompany) {
					return config.LeadIntake{}, nil
				}
				if err != nil {
					return config.LeadIntake{}, err
				}
				data = snap.Data
			} else {
				b, err := os.ReadFile(c.CompanyFile)
				if errors.Is(err, os.ErrNotExist) {
					return config.LeadIntake{}, nil
				}
				if err != nil {
					return config.LeadIntake{}, err
				}
				data = b
			}
			asked, err := leadIntake(data)
			if err != nil {
				return config.LeadIntake{}, err
			}
			return installedIntake(ctx, db, asked)
		},
		Lead: func(ctx context.Context, id string) (leadadapter.Lead, error) {
			l, err := leads.Get(ctx, id)
			if errors.Is(err, adminui.ErrLeadNotFound) {
				return leadadapter.Lead{}, leadadapter.ErrNoLead
			}
			if err != nil {
				return leadadapter.Lead{}, err
			}
			out := leadadapter.Lead{FormID: l.FormID, Email: l.Email, Name: l.Name, Consents: l.Consents, Fields: map[string]string{}}
			for k, v := range l.Fields {
				out.Fields[k] = fmt.Sprint(v)
			}
			return out, nil
		},
		Newsletter: func() (leadadapter.Credentials, error) {
			dir := c.env("CAN_NEWSLETTER_API_SECRET_DIR")
			if dir == "" {
				dir = defaultNewsletterAPIDir
			}
			user, err := readSecretKey(filepath.Join(dir, "username"))
			if err != nil {
				return leadadapter.Credentials{}, err
			}
			token, err := readSecretKey(filepath.Join(dir, "token"))
			return leadadapter.Credentials{Username: user, Token: token}, err
		},
	}
}
