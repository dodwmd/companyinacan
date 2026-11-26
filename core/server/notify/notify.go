// Package notify is can-core's notification service (SPEC §3.7, epic E11a): a per-user in-app
// inbox with unread counts, per-type/per-channel preferences, email with quiet hours, digests and
// a rate limit, named outbound webhook channels, a delivery log and de-duplication.
//
// In-app notifications are the baseline: they are stored for every notice (unless the user muted
// the type and the notice is not urgent). Email and webhook sends are best effort and recorded in
// the delivery log; a failure never fails the caller. Notification content is built from
// templates owned by the project and from short, escaped data values; callers pass identifiers,
// not personal data (the events and the in-app rows carry ids only).
package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	_ "time/tzdata" // named time zones for quiet hours on images without a zoneinfo database

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Channels.
const (
	ChannelInApp = "inapp"
	ChannelEmail = "email"
)

// Limits.
const (
	MaxUnreadCount       = 100 // the badge shows "99+" beyond this
	MaxRecipientsPerRule = 50
	MaxInAppPerHour      = 200 // per recipient; further notices in the hour are dropped
	maxDataKeys          = 20
	maxDataValue         = 200
	digestBatch          = 50
	defaultRateLimit     = 30
	sendTimeout          = 10 * time.Second
)

var (
	typeRe    = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	dataKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	chanRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
)

// RoleResolver finds the subjects holding a role (rbac.Service satisfies it).
type RoleResolver interface {
	SubjectsWithRole(ctx context.Context, role string, limit int) ([]string, error)
}

var _ RoleResolver = (*rbac.Service)(nil)

// Options configure a Service.
type Options struct {
	Now     func() time.Time
	Mailer  Mailer
	Webhook Webhook
	// Bus receives notification.sent.v1 events and is where the rules handler is registered; nil
	// disables events.
	Bus   *events.Bus
	Roles RoleResolver
	Log   *slog.Logger
	// RateLimitPerHour caps external messages per recipient per hour (default 30).
	RateLimitPerHour int

	// Web Push (push.go). Nothing below is used until a user subscribes.
	// PushContact is the VAPID `sub` claim (mailto: or https:), default the project URL.
	PushContact string
	// VAPIDKeyFile, when set, is the application server private key (a mounted Secret); otherwise a
	// key is generated on first use and kept in the state database.
	VAPIDKeyFile string
	// PushSender replaces the real Web Push client (tests).
	PushSender PushSender
	// PushHTTP replaces the push client's HTTP client; AllowInsecurePush skips the endpoint rules
	// of Subscribe. Both are for tests with an httptest push service.
	PushHTTP *http.Client
	// PushEndpointAllow is an optional host-suffix allowlist for push endpoints (default empty: any
	// public https host).
	PushEndpointAllow []string
	AllowInsecurePush bool
	// PushSleep waits between delivery retries (tests); default honours the context.
	PushSleep func(ctx context.Context, d time.Duration) error
}

// Service is the notification service.
type Service struct {
	db  *store.DB
	opt Options

	push push

	rulesAt time.Time
	rules   []moduleRule
}

