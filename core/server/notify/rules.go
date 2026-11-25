package notify

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type moduleRule struct {
	module string
	rule   config.NotifyRule
}

const rulesTTL = 30 * time.Second

// loadRules reads the notification rules of enabled modules, cached for rulesTTL.
func (s *Service) loadRules(ctx context.Context) ([]moduleRule, error) {
	if s.rules != nil && s.opt.Now().Sub(s.rulesAt) < rulesTTL {
		return s.rules, nil
	}
	mods, err := s.db.ListModules(ctx)
	if err != nil {
		return nil, err
	}
	rules := []moduleRule{}
	for _, m := range mods {
		if m.State != store.StateEnabled {
			continue
		}
		for _, r := range m.Manifest.Notifications {
			rules = append(rules, moduleRule{m.ID, r})
		}
	}
	s.rules, s.rulesAt = rules, s.opt.Now()
	return rules, nil
}

// idData keeps only identifier members of event data: `id` and `*_id`. Event payloads may carry
// personal data (names, emails); notifications never copy it.
func idData(raw json.RawMessage) map[string]string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return map[string]string{}
	}
	out := map[string]string{}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, ok := m[k].(string)
		if !ok || !dataKeyRe.MatchString(k) || (k != "id" && !strings.HasSuffix(k, "_id")) {
			continue
		}
		if len(out) >= maxDataKeys-2 {
			break
		}
		out[k] = oneLine(v, maxDataValue)
	}
	return out
}

// HandleEvent is the bus handler (events.Bus.SubscribeAll) that turns events into notifications
// according to the rules in module manifests. Errors are logged: a failing notification must never
// fail the publisher. notification.* events are ignored so rules cannot loop.
func (s *Service) HandleEvent(ctx context.Context, env events.Envelope) {
	if strings.HasPrefix(env.Type, "notification.") {
		return
	}
	rules, err := s.loadRules(ctx)
	if err != nil {
		s.opt.Log.Error("loading notification rules failed", "error", err)
		return
	}
	for _, mr := range rules {
		if mr.rule.Event != env.Type {
			continue
		}
		s.apply(ctx, mr, env)
	}
}

func (s *Service) apply(ctx context.Context, mr moduleRule, env events.Envelope) {
	typ := mr.rule.Type
	if typ != "" && !strings.HasPrefix(typ, mr.module+".") {
		s.opt.Log.Error("notification rule type outside the module namespace ignored", "module", mr.module, "type", typ)
		return
	}
	key := env.ID + "|" + mr.module + "|" + typ
	if typ == "" {
		typ = strings.TrimSuffix(env.Type, env.Type[strings.LastIndex(env.Type, "."):])
	}
	data := idData(env.Data)
	data["event_id"] = env.ID
	var raw map[string]any
	_ = json.Unmarshal(env.Data, &raw)

	seen := map[string]bool{}
	var subjects, channels []string
	for _, rcp := range mr.rule.Recipients {
		kind, val, _ := strings.Cut(rcp, ":")
		switch kind {
		case "role":
			if s.opt.Roles == nil {
				continue
			}
			ss, err := s.opt.Roles.SubjectsWithRole(ctx, rbac.QualifyRole(mr.module, val), MaxRecipientsPerRule)
			if err != nil {
				s.opt.Log.Error("resolving notification role failed", "role", val, "error", err)
				continue
			}
			subjects = append(subjects, ss...)
		case "user":
			subjects = append(subjects, val)
		case "field":
			if v, ok := raw[val].(string); ok {
				subjects = append(subjects, v)
			}
		case "channel":
			channels = append(channels, val)
		}
	}
	n := 0
	for _, subj := range subjects {
		if seen[subj] || !rbac.ValidSubject(subj) {
			continue
		}
		seen[subj] = true
		if n++; n > MaxRecipientsPerRule {
			break
		}
		if _, _, err := s.Notify(ctx, Notice{Subject: subj, Type: typ, DedupeKey: key, Data: data, Urgent: mr.rule.Urgent, RespectMute: true}); err != nil {
			s.opt.Log.Error("notification rule failed", "module", mr.module, "event", env.Type, "error", err)
		}
	}
	for _, ch := range channels {
		if err := s.NotifyChannel(ctx, ch, Notice{Type: typ, DedupeKey: key, Data: data}); err != nil && !errorsIsNotFound(err) {
			s.opt.Log.Error("notification channel failed", "module", mr.module, "event", env.Type, "error", err)
		}
	}
}

func errorsIsNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
