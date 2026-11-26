package leadadapter

// Listmonk. Subscribing goes through its public subscription endpoint, the one its own sign-up form
// uses: no credential, double opt-in, a blocklisted address stays blocklisted and a confirmed one
// stays confirmed. The API user (a list role limited to the one list) is used to check that the
// list is double opt-in, to learn the subscriber's number and to delete it on erasure.
// The calls were checked against Listmonk 6.2.0.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dodwmd/companyinacan/core/server/store"
)

// leadAttrib is the subscriber attribute naming the lead a subscriber record was made from. A
// record without it existed before the form and is never deleted.
const leadAttrib = "can_lead"

const maxResponse = 1 << 20

type listmonk struct {
	a     *Adapter
	base  string
	creds Credentials
}

type subscriber struct {
	ID      int64          `json:"id"`
	Email   string         `json:"email"`
	Attribs map[string]any `json:"attribs"`
}

type list struct {
	ID     int64  `json:"id"`
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Optin  string `json:"optin"`
	Status string `json:"status"`
}

// call sends one request and returns the status; out receives the "data" member of a 2xx answer.
func (l *listmonk) call(ctx context.Context, method, path string, auth bool, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.base+path, body)
	if err != nil {
		return 0, failure("the newsletter module's address is not usable")
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "token "+l.creds.Username+":"+l.creds.Token)
	}
	resp, err := l.a.Client.Do(req)
	if err != nil {
		return 0, failure("the newsletter module does not answer")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, failure("the newsletter module's answer broke off")
	}
	if resp.StatusCode/100 == 2 && out != nil {
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &env) != nil || json.Unmarshal(env.Data, out) != nil {
			return 0, failure("the newsletter module answered something unexpected")
		}
	}
	return resp.StatusCode, nil
}

func (a *Adapter) listmonk(serviceURL string) (*listmonk, error) {
	u, err := url.Parse(serviceURL)
	if err != nil || u.Host == "" {
		return nil, failure("the newsletter module's address is not usable")
	}
	creds, err := a.Newsletter()
	if err != nil || creds.Username == "" || creds.Token == "" {
		return nil, failure("no Listmonk API user: create the Secret can-system/can-newsletter-api (modules/newsletter/README.md)")
	}
	return &listmonk{a: a, base: u.Scheme + "://" + u.Host, creds: creds}, nil
}

func statusFailure(what string, status int) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return failure(what + ": Listmonk refused the API user or its permissions (modules/newsletter/README.md)")
	}
	return failure(what + ": Listmonk answered HTTP " + strconv.Itoa(status))
}

// doubleOptInList finds the configured list among those the API user may see, and refuses any that
// would subscribe somebody without a confirmation.
func (l *listmonk) doubleOptInList(ctx context.Context, want string) (list, error) {
	var res struct {
		Results []list `json:"results"`
	}
	status, err := l.call(ctx, http.MethodGet, "/api/lists?per_page=all", true, nil, &res)
	if err != nil {
		return list{}, err
	}
	if status != http.StatusOK {
		return list{}, statusFailure("reading the lists", status)
	}
	var found []list
	for _, x := range res.Results {
		if x.UUID == want || strings.EqualFold(x.Name, want) {
			found = append(found, x)
		}
	}
	switch {
	case len(found) == 0:
		return list{}, failure("the list of default_list is not there, or the API user has no list role for it")
	case len(found) > 1:
		return list{}, failure("several lists have the name of default_list: write its UUID instead")
	case found[0].Optin != "double":
		return list{}, failure("the list of default_list is not double opt-in: nobody is subscribed")
	case found[0].Type != "public" || (found[0].Status != "" && found[0].Status != "active"):
		return list{}, failure("the list of default_list must be public and active")
	}
	return found[0], nil
}

// find looks a subscriber up by address. Listmonk's search finds nothing for a "+", so the longest
// part without one is searched and the address compared here.
func (l *listmonk) find(ctx context.Context, email string) (*subscriber, error) {
	term := ""
	for _, part := range strings.Split(email, "+") {
		if len(part) > len(term) {
			term = part
		}
	}
	var res struct {
		Results []subscriber `json:"results"`
	}
	status, err := l.call(ctx, http.MethodGet, "/api/subscribers?per_page=all&search="+url.QueryEscape(term), true, nil, &res)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusFailure("looking the subscriber up", status)
	}
	for _, s := range res.Results {
		if strings.EqualFold(s.Email, email) {
			return &s, nil
		}
	}
	return nil, nil
}