// New returns a Service.
func New(db *store.DB, opt Options) *Service {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Mailer == nil {
		opt.Mailer = NoMailer{}
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.RateLimitPerHour <= 0 {
		opt.RateLimitPerHour = defaultRateLimit
	}
	if opt.PushSleep == nil {
		opt.PushSleep = sleepCtx
	}
	return &Service{db: db, opt: opt, push: push{sem: make(chan struct{}, maxPushInFlight), lastT: map[string]time.Time{}}}
}

// SetRateLimit changes the per-recipient hourly limit of external messages.
func (s *Service) SetRateLimit(n int) {
	if n > 0 {
		s.opt.RateLimitPerHour = n
	}
}

// Notice is one thing to tell one subject.
type Notice struct {
	Subject string
	Type    string
	// DedupeKey makes the notice idempotent per subject: a second notice with the same key is a no-op.
	DedupeKey string
	// Data are short values (ids, not personal data) available to the template.
	Data map[string]string
	// Link is a site-relative admin UI path.
	Link   string
	Urgent bool
	// RespectMute makes the recipient's in-app mute apply even to an urgent notice. Modules set it:
	// urgency is the platform's to grant, not a module's to claim against a recipient's choice.
	RespectMute bool
	// Module is the module that caused the notice (empty: the platform itself); Authored says that
	// module supplied part of its content (its own API call, or data of an event).
	Module   string
	Authored bool
}

var errInvalid = errors.New("invalid notification")

func cleanData(in map[string]string) (map[string]string, error) {
	if len(in) > maxDataKeys {
		return nil, fmt.Errorf("%w: more than %d data keys", errInvalid, maxDataKeys)
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if !dataKeyRe.MatchString(k) {
			return nil, fmt.Errorf("%w: data key %q", errInvalid, k)
		}
		out[k] = oneLine(v, maxDataValue)
	}
	return out, nil
}

func (n Notice) validate(subjectOK bool) (map[string]string, error) {
	if !subjectOK {
		return nil, fmt.Errorf("%w: subject", errInvalid)
	}
	if !typeRe.MatchString(n.Type) {
		return nil, fmt.Errorf("%w: type", errInvalid)
	}
	if n.Link != "" && (!strings.HasPrefix(n.Link, "/") || strings.HasPrefix(n.Link, "//") || strings.ContainsAny(n.Link, "\\\r\n ") || len(n.Link) > 200) {
		return nil, fmt.Errorf("%w: link must be a site-relative path", errInvalid)
	}
	if len(n.DedupeKey) > 200 {
		return nil, fmt.Errorf("%w: dedupe key too long", errInvalid)
	}
	return cleanData(n.Data)
}

// ErrInvalid reports whether err is a validation error of this package.
func ErrInvalid(err error) bool { return errors.Is(err, errInvalid) }

func (s *Service) prefEnabled(ctx context.Context, subject, typ, channel string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT type, enabled FROM notify_prefs WHERE subject=? AND channel=? AND type IN (?, '*')`, subject, channel, typ)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	enabled, wild := true, true
	specific := false
	for rows.Next() {
		var t string
		var e int
		if err := rows.Scan(&t, &e); err != nil {
			return false, err
		}
		if t == typ {
			enabled, specific = e == 1, true
		} else {
			wild = e == 1
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if specific {
		return enabled, nil
	}
	return wild, nil
}

// Notify records the notice in the subject's inbox and delivers it on the other channels the
// subject allows. created is false when the notice was a duplicate or muted.
func (s *Service) Notify(ctx context.Context, n Notice) (id int64, created bool, err error) {
	data, err := n.validate(rbac.ValidSubject(n.Subject))
	if err != nil {
		return 0, false, err
	}
	if !n.Urgent || n.RespectMute {
		on, err := s.prefEnabled(ctx, n.Subject, n.Type, ChannelInApp)
		if err != nil {
			return 0, false, err
		}
		if !on {
			return 0, false, nil
		}
	}
	var recent int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM notifications WHERE subject=? AND created_at>? LIMIT ?)`,
		n.Subject, s.opt.Now().Add(-time.Hour).Unix(), MaxInAppPerHour).Scan(&recent); err != nil {
		return 0, false, err
	}
	if recent >= MaxInAppPerHour {
		return 0, false, nil // a flood of notices cannot bury the inbox or the database
	}
	id, created, err = s.insert(ctx, n, data)
	if err != nil || !created {
		return id, created, err
	}
	s.logDelivery(ctx, id, ChannelInApp, "sent", "")
	s.emitSent(ctx, id, n.Subject, n.Type, ChannelInApp)
	s.deliverEmail(ctx, id, n, data)
	s.deliverPush(ctx, id, n)
	return id, true, nil
}

func (s *Service) insert(ctx context.Context, n Notice, data map[string]string) (int64, bool, error) {
	dj, _ := json.Marshal(data)
	urgent := 0
	if n.Urgent {
		urgent = 1
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO notifications(subject, type, dedupe_key, data_json, link, urgent, created_at, origin, authored) VALUES (?,?,?,?,?,?,?,?,?)`,
		n.Subject, n.Type, n.DedupeKey, string(dj), n.Link, urgent, s.opt.Now().Unix(), n.Module, n.Authored)
	if err != nil {
		return 0, false, err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		var id int64
		err := s.db.QueryRowContext(ctx, `SELECT id FROM notifications WHERE subject=? AND dedupe_key=?`, n.Subject, n.DedupeKey).Scan(&id)
		return id, false, err
	}
	id, _ := res.LastInsertId()
	return id, true, nil
}

func (s *Service) logDelivery(ctx context.Context, id int64, channel, status, detail string) {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO notify_deliveries(notification_id, channel, status, detail, at) VALUES (?,?,?,?,?)`,
		id, channel, status, truncate(detail, 200), s.opt.Now().Unix()); err != nil {
		s.opt.Log.Error("notification delivery log failed", "error", err)
	}
}

