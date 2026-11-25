package provision

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// WebhookConnector delivers each operation as a signed JSON POST to one URL, for systems that have
// no SCIM server. The receiver must treat Idempotency-Key as the de-duplication key. It may answer
// {"external_id": "..."} to a create_account; otherwise the person id is used.
//
//	X-Provision-Timestamp: unix seconds
//	X-Provision-Signature: v1=<hex HMAC-SHA256(secret, "<timestamp>." + body)>
type WebhookConnector struct {
	ConnName string
	URL      string
	Secret   string
	HTTP     *http.Client
	Now      func() time.Time
}

// Name implements Connector.
func (c *WebhookConnector) Name() string { return c.ConnName }

// Verified implements Connector.
func (c *WebhookConnector) Verified() bool { return false }

// WebhookSignature is the value of X-Provision-Signature for body sent at ts.
func WebhookSignature(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (c *WebhookConnector) send(ctx context.Context, key, op string, payload map[string]any) ([]byte, error) {
	payload["operation"] = op
	payload["idempotency_key"] = key
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, permanent("bad_request")
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	ts := now().Unix()
	st, out, err := doJSON(ctx, c.HTTP, http.MethodPost, c.URL, "application/json", map[string]string{
		"Idempotency-Key":       key,
		"X-Provision-Timestamp": strconv.FormatInt(ts, 10),
		"X-Provision-Signature": WebhookSignature(c.Secret, ts, b),
	}, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return out, classify(st)
}

func identityPayload(id Identity) map[string]any {
	return map[string]any{"party_id": id.PartyID, "user_name": id.UserName, "given_name": id.GivenName, "family_name": id.FamilyName, "display_name": id.DisplayName, "active": id.Active}
}

// CreateAccount implements Connector.
func (c *WebhookConnector) CreateAccount(ctx context.Context, key string, id Identity) (string, error) {
	out, err := c.send(ctx, key, opCreateAccount, map[string]any{"account": identityPayload(id)})
	if err != nil {
		return "", err
	}
	var r struct {
		ExternalID string `json:"external_id"`
	}
	if json.Unmarshal(out, &r) == nil && r.ExternalID != "" {
		if !extIDRe.MatchString(r.ExternalID) {
			return "", permanent("bad_response")
		}
		return r.ExternalID, nil
	}
	return id.PartyID, nil
}

// UpdateAccount implements Connector.
func (c *WebhookConnector) UpdateAccount(ctx context.Context, key, ext string, id Identity) error {
	_, err := c.send(ctx, key, opUpdateAccount, map[string]any{"external_id": ext, "account": identityPayload(id)})
	return err
}

// DisableAccount implements Connector.
func (c *WebhookConnector) DisableAccount(ctx context.Context, key, ext string) error {
	_, err := c.send(ctx, key, opDisableAccount, map[string]any{"external_id": ext})
	return err
}

// DeleteAccount implements Connector.
func (c *WebhookConnector) DeleteAccount(ctx context.Context, key, ext string) error {
	_, err := c.send(ctx, key, opDeleteAccount, map[string]any{"external_id": ext})
	return err
}

// AddToGroup implements Connector.
func (c *WebhookConnector) AddToGroup(ctx context.Context, key, ext, group string) error {
	_, err := c.send(ctx, key, opAddGroup, map[string]any{"external_id": ext, "group": group})
	return err
}

// RemoveFromGroup implements Connector.
func (c *WebhookConnector) RemoveFromGroup(ctx context.Context, key, ext, group string) error {
	_, err := c.send(ctx, key, opRemoveGroup, map[string]any{"external_id": ext, "group": group})
	return err
}