const maxName = 60

// greetingName is the name the confirmation mail greets. Anybody can type anybody's address into a
// form, so a "name" that could carry a message or a link to a stranger is replaced by the local part
// of the address.
func greetingName(name, email string) string {
	name = strings.TrimSpace(name)
	lower := strings.ToLower(name)
	if name == "" || utf8.RuneCountInString(name) > maxName || strings.ContainsAny(name, "/\\@:<>\r\n") || strings.Contains(lower, "www.") {
		name, _, _ = strings.Cut(email, "@")
	}
	return name
}

// subscribe is safe to repeat: the record is made at most once, and the public subscription changes
// nothing for somebody already confirmed.
func (a *Adapter) subscribe(ctx context.Context, serviceURL, listName string, lead Lead, link *store.LeadLink) error {
	l, err := a.listmonk(serviceURL)
	if err != nil {
		return err
	}
	to, err := l.doubleOptInList(ctx, listName)
	if err != nil {
		return err
	}
	email := strings.ToLower(lead.Email)
	name := greetingName(lead.Name, email)
	if link.Ref == "" {
		// A record of our own, on no list: its number is known at once and it carries the lead's id.
		var s subscriber
		status, err := l.call(ctx, http.MethodPost, "/api/subscribers", true, map[string]any{
			"email": email, "name": name, "status": "enabled", "lists": []int64{}, "attribs": map[string]string{leadAttrib: link.LeadID}}, &s)
		switch {
		case err != nil:
			return err
		case status == http.StatusOK:
			link.Ref = strconv.FormatInt(s.ID, 10)
		case status != http.StatusConflict: // 409: the address is there already
			return statusFailure("creating the subscriber", status)
		}
		if err := a.DB.PutLeadLink(ctx, *link); err != nil {
			return err
		}
	}
	status, err := l.call(ctx, http.MethodPost, "/api/public/subscription", false,
		map[string]any{"email": email, "name": name, "list_uuids": []string{to.UUID}}, nil)
	if err != nil {
		return err
	}
	if link.Ref == "" {
		// Looked up whatever the answer was: Listmonk adds the subscription before it sends the
		// confirmation, so a failed mail still leaves one that an erasure must find.
		s, err := l.find(ctx, email)
		if err != nil {
			return err
		}
		if s != nil {
			link.Ref = strconv.FormatInt(s.ID, 10)
			if err := a.DB.PutLeadLink(ctx, *link); err != nil {
				return err
			}
		}
	}
	if status != http.StatusOK {
		return failure("Listmonk could not subscribe or send the confirmation (HTTP " + strconv.Itoa(status) + "): check its SMTP settings and that public subscription is on")
	}
	return nil
}

// unsubscribe deletes the subscriber record a lead made. kept reports a record that stays: one that
// existed before the form, or that another lead that still exists led to as well.
func (a *Adapter) unsubscribe(ctx context.Context, serviceURL string, link store.LeadLink) (kept bool, err error) {
	id, err := strconv.ParseInt(link.Ref, 10, 64)
	if err != nil {
		return false, errors.New("a lead link holds no subscriber number")
	}
	others, err := a.DB.OtherLeadsOfRef(ctx, link.ModuleID, link.Ref, link.LeadID)
	if err != nil || others > 0 {
		return others > 0, err
	}
	l, err := a.listmonk(serviceURL)
	if err != nil {
		return false, err
	}
	var s subscriber
	path := "/api/subscribers/" + strconv.FormatInt(id, 10)
	status, err := l.call(ctx, http.MethodGet, path, true, nil, &s)
	switch {
	case err != nil:
		return false, err
	case status == http.StatusBadRequest || status == http.StatusNotFound:
		return false, nil // gone already
	case status != http.StatusOK:
		return false, statusFailure("reading the subscriber to erase (if it was taken off the list, erase it in Listmonk)", status)
	}
	if v, _ := s.Attribs[leadAttrib].(string); !leadIDRe.MatchString(v) {
		return true, nil
	}
	status, err = l.call(ctx, http.MethodDelete, path, true, nil, nil)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, statusFailure("deleting the subscriber", status)
	}
	return false, nil
}