func (s *Service) emitSent(ctx context.Context, id int64, subject, typ, channel string) {
	if s.opt.Bus == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"notification_id": id, "subject": subject, "type": typ, "channel": channel})
	if _, err := s.opt.Bus.Publish(ctx, "can-core", "notification.sent.v1", b); err != nil {
		s.opt.Log.Error("publishing notification.sent.v1 failed", "error", err)
	}
}

func (s *Service) externalCount(ctx context.Context, subject, channel string, since time.Time) (int, error) {
	var c int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_deliveries d JOIN notifications n ON n.id=d.notification_id
		WHERE n.subject=? AND d.channel=? AND d.status='sent' AND d.at>?`, subject, channel, since.Unix()).Scan(&c)
	return c, err
}

func (s *Service) deliverEmail(ctx context.Context, id int64, n Notice, data map[string]string) {
	on, err := s.prefEnabled(ctx, n.Subject, n.Type, ChannelEmail)
	if err != nil {
		s.opt.Log.Error("reading notification preferences failed", "error", err)
		return
	}
	set, err := s.Settings(ctx, n.Subject)
	if err != nil {
		s.opt.Log.Error("reading notification settings failed", "error", err)
		return
	}
	if set.Email == "" {
		return // nowhere to send; in-app only
	}
	if !on {
		s.logDelivery(ctx, id, ChannelEmail, "suppressed", "preference")
		return
	}
	now := s.opt.Now()
	if !n.Urgent && (set.Digest != "off" || inQuiet(now, set.QuietStart, set.QuietEnd, set.TZ)) {
		s.logDelivery(ctx, id, ChannelEmail, "queued", "digest or quiet hours")
		return
	}
	cnt, err := s.externalCount(ctx, n.Subject, ChannelEmail, now.Add(-time.Hour))
	if err != nil {
		s.opt.Log.Error("rate limit lookup failed", "error", err)
		return
	}
	if cnt >= s.opt.RateLimitPerHour {
		s.logDelivery(ctx, id, ChannelEmail, "suppressed", "rate limited")
		return
	}
	r := Render(n.Type, data)
	cctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := s.opt.Mailer.Send(cctx, Message{To: set.Email, Subject: r.Subject, Text: r.Text, HTML: r.HTML, ForUser: n.Subject, Module: n.Module, Authored: n.Authored, Type: n.Type}); err != nil {
		s.logDelivery(ctx, id, ChannelEmail, "failed", err.Error())
		return
	}
	s.logDelivery(ctx, id, ChannelEmail, "sent", "")
	s.emitSent(ctx, id, n.Subject, n.Type, ChannelEmail)
}

// NotifyChannel posts the notice to a named outbound webhook channel.
func (s *Service) NotifyChannel(ctx context.Context, name string, n Notice) error {
	if !chanRe.MatchString(name) {
		return store.ErrNotFound
	}
	n.Subject = "channel:" + name
	data, err := n.validate(true)
	if err != nil {
		return err
	}
	var target string
	if err := s.db.QueryRowContext(ctx, `SELECT url FROM notify_channels WHERE name=?`, name).Scan(&target); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	id, created, err := s.insert(ctx, n, data)
	if err != nil || !created {
		return err
	}
	cnt, err := s.externalCount(ctx, n.Subject, "webhook", s.opt.Now().Add(-time.Hour))
	if err != nil {
		return err
	}
	if cnt >= s.opt.RateLimitPerHour {
		s.logDelivery(ctx, id, "webhook", "suppressed", "rate limited")
		return nil
	}
	if s.opt.Webhook == nil {
		s.logDelivery(ctx, id, "webhook", "failed", "no webhook sender configured")
		return nil
	}
	r := Render(n.Type, data)
	cctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	text := r.Subject
	if r.Text != "" {
		text += "\n" + r.Text
	}
	if err := s.opt.Webhook.Post(cctx, target, text); err != nil {
		s.logDelivery(ctx, id, "webhook", "failed", err.Error())
		return nil
	}
	s.logDelivery(ctx, id, "webhook", "sent", "")
	s.emitSent(ctx, id, n.Subject, n.Type, "webhook")
	return nil
}

// SetChannels replaces the outbound webhook channels (company.yaml notifications.webhooks).
func (s *Service) SetChannels(ctx context.Context, channels map[string]string) error {
	for name, u := range channels {
		if !chanRe.MatchString(name) || !strings.HasPrefix(u, "https://") || len(u) > 500 || strings.ContainsAny(u, " \r\n") {
			return fmt.Errorf("%w: channel %q needs an https URL", errInvalid, name)
		}
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM notify_channels`); err != nil {
			return err
		}
		for name, u := range channels {
			if _, err := tx.ExecContext(ctx, `INSERT INTO notify_channels(name, url) VALUES (?,?)`, name, u); err != nil {
				return err
			}
		}
		return nil
	})
}

