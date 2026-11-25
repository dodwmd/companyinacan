package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CloudflareAPIBase is the Cloudflare API root.
const CloudflareAPIBase = "https://api.cloudflare.com/client/v4"

const (
	cfOwnerComment = "managed-by=can"
	defaultCFToken = "env:CLOUDFLARE_API_TOKEN"
)

// Cloudflare is the DNS plugin that upserts the company's A/AAAA records in a
// Cloudflare zone. Records it creates carry the comment "managed-by=can";
// records without it are never modified unless dns.adopt_existing is set, and
// even then only unproxied ones and they are never deleted.
//
// API shapes follow https://developers.cloudflare.com/api/ and are UNVERIFIED
// against a live account.
type Cloudflare struct {
	cfg  DNSConfig
	deps Deps
	st   *Store
}

func newCloudflare(cfg DNSConfig, deps Deps, st *Store) (*Cloudflare, error) {
	if cfg.TTL != 0 && cfg.TTL != 1 && (cfg.TTL < 60 || cfg.TTL > 86400) {
		return nil, fmt.Errorf("dns.ttl %d: Cloudflare accepts 1 (automatic) or 60..86400 seconds", cfg.TTL)
	}
	if st == nil {
		return nil, fmt.Errorf("the cloudflare DNS plugin needs the company state store")
	}
	if cfg.TokenRef == "" {
		cfg.TokenRef = defaultCFToken
	}
	return &Cloudflare{cfg: cfg, deps: deps, st: st}, nil
}

// Name implements DNS.
func (*Cloudflare) Name() string { return "cloudflare" }

func (c *Cloudflare) ttlFor(r Record) int {
	switch {
	case c.cfg.Proxied:
		return 1 // proxied records must use automatic TTL
	case c.cfg.TTL != 0:
		return c.cfg.TTL
	case r.TTL != 0:
		return r.TTL
	}
	return DefaultTTL
}

// PlanRecords implements DNSPlanner.
func (c *Cloudflare) PlanRecords(domain string, recs []Record) []string {
	zone := "the zone containing " + domain
	if c.cfg.Zone != "" {
		zone = "zone " + c.cfg.Zone
	}
	var out []string
	for _, r := range recs {
		out = append(out, fmt.Sprintf("cloudflare: upsert %s %s -> %s in %s (ttl %d, proxied=%t, comment %q)", r.Type, r.Name, r.Value, zone, c.ttlFor(r), c.cfg.Proxied, cfOwnerComment))
	}
	return out
}

type cfClient struct {
	base, token string
	http        *http.Client
	redact      *Redactor
	sleep       func(context.Context, time.Duration)
}

type cfEnvelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

// do retries only where it cannot repeat an effect: 429 for any method (the
// request was refused) and GET on 5xx or network errors.
func (c *cfClient) do(ctx context.Context, method, path string, in any) (cfEnvelope, error) {
	var payload []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return cfEnvelope{}, err
		}
		payload = b
	}
	var lastErr error
	var wait time.Duration
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if wait <= 0 {
				wait = time.Duration(1<<attempt) * 500 * time.Millisecond
			}
			c.sleep(ctx, wait)
			wait = 0
			if ctx.Err() != nil {
				return cfEnvelope{}, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
		if err != nil {
			return cfEnvelope{}, c.redact.Err(err)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "can-cli")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = c.redact.Err(err)
			if method == http.MethodGet && ctx.Err() == nil {
				continue
			}
			return cfEnvelope{}, lastErr
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		var env cfEnvelope
		jerr := json.Unmarshal(body, &env)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && jerr == nil && env.Success {
			return env, nil
		}
		msgs := make([]string, 0, len(env.Errors))
		code := ""
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
			code = strconv.Itoa(e.Code)
		}
		msg := strings.Join(msgs, "; ")
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		lastErr = &APIError{Status: resp.StatusCode, Code: code, Message: c.redact.Redact(msg), Service: "cloudflare"}
		if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
			if wait > time.Minute {
				wait = time.Minute
			}
		}
		if resp.StatusCode != http.StatusTooManyRequests && (resp.StatusCode < 500 || method != http.MethodGet) {
			return cfEnvelope{}, lastErr
		}
	}
	return cfEnvelope{}, lastErr
}

func (c *Cloudflare) client() (*cfClient, error) {
	tok, err := c.deps.ResolveSecret(c.cfg.TokenRef)
	if err != nil {
		return nil, err
	}
	base := c.deps.CFBase
	if base == "" {
		base = CloudflareAPIBase
	}
	return &cfClient{base: strings.TrimRight(base, "/"), token: tok, http: c.deps.HTTP, redact: c.deps.Redactor, sleep: c.deps.Sleep}, nil
}

type cfRec struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment"`
}

