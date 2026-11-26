package leadadapter

// FreeScout has no API in its free core, so the support chart runs a small listener beside it,
// inside the pod, on a port no route reaches (modules/support/chart/templates/_leads.tpl). Requests
// are signed like every webhook of can-core, with the module's own secret.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/webhook"
)

// supportRequest is the body of both calls; the erasure carries the id only.
type supportRequest struct {
	LeadID string            `json:"lead_id"`
	FormID string            `json:"form_id,omitempty"`
	Email  string            `json:"email,omitempty"`
	Name   string            `json:"name,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

type supportAnswer struct {
	// Conversation is the number of the lead's conversation, 0 when there is none.
	Conversation int64 `json:"conversation"`
}

func (a *Adapter) post(ctx context.Context, del store.Delivery, url string, in supportRequest) (supportAnswer, error) {
	var out supportAnswer
	body, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	if del.HMACSecret == "" {
		return out, failure("the support module has no signing secret; refusing to send unsigned")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return out, failure("the support module's address is not usable")
	}
	now := a.Now()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	req.Header.Set(webhook.HeaderSignature, webhook.Sign(del.HMACSecret, now, body))
	// The path is signed too, in effect: a creation cannot be replayed as an erasure, because the
	// listener tells them apart by the body (only a creation has a form).
	resp, err := a.Client.Do(req)
	if err != nil {
		return out, failure("the support module's lead listener does not answer (is the module upgraded and started?)")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		return out, failure("the support inbox has no mailbox yet: set support_address and let the module start")
	case resp.StatusCode != http.StatusOK:
		return out, failure("the support module answered HTTP " + strconv.Itoa(resp.StatusCode))
	case err != nil || json.Unmarshal(raw, &out) != nil:
		return out, failure("the support module answered something unexpected")
	}
	return out, nil
}

func (a *Adapter) openConversation(ctx context.Context, del store.Delivery, url string, lead Lead, link *store.LeadLink) error {
	out, err := a.post(ctx, del, url, supportRequest{LeadID: link.LeadID, FormID: lead.FormID, Email: lead.Email, Name: lead.Name, Fields: lead.Fields})
	if err != nil {
		return err
	}
	if out.Conversation <= 0 {
		return failure("the support module made no conversation")
	}
	link.Ref = strconv.FormatInt(out.Conversation, 10)
	return nil
}

// noteErasure has the module write an internal note into the lead's conversation. noted is false
// when there is no conversation (any more).
func (a *Adapter) noteErasure(ctx context.Context, del store.Delivery, url, lead string) (bool, error) {
	out, err := a.post(ctx, del, url, supportRequest{LeadID: lead})
	return out.Conversation > 0, err
}