// ChannelNames lists the configured channels (never their URLs).
func (s *Service) ChannelNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM notify_channels ORDER BY name LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var x string
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// --- inbox ----------------------------------------------------------------------------------------

// Item is one inbox entry.
type Item struct {
	ID        int64             `json:"id"`
	Type      string            `json:"type"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	Data      map[string]string `json:"data"`
	DataJSON  string            `json:"-"`
	Link      string            `json:"link,omitempty"`
	Urgent    bool              `json:"urgent,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	Read      bool              `json:"read"`
}

// ListOptions select a page of the inbox, newest first.
type ListOptions struct {
	UnreadOnly bool
	BeforeID   int64 // keyset pagination: only ids below this
	Limit      int
}

// List returns one page of the subject's inbox (at most 100 rows).
func (s *Service) List(ctx context.Context, subject string, o ListOptions) ([]Item, error) {
	if o.Limit < 1 || o.Limit > 100 {
		o.Limit = 50
	}
	q := `SELECT id, type, data_json, link, urgent, created_at, read_at FROM notifications WHERE subject=?`
	args := []any{subject}
	if o.UnreadOnly {
		q += ` AND read_at=0`
	}
	if o.BeforeID > 0 {
		q += ` AND id<?`
		args = append(args, o.BeforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, o.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Item
	for rows.Next() {
		var it Item
		var created, read int64
		var urgent int
		if err := rows.Scan(&it.ID, &it.Type, &it.DataJSON, &it.Link, &urgent, &created, &read); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(it.DataJSON), &it.Data)
		r := Render(it.Type, it.Data)
		it.Title, it.Body, it.Urgent, it.CreatedAt, it.Read = r.Subject, r.Text, urgent == 1, time.Unix(created, 0), read != 0
		out = append(out, it)
	}
	return out, rows.Err()
}

// UnreadCount counts unread notifications up to MaxUnreadCount (a bounded index scan).
func (s *Service) UnreadCount(ctx context.Context, subject string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM notifications WHERE subject=? AND read_at=0 LIMIT ?)`, subject, MaxUnreadCount).Scan(&n)
	return n, err
}

// MarkRead marks one of the subject's own notifications read.
func (s *Service) MarkRead(ctx context.Context, subject string, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE notifications SET read_at=? WHERE id=? AND subject=? AND read_at=0`, s.opt.Now().Unix(), id, subject)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		err := s.db.QueryRowContext(ctx, `SELECT 1 FROM notifications WHERE id=? AND subject=?`, id, subject).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	return nil
}