// findZone returns the id of the zone for domain: dns.zone if set, otherwise
// the first of domain, its parent, ... that Cloudflare knows (down to two labels).
func (cl *cfClient) findZone(ctx context.Context, domain, explicit string) (string, string, error) {
	cands := []string{explicit}
	if explicit == "" {
		cands = nil
		labels := strings.Split(strings.ToLower(strings.TrimSuffix(domain, ".")), ".")
		for i := 0; i+2 <= len(labels); i++ {
			cands = append(cands, strings.Join(labels[i:], "."))
		}
	}
	for _, name := range cands {
		env, err := cl.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(name), nil)
		if err != nil {
			return "", "", err
		}
		var zs []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(env.Result, &zs); err != nil {
			return "", "", fmt.Errorf("cloudflare API: unreadable response: %w", err)
		}
		for _, z := range zs {
			if strings.EqualFold(z.Name, name) {
				return z.ID, z.Name, nil
			}
		}
	}
	return "", "", fmt.Errorf("no Cloudflare zone found for %s (tried %s); add the domain to Cloudflare, or check the token can read zones (Zone:Zone:Read)", domain, strings.Join(cands, ", "))
}

func (cl *cfClient) listByName(ctx context.Context, zone, name string) ([]cfRec, error) {
	var all []cfRec
	for page := 1; page <= 100; page++ {
		env, err := cl.do(ctx, http.MethodGet, fmt.Sprintf("/zones/%s/dns_records?name=%s&per_page=100&page=%d", url.PathEscape(zone), url.QueryEscape(name), page), nil)
		if err != nil {
			return nil, err
		}
		var rs []cfRec
		if err := json.Unmarshal(env.Result, &rs); err != nil {
			return nil, fmt.Errorf("cloudflare API: unreadable response: %w", err)
		}
		all = append(all, rs...)
		if page >= env.ResultInfo.TotalPages {
			break
		}
	}
	return all, nil
}

type cfWrite struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment,omitempty"`
}

func (c *Cloudflare) remember(rec DNSRecordState) error {
	return c.st.Mutate(func(s *State) error {
		for i, e := range s.DNSRecords {
			if e.Plugin == rec.Plugin && e.ID == rec.ID {
				s.DNSRecords[i] = rec
				return nil
			}
		}
		s.DNSRecords = append(s.DNSRecords, rec)
		return nil
	})
}

// Preflight implements DNSChecker: resolve the token and find the zone,
// changing nothing.
func (c *Cloudflare) Preflight(ctx context.Context, domain string) error {
	cl, err := c.client()
	if err != nil {
		return err
	}
	zone, _, err := cl.findZone(ctx, domain, c.cfg.Zone)
	if err != nil {
		return err
	}
	// read the records too: a token that can see the zone but not its DNS fails here
	if _, err := cl.listByName(ctx, zone, domain); err != nil {
		return fmt.Errorf("cannot read DNS records in the zone (token needs Zone:DNS:Edit): %w", err)
	}
	return nil
}

