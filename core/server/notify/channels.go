package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"syscall"
	"time"

	"github.com/dodwmd/companyinacan/core/server/netguard"
)

// Message is one outgoing email.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string

	// ForUser is the subject ("user:<name>") the address belongs to.
	ForUser string
	// Module is the module that caused the notification, empty for the platform's own. The mailer
	// applies that module's email switch and quota.
	Module string
	// Authored means the module chose some of what Subject and Text say, so a mailer must not send
	// them unless the operator allowed that for the module.
	Authored bool
	// Platform marks an approval notice of can-core's engine (Notice.Platform).
	Platform bool
	// Type is the notification type; Count the number of notifications in a digest (0 otherwise).
	Type  string
	Count int
}

// Mailer sends email. The platform's `email.send` capability sits behind this interface; the
// implementation must bound its own run time (the service also passes a deadline in ctx).
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// Webhook posts a Slack-style message to an outbound channel URL.
type Webhook interface {
	Post(ctx context.Context, url, text string) error
}

// ErrNoMailer is returned by NoMailer.
var ErrNoMailer = errors.New("no email provider is configured")

// NoMailer is the default Mailer until an email.send provider exists: every send fails visibly
// (the delivery log shows it), nothing is silently dropped.
type NoMailer struct{}

// Send implements Mailer.
func (NoMailer) Send(context.Context, Message) error { return ErrNoMailer }

// FakeMailer records messages; used by tests.
type FakeMailer struct {
	mu   sync.Mutex
	msgs []Message
	Err  error
}

// Send implements Mailer.
func (f *FakeMailer) Send(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.msgs = append(f.msgs, m)
	return nil
}

// Sent returns the messages sent so far.
func (f *FakeMailer) Sent() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.msgs...)
}

// Post is one recorded webhook call.
type Post struct{ URL, Text string }

// FakeWebhook records posts; used by tests.
type FakeWebhook struct {
	mu    sync.Mutex
	posts []Post
	Err   error
}

// Post implements Webhook.
func (f *FakeWebhook) Post(_ context.Context, u, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.posts = append(f.posts, Post{u, text})
	return nil
}

// Posts returns the calls so far.
func (f *FakeWebhook) Posts() []Post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Post(nil), f.posts...)
}

// HTTPWebhook posts {"text": ...} to an https URL (the Slack incoming-webhook shape, also accepted
// by Mattermost, Rocket.Chat and Teams connectors). Redirects are never followed and errors never
// contain the URL, which usually embeds a secret token.
type HTTPWebhook struct {
	Client *http.Client
	// AllowInsecure permits http URLs (tests only).
	AllowInsecure bool
}

// Post implements Webhook.
func (h HTTPWebhook) Post(ctx context.Context, target, text string) error {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "https" && (!h.AllowInsecure || u.Scheme != "http")) || u.Host == "" {
		return errors.New("webhook URL must be https")
	}
	c := h.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if !h.AllowInsecure {
			// The URL comes from company.yaml, so it is operator-trusted, but a typo or a poisoned
			// file must not turn can-core into a probe of the cluster: refuse private, loopback and
			// link-local addresses at connect time (after DNS, so rebinding does not help).
			c.Transport = &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second, Control: denyPrivate}).DialContext}
		}
	}
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return errors.New("webhook request could not be built")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL
		}
		return fmt.Errorf("webhook post failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook answered %d", resp.StatusCode)
	}
	return nil
}

// denyPrivate rejects non-public destinations at connect time (see netguard).
func denyPrivate(network, address string, c syscall.RawConn) error {
	if err := netguard.DenyPrivate(network, address, c); err != nil {
		return errors.New("webhook destination is not a public address")
	}
	return nil
}

func publicAddr(ip netip.Addr) bool { return netguard.Public(ip) }