// MarkAllRead marks the subject's whole inbox read.
func (s *Service) MarkAllRead(ctx context.Context, subject string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET read_at=? WHERE subject=? AND read_at=0`, s.opt.Now().Unix(), subject)
	return err
}

// Prune deletes notifications (and their delivery log) older than age and returns how many.
func (s *Service) Prune(ctx context.Context, age time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM notifications WHERE created_at < ?`, s.opt.Now().Add(-age).Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeliveryLog is one row of the delivery log.
type DeliveryLog struct {
	ID             int64     `json:"id"`
	NotificationID int64     `json:"notification_id"`
	Channel        string    `json:"channel"`
	Status         string    `json:"status"`
	Detail         string    `json:"detail,omitempty"`
	At             time.Time `json:"at"`
}

// Deliveries returns the newest delivery log rows (at most 500).
func (s *Service) Deliveries(ctx context.Context, limit int) ([]DeliveryLog, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, notification_id, channel, status, detail, at FROM notify_deliveries ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeliveryLog
	for rows.Next() {
		var d DeliveryLog
		var at int64
		if err := rows.Scan(&d.ID, &d.NotificationID, &d.Channel, &d.Status, &d.Detail, &at); err != nil {
			return nil, err
		}
		d.At = time.Unix(at, 0)
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- preferences and settings -----------------------------------------------------------------

// SetPref sets whether a channel is on for a notification type ("*" = every type without its own).
func (s *Service) SetPref(ctx context.Context, subject, typ, channel string, enabled bool) error {
	if !rbac.ValidSubject(subject) || (typ != "*" && !typeRe.MatchString(typ)) || (channel != ChannelEmail && channel != ChannelInApp && channel != ChannelWebPush) {
		return errInvalid
	}
	e := 0
	if enabled {
		e = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO notify_prefs(subject, type, channel, enabled) VALUES (?,?,?,?)
		ON CONFLICT(subject, type, channel) DO UPDATE SET enabled=excluded.enabled`, subject, typ, channel, e)
	return err
}

// Pref is one stored preference.
type Pref struct {
	Type    string `json:"type"`
	Channel string `json:"channel"`
	Enabled bool   `json:"enabled"`
}

// Prefs lists the subject's stored preferences (at most 200).
func (s *Service) Prefs(ctx context.Context, subject string) ([]Pref, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT type, channel, enabled FROM notify_prefs WHERE subject=? ORDER BY type, channel LIMIT 200`, subject)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Pref{}
	for rows.Next() {
		var p Pref
		var e int
		if err := rows.Scan(&p.Type, &p.Channel, &e); err != nil {
			return nil, err
		}
		p.Enabled = e == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// Settings are a subject's delivery settings.
type Settings struct {
	Subject    string `json:"subject"`
	Email      string `json:"email"`
	QuietStart int    `json:"quiet_start"` // minutes after midnight in TZ; -1 = none
	QuietEnd   int    `json:"quiet_end"`
	TZ         string `json:"tz"`
	Digest     string `json:"digest"` // off, hourly or daily
}

// Settings returns the subject's settings (defaults when none are stored).
func (s *Service) Settings(ctx context.Context, subject string) (Settings, error) {
	set := Settings{Subject: subject, QuietStart: -1, QuietEnd: -1, TZ: "UTC", Digest: "off"}
	err := s.db.QueryRowContext(ctx, `SELECT email, quiet_start, quiet_end, tz, digest FROM notify_settings WHERE subject=?`, subject).
		Scan(&set.Email, &set.QuietStart, &set.QuietEnd, &set.TZ, &set.Digest)
	if errors.Is(err, sql.ErrNoRows) {
		return set, nil
	}
	return set, err
}

// SetSettings validates and stores the subject's settings.
func (s *Service) SetSettings(ctx context.Context, set Settings) error {
	if !rbac.ValidSubject(set.Subject) {
		return errInvalid
	}
	if set.Email != "" {
		if !config.ValidAddress(set.Email) {
			return fmt.Errorf("%w: email address", errInvalid)
		}
	}
	inRange := func(m int) bool { return m == -1 || (m >= 0 && m < 24*60) }
	if !inRange(set.QuietStart) || !inRange(set.QuietEnd) || (set.QuietStart == -1) != (set.QuietEnd == -1) {
		return fmt.Errorf("%w: quiet hours", errInvalid)
	}
	if _, err := time.LoadLocation(set.TZ); err != nil || set.TZ == "" || set.TZ == "Local" {
		return fmt.Errorf("%w: time zone", errInvalid)
	}
	if set.Digest != "off" && set.Digest != "hourly" && set.Digest != "daily" {
		return fmt.Errorf("%w: digest", errInvalid)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO notify_settings(subject, email, quiet_start, quiet_end, tz, digest) VALUES (?,?,?,?,?,?)
		ON CONFLICT(subject) DO UPDATE SET email=excluded.email, quiet_start=excluded.quiet_start, quiet_end=excluded.quiet_end, tz=excluded.tz, digest=excluded.digest`,
		set.Subject, set.Email, set.QuietStart, set.QuietEnd, set.TZ, set.Digest)
	return err
}

// inQuiet reports whether now falls in the quiet window (which may wrap midnight) in tz. An
// unknown zone is read as UTC.
func inQuiet(now time.Time, start, end int, tz string) bool {
	if start < 0 || end < 0 || start == end {
		return false
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	l := now.In(loc)
	m := l.Hour()*60 + l.Minute()
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}

// --- digests ----------------------------------------------------------------------------------

type queued struct {
	deliveryID, notificationID int64
	subject, typ, dataJSON     string
	module                     string
	authored                   bool
	at                         time.Time
}

// FlushDigests sends the queued emails that are due: those held back by quiet hours once they are
// over, and digest subscribers' batches once an hour or day has passed since the oldest queued
// item. It returns how many digest emails were sent. Work per call is bounded.
func (s *Service) FlushDigests(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.id, d.notification_id, n.subject, n.type, n.data_json, n.origin, n.authored, d.at FROM notify_deliveries d
		JOIN notifications n ON n.id=d.notification_id WHERE d.channel='email' AND d.status='queued' ORDER BY n.subject, d.id LIMIT 500`)
	if err != nil {
		return 0, err
	}
	// One digest per person and originating module: each module's mail has its own switch, quota and
	// wording, and the platform's own notices never share a message with a module's.
	type group struct {
		subject, module string
		authored        bool
	}
	groups := map[group][]queued{}
	var order []group
	for rows.Next() {
		var q queued
		var at int64
		if err := rows.Scan(&q.deliveryID, &q.notificationID, &q.subject, &q.typ, &q.dataJSON, &q.module, &q.authored, &at); err != nil {
			_ = rows.Close()
			return 0, err
		}
		q.at = time.Unix(at, 0)
		g := group{q.subject, q.module, q.authored}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], q)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := s.opt.Now()
	sent := 0
	for _, g := range order {
		items, subj := groups[g], g.subject
		set, err := s.Settings(ctx, subj)
		if err != nil {
			return sent, err
		}
		if set.Email == "" || inQuiet(now, set.QuietStart, set.QuietEnd, set.TZ) {
			continue
		}
		switch set.Digest {
		case "hourly":
			if now.Sub(items[0].at) < time.Hour {
				continue
			}
		case "daily":
			if now.Sub(items[0].at) < 24*time.Hour {
				continue
			}
		}
		if len(items) > digestBatch {
			items = items[:digestBatch]
		}
		var b strings.Builder
		fmt.Fprintf(&b, "You have %d notifications.\n\n", len(items))
		for _, it := range items {
			var data map[string]string
			_ = json.Unmarshal([]byte(it.dataJSON), &data)
			b.WriteString("- " + Render(it.typ, data).Subject + "\n")
		}
		cctx, cancel := context.WithTimeout(ctx, sendTimeout)
		err = s.opt.Mailer.Send(cctx, Message{To: set.Email, Subject: fmt.Sprintf("%d notifications", len(items)), Text: b.String(), HTML: "<pre>" + htmlEscape(b.String()) + "</pre>",
			ForUser: subj, Module: g.module, Authored: g.authored, Type: items[0].typ, Count: len(items)})
		cancel()
		status, detail := "sent", "digest"
		if err != nil {
			status, detail = "failed", "digest: "+err.Error()
		} else {
			sent++
		}
		for _, it := range items {
			if _, uerr := s.db.ExecContext(ctx, `UPDATE notify_deliveries SET status=?, detail=?, at=? WHERE id=?`, status, truncate(detail, 200), now.Unix(), it.deliveryID); uerr != nil {
				return sent, uerr
			}
			if status == "sent" {
				s.emitSent(ctx, it.notificationID, subj, it.typ, ChannelEmail)
			}
		}
	}
	return sent, nil
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;").Replace(s)
}