// Apply implements DNS: upsert every record, idempotently.
func (c *Cloudflare) Apply(ctx context.Context, domain string, recs []Record, w io.Writer) error {
	cl, err := c.client()
	if err != nil {
		return err
	}
	zone, zoneName, err := cl.findZone(ctx, domain, c.cfg.Zone)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "dns: cloudflare zone %s\n", zoneName)
	for _, r := range recs {
		want := cfWrite{Type: r.Type, Name: r.Name, Content: r.Value, TTL: c.ttlFor(r), Proxied: c.cfg.Proxied, Comment: cfOwnerComment}
		existing, err := cl.listByName(ctx, zone, r.Name)
		if err != nil {
			return err
		}
		var owned, foreign *cfRec
		for i := range existing {
			e := &existing[i]
			switch {
			case e.Type == "CNAME":
				return fmt.Errorf("cannot create %s %s: a CNAME already exists at that name (Cloudflare does not allow it next to address records); remove or rename it", r.Type, r.Name)
			case e.Type != r.Type:
			case e.Comment == cfOwnerComment:
				if owned == nil {
					owned = e
				}
			case foreign == nil:
				foreign = e
			}
		}
		base := DNSRecordState{Plugin: "cloudflare", Zone: zone, Type: r.Type, Name: r.Name, Content: r.Value, Proxied: want.Proxied}
		if same := sameTypeCount(existing, r.Type); same > 1 {
			fmt.Fprintf(w, "dns: warning: %d %s records exist for %s (round robin?); can manages only one of them\n", same, r.Type, r.Name)
		}
		switch {
		case owned != nil:
			if owned.Content != want.Content || owned.TTL != want.TTL || owned.Proxied != want.Proxied {
				if _, err := cl.do(ctx, http.MethodPut, "/zones/"+url.PathEscape(zone)+"/dns_records/"+url.PathEscape(owned.ID), want); err != nil {
					return err
				}
				fmt.Fprintf(w, "dns: updated %s %s -> %s\n", r.Type, r.Name, r.Value)
			} else {
				fmt.Fprintf(w, "dns: %s %s already set\n", r.Type, r.Name)
			}
			base.ID = owned.ID
		case foreign != nil:
			if !c.cfg.AdoptExisting {
				return fmt.Errorf("a %s record for %s already exists in Cloudflare and was not created by can (no %q comment); refusing to change it. Remove it, or set dns.adopt_existing: true to take over an unproxied record", r.Type, r.Name, cfOwnerComment)
			}
			if foreign.Proxied {
				return fmt.Errorf("the existing %s record for %s is proxied; dns.adopt_existing only takes over unproxied records. Turn the proxy off in Cloudflare first", r.Type, r.Name)
			}
			adopt := want
			adopt.Comment = foreign.Comment // leave its comment alone: it stays foreign
			if _, err := cl.do(ctx, http.MethodPut, "/zones/"+url.PathEscape(zone)+"/dns_records/"+url.PathEscape(foreign.ID), adopt); err != nil {
				return err
			}
			fmt.Fprintf(w, "dns: adopted existing %s %s -> %s (it will not be deleted by `can down`)\n", r.Type, r.Name, r.Value)
			base.ID, base.Adopted = foreign.ID, true
		default:
			env, err := cl.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zone)+"/dns_records", want)
			if err != nil {
				return err
			}
			var created cfRec
			if err := json.Unmarshal(env.Result, &created); err != nil || created.ID == "" {
				return fmt.Errorf("cloudflare API: created %s %s but the response has no record id", r.Type, r.Name)
			}
			fmt.Fprintf(w, "dns: created %s %s -> %s\n", r.Type, r.Name, r.Value)
			base.ID = created.ID
		}
		if err := c.remember(base); err != nil {
			return err
		}
	}
	fmt.Fprintln(w, "\nDNS changes are made; they can take a while to reach resolvers (not part of the five-minute budget).")
	return nil
}

func sameTypeCount(rs []cfRec, typ string) int {
	n := 0
	for _, r := range rs {
		if r.Type == typ {
			n++
		}
	}
	return n
}

// Remove implements DNSRemover: delete only records this plugin created.
func (c *Cloudflare) Remove(ctx context.Context, _ string, w io.Writer) error {
	state, err := c.st.Load()
	if err != nil {
		return err
	}
	var mine []DNSRecordState
	for _, r := range state.DNSRecords {
		if r.Plugin == "cloudflare" {
			mine = append(mine, r)
		}
	}
	if len(mine) == 0 {
		return nil
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	done := map[string]bool{}
	for _, r := range mine {
		switch {
		case r.Adopted:
			fmt.Fprintf(w, "dns: kept %s %s (it existed before can)\n", r.Type, r.Name)
		default:
			env, err := cl.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(r.Zone)+"/dns_records/"+url.PathEscape(r.ID), nil)
			if IsNotFound(err) {
				fmt.Fprintf(w, "dns: %s %s already gone\n", r.Type, r.Name)
				break
			}
			if err != nil {
				return err
			}
			var cur cfRec
			if err := json.Unmarshal(env.Result, &cur); err != nil {
				return fmt.Errorf("cloudflare API: unreadable response: %w", err)
			}
			if !strings.EqualFold(cur.Type, r.Type) || !strings.EqualFold(cur.Name, r.Name) || (r.Content != "" && cur.Content != r.Content) {
				fmt.Fprintf(w, "dns: %s %s was changed by an operator (now %s %s -> %s); left in place\n", r.Type, r.Name, cur.Type, cur.Name, cur.Content)
				break
			}
			if cur.Comment != cfOwnerComment {
				fmt.Fprintf(w, "dns: %s %s no longer carries the %q comment; leaving it alone\n", r.Type, r.Name, cfOwnerComment)
				break
			}
			if r.Content != "" && cur.Proxied != r.Proxied {
				fmt.Fprintf(w, "dns: warning: proxied was changed on %s %s after can wrote it; deleting it anyway\n", r.Type, r.Name)
			}
			if _, err := cl.do(ctx, http.MethodDelete, "/zones/"+url.PathEscape(r.Zone)+"/dns_records/"+url.PathEscape(r.ID), nil); err != nil && !IsNotFound(err) {
				return err
			}
			fmt.Fprintf(w, "dns: deleted %s %s\n", r.Type, r.Name)
		}
		done[r.ID] = true
	}
	return c.st.Mutate(func(s *State) error {
		kept := s.DNSRecords[:0]
		for _, r := range s.DNSRecords {
			if r.Plugin != "cloudflare" || !done[r.ID] {
				kept = append(kept, r)
			}
		}
		s.DNSRecords = kept
		return nil
	})
}
