package provision

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// SCIM 2.0 server (RFC 7643 and 7644), a subset by design:
//
//	/ServiceProviderConfig /Schemas /ResourceTypes   discovery
//	/Users   GET list (filter eq/and, pagination), GET one, PATCH displayName and phone
//	/Groups  GET list, GET one, POST, PATCH members and displayName, DELETE
//
// Users are owned by the people directory: they cannot be created, replaced or deleted here, and
// only the preferred name and work phone can be patched. What is exposed is a fixed allow-list of
// non-sensitive fields; the leave status is never shown (a person on leave is active), and a
// candidate is not shown at all. Every request needs a bearer token with the scim.read scope
// (GET) or scim.write (everything else), and writes are audited with ids only.
const (
	schemaUser       = "urn:ietf:params:scim:schemas:core:2.0:User"
	schemaGroup      = "urn:ietf:params:scim:schemas:core:2.0:Group"
	schemaEnterprise = "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"
	schemaList       = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	schemaError      = "urn:ietf:params:scim:api:messages:2.0:Error"
	schemaPatch      = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	schemaSPC        = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	schemaRT         = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	schemaSchema     = "urn:ietf:params:scim:schemas:core:2.0:Schema"

	scimContentType = "application/scim+json"
	scimBase        = "/scim/v2"
	maxScimBody     = 64 << 10
	maxPatchOps     = 20
	maxGroupMembers = 1000
	maxGroups       = 500
	maxCount        = 100
)

var (
	userFilterAttrs  = map[string]bool{"id": true, "username": true, "externalid": true, "active": true, "emails.value": true, "name.givenname": true, "name.familyname": true, "displayname": true, "usertype": true}
	groupFilterAttrs = map[string]bool{"id": true, "displayname": true, "members.value": true}
)

// SCIM serves the SCIM endpoints.
type SCIM struct {
	Store *Store
	Dir   Directory
	Log   *slog.Logger
	Now   func() time.Time
	Cfg   SCIMConfig
	// OnMembership is called (best effort, after the change is stored) with the people whose
	// group membership changed, so the engine can start a mover run for them.
	OnMembership func(ctx context.Context, partyIDs []string)

	scan   chan struct{}
	routes []string
}

func (s *SCIM) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *SCIM) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Handler returns the SCIM routes, mounted at /scim/v2.
func (s *SCIM) Handler() http.Handler {
	if s.scan == nil {
		s.scan = make(chan struct{}, 4) // at most 4 directory scans at once
	}
	mux := http.NewServeMux()
	s.routes = nil
	h := func(pattern string, fn func(http.ResponseWriter, *http.Request, *scimPrincipal) error) {
		s.routes = append(s.routes, pattern)
		mux.HandleFunc(pattern, s.wrap(fn))
	}
	h("GET "+scimBase+"/ServiceProviderConfig", s.serviceProviderConfig)
	h("GET "+scimBase+"/ResourceTypes", s.resourceTypes)
	h("GET "+scimBase+"/ResourceTypes/{id}", s.resourceType)
	h("GET "+scimBase+"/Schemas", s.schemas)
	h("GET "+scimBase+"/Schemas/{id}", s.schema)
	h("GET "+scimBase+"/Users", s.listUsers)
	h("GET "+scimBase+"/Users/{id}", s.getUser)
	h("PATCH "+scimBase+"/Users/{id}", s.patchUser)
	h("POST "+scimBase+"/Users", s.refuseUserWrite)
	h("PUT "+scimBase+"/Users/{id}", s.refuseUserWrite)
	h("DELETE "+scimBase+"/Users/{id}", s.refuseUserWrite)
	h("GET "+scimBase+"/Groups", s.listGroups)
	h("GET "+scimBase+"/Groups/{id}", s.getGroup)
	h("POST "+scimBase+"/Groups", s.createGroup)
	h("PATCH "+scimBase+"/Groups/{id}", s.patchGroup)
	h("DELETE "+scimBase+"/Groups/{id}", s.deleteGroup)
	h("PUT "+scimBase+"/Groups/{id}", func(http.ResponseWriter, *http.Request, *scimPrincipal) error {
		return &SCIMError{Status: 501, Detail: "PUT is not supported; use PATCH"}
	})
	// anything else under the prefix is a SCIM-formatted 404
	mux.HandleFunc(scimBase+"/", s.wrap(func(http.ResponseWriter, *http.Request, *scimPrincipal) error {
		return &SCIMError{Status: 404, Detail: "no such resource"}
	}))
	return mux
}

// ---- plumbing ----------------------------------------------------------------------------------

func writeSCIM(w http.ResponseWriter, status int, v any, etag string) {
	w.Header().Set("Content-Type", scimContentType+"; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSCIMError(w http.ResponseWriter, e *SCIMError) {
	body := map[string]any{"schemas": []string{schemaError}, "status": strconv.Itoa(e.Status), "detail": e.Detail}
	if e.Type != "" {
		body["scimType"] = e.Type
	}
	writeSCIM(w, e.Status, body, "")
}

type scimPrincipal struct {
	TokenID string
	Write   bool
}

func (s *SCIM) wrap(fn func(http.ResponseWriter, *http.Request, *scimPrincipal) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !acceptsSCIM(r.Header.Get("Accept")) {
			writeSCIMError(w, &SCIMError{Status: 406, Detail: "this server only produces " + scimContentType})
			return
		}
		tok, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
			writeSCIMError(w, &SCIMError{Status: 401, Detail: "a bearer token is required"})
			return
		}
		pr, err := s.Store.scimAuth(r.Context(), tok)
		if err != nil {
			s.log().Error("scim: token check failed", "error", err)
			writeSCIMError(w, &SCIMError{Status: 503, Detail: "try again later"})
			return
		}
		if pr == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="scim", error="invalid_token"`)
			writeSCIMError(w, &SCIMError{Status: 401, Detail: "the token is not valid"})
			return
		}
		if r.Method != http.MethodGet && !pr.Write {
			writeSCIMError(w, &SCIMError{Status: 403, Detail: "the token has no scim.write scope"})
			return
		}
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodDelete {
			if r.ContentLength != 0 && !isSCIMContentType(r.Header.Get("Content-Type")) {
				writeSCIMError(w, &SCIMError{Status: 415, Detail: "Content-Type must be " + scimContentType})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxScimBody)
		}
		if err := fn(w, r, pr); err != nil {
			var se *SCIMError
			switch {
			case errors.As(err, &se):
				writeSCIMError(w, se)
			case errors.Is(err, ErrNotFound):
				writeSCIMError(w, &SCIMError{Status: 404, Detail: "no such resource"})
			case errors.Is(err, ErrUnavailable):
				w.Header().Set("Retry-After", "5")
				writeSCIMError(w, &SCIMError{Status: 503, Detail: "the people directory is unavailable"})
			default:
				s.log().Error("scim: request failed", "method", r.Method, "path", r.URL.Path, "error", err)
				writeSCIMError(w, &SCIMError{Status: 500, Detail: "internal error"})
			}
		}
	}
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || len(tok) > 256 {
		return "", false
	}
	return tok, true
}

func acceptsSCIM(accept string) bool {
	if strings.TrimSpace(accept) == "" {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		switch mt {
		case scimContentType, "application/json", "application/*", "*/*":
			return true
		}
	}
	return false
}

func isSCIMContentType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == scimContentType
}

// decodeBody reads a JSON body (bounded by the wrapper) and rejects trailing data.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return &SCIMError{Status: 413, Detail: "the request body is too large"}
		}
		return &SCIMError{Status: 400, Type: "invalidSyntax", Detail: "the body is not valid JSON"}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return &SCIMError{Status: 400, Type: "invalidSyntax", Detail: "unexpected data after the JSON body"}
	}
	return nil
}

func etagOf(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return `W/"` + hex.EncodeToString(h[:8]) + `"`
}

// checkIfMatch enforces optimistic concurrency: a write with a stale If-Match answers 412.
func (s *SCIM) checkIfMatch(r *http.Request, current string) error {
	im := strings.TrimSpace(r.Header.Get("If-Match"))
	if im == "" {
		if s.Cfg.RequireIfMatch {
			return &SCIMError{Status: 428, Detail: "an If-Match header is required"}
		}
		return nil
	}
	if im == "*" {
		return nil
	}
	for _, t := range strings.Split(im, ",") {
		if strings.TrimPrefix(strings.TrimSpace(t), "W/") == strings.TrimPrefix(current, "W/") {
			return nil
		}
	}
	return &SCIMError{Status: 412, Detail: "the resource changed; fetch it again"}
}

// ---- users -------------------------------------------------------------------------------------

func scimActive(p Person) bool {
	switch p.Status {
	case "onboarding", "active", "leave", "offboarding":
		return true // leave is shown as active: it can reveal health or family circumstances
	}
	return false
}

func visible(p Person) bool { return p.Status != "candidate" && p.Status != "" }

func (s *SCIM) userResource(p Person) map[string]any {
	user := p.WorkEmail
	if user == "" {
		user = p.ID
	}
	res := map[string]any{
		"schemas":     []string{schemaUser},
		"id":          p.ID,
		"userName":    user,
		"displayName": p.DisplayName(),
		"name":        map[string]string{"givenName": p.GivenName, "familyName": p.FamilyName},
		"userType":    p.Kind,
		"active":      scimActive(p),
		"meta":        map[string]any{"resourceType": "User", "location": scimBase + "/Users/" + p.ID},
	}
	if p.WorkEmail != "" {
		res["emails"] = []map[string]any{{"value": p.WorkEmail, "type": "work", "primary": true}}
	}
	if p.WorkPhone != "" {
		res["phoneNumbers"] = []map[string]any{{"value": p.WorkPhone, "type": "work"}}
	}
	meta := res["meta"].(map[string]any)
	if p.CreatedAt != "" {
		meta["created"] = p.CreatedAt
	}
	if p.UpdatedAt != "" {
		meta["lastModified"] = p.UpdatedAt
	}
	if p.ManagerID != "" {
		res["schemas"] = []string{schemaUser, schemaEnterprise}
		res[schemaEnterprise] = map[string]any{"manager": map[string]any{"value": p.ManagerID, "$ref": scimBase + "/Users/" + p.ManagerID}}
	}
	meta["version"] = etagOf(withoutMeta(res))
	return res
}

func withoutMeta(res map[string]any) map[string]any {
	c := make(map[string]any, len(res))
	for k, v := range res {
		if k != "meta" {
			c[k] = v
		}
	}
	return c
}

func userAttr(p Person) func(string) (string, bool) {
	return func(a string) (string, bool) {
		switch a {
		case "id":
			return p.ID, true
		case "username":
			if p.WorkEmail != "" {
				return p.WorkEmail, true
			}
			return p.ID, true
		case "externalid":
			return "", false
		case "active":
			return strconv.FormatBool(scimActive(p)), true
		case "emails.value":
			return p.WorkEmail, p.WorkEmail != ""
		case "name.givenname":
			return p.GivenName, true
		case "name.familyname":
			return p.FamilyName, true
		case "displayname":
			return p.DisplayName(), true
		case "usertype":
			return p.Kind, true
		}
		return "", false
	}
}

type listParams struct {
	filter Filter
	start  int
	count  int
}

func parseListParams(r *http.Request, attrs map[string]bool) (listParams, error) {
	q := r.URL.Query()
	lp := listParams{start: 1, count: 50}
	if v := q.Get("startIndex"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n > 1_000_000 {
			return lp, &SCIMError{Status: 400, Type: "invalidValue", Detail: "startIndex must be an integer"}
		}
		if n >= 1 {
			lp.start = n // a value below 1 is treated as 1 (RFC 7644 3.4.2.4)
		}
	}
	if v := q.Get("count"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return lp, &SCIMError{Status: 400, Type: "invalidValue", Detail: "count must be an integer"}
		}
		lp.count = max(0, min(n, maxCount))
	}
	if q.Has("sortBy") || q.Has("sortOrder") {
		return lp, &SCIMError{Status: 400, Type: "invalidValue", Detail: "sorting is not supported"}
	}
	if f := q.Get("filter"); q.Has("filter") {
		var err error
		if lp.filter, err = ParseFilter(f, attrs); err != nil {
			return lp, err
		}
	}
	return lp, nil
}

func (s *SCIM) acquireScan(ctx context.Context) (func(), error) {
	t := time.NewTimer(3 * time.Second)
	defer t.Stop()
	select {
	case s.scan <- struct{}{}:
		return func() { <-s.scan }, nil
	case <-t.C:
		return nil, &SCIMError{Status: 503, Detail: "the server is busy; try again"}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *SCIM) listUsers(w http.ResponseWriter, r *http.Request, _ *scimPrincipal) error {
	lp, err := parseListParams(r, userFilterAttrs)
	if err != nil {
		return err
	}
	// id eq: a direct lookup instead of a scan
	if len(lp.filter) == 1 && lp.filter[0].Attr == "id" {
		p, err := s.Dir.Get(r.Context(), lp.filter[0].Value)
		var res []any
		total := 0
		if err == nil && visible(p) {
			res, total = []any{s.userResource(p)}, 1
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		return s.writeList(w, lp, total, res)
	}
	release, err := s.acquireScan(r.Context())
	if err != nil {
		return err
	}
	defer release()
	scanLimit := s.Cfg.MaxScan
	if scanLimit <= 0 {
		scanLimit = 5000
	}
	var (
		total   int
		window  []any
		scanned int
		cursor  string
	)
	for {
		people, next, err := s.Dir.List(r.Context(), ListQuery{Cursor: cursor, Limit: 100})
		if err != nil {
			return err
		}
		for _, p := range people {
			scanned++
			if !visible(p) || !lp.filter.Match(userAttr(p)) {
				continue
			}
			total++
			if total >= lp.start && len(window) < lp.count {
				window = append(window, s.userResource(p))
			}
		}
		if next == "" || len(people) == 0 {
			break
		}
		if scanned >= scanLimit {
			return &SCIMError{Status: 400, Type: "tooMany", Detail: "the directory is too large to list in one request; narrow the filter"}
		}
		cursor = next
	}
	return s.writeList(w, lp, total, window)
}

func (s *SCIM) writeList(w http.ResponseWriter, lp listParams, total int, res []any) error {
	if res == nil {
		res = []any{}
	}
	writeSCIM(w, 200, map[string]any{"schemas": []string{schemaList}, "totalResults": total, "startIndex": lp.start, "itemsPerPage": len(res), "Resources": res}, "")
	return nil
}

func (s *SCIM) getPerson(ctx context.Context, id string) (Person, error) {
	if !partyRe.MatchString(id) {
		return Person{}, ErrNotFound
	}
	p, err := s.Dir.Get(ctx, id)
	if err != nil {
		return Person{}, err
	}
	if !visible(p) {
		return Person{}, ErrNotFound
	}
	return p, nil
}

func (s *SCIM) getUser(w http.ResponseWriter, r *http.Request, _ *scimPrincipal) error {
	p, err := s.getPerson(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	res := s.userResource(p)
	writeSCIM(w, 200, res, res["meta"].(map[string]any)["version"].(string))
	return nil
}

func (s *SCIM) refuseUserWrite(http.ResponseWriter, *http.Request, *scimPrincipal) error {
	return &SCIMError{Status: 403, Type: "mutability", Detail: "users are managed in the people directory; only PATCH of displayName and phoneNumbers is accepted"}
}

type patchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

type patchRequest struct {
	Schemas    []string  `json:"schemas"`
	Operations []patchOp `json:"Operations"`
}

func readPatch(r *http.Request) (patchRequest, error) {
	var pr patchRequest
	if err := decodeBody(r, &pr); err != nil {
		return pr, err
	}
	if !containsStr(pr.Schemas, schemaPatch) {
		return pr, &SCIMError{Status: 400, Type: "invalidSyntax", Detail: "schemas must contain " + schemaPatch}
	}
	if len(pr.Operations) == 0 || len(pr.Operations) > maxPatchOps {
		return pr, &SCIMError{Status: 400, Type: "invalidValue", Detail: fmt.Sprintf("between 1 and %d operations are required", maxPatchOps)}
	}
	return pr, nil
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func cleanText(s string, max int, what string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return "", &SCIMError{Status: 400, Type: "invalidValue", Detail: what + " is too long"}
	}
	for _, c := range s {
		if unicode.IsControl(c) || !unicode.IsPrint(c) {
			return "", &SCIMError{Status: 400, Type: "invalidValue", Detail: what + " contains control or invisible characters"}
		}
	}
	return s, nil
}

// userPatch turns PATCH operations into the one directory change this server makes. It accepts
// displayName and the work phone; every other attribute is managed in the directory.
func userPatch(ops []patchOp) (PersonPatch, error) {
	var pt PersonPatch
	set := func(attr string, raw json.RawMessage, remove bool) error {
		var val string
		if !remove {
			if err := json.Unmarshal(raw, &val); err != nil {
				return &SCIMError{Status: 400, Type: "invalidValue", Detail: attr + " must be a string"}
			}
		}
		switch attr {
		case "displayname":
			v, err := cleanText(val, 100, "displayName")
			if err != nil {
				return err
			}
			pt.PreferredName = &v
		case "phonenumbers.work":
			v, err := cleanText(val, 40, "phoneNumbers")
			if err != nil {
				return err
			}
			pt.WorkPhone = &v
		default:
			return &SCIMError{Status: 400, Type: "mutability", Detail: "that attribute is managed in the people directory"}
		}
		return nil
	}
	for _, op := range ops {
		kind := strings.ToLower(op.Op)
		if kind != "add" && kind != "replace" && kind != "remove" {
			return pt, &SCIMError{Status: 400, Type: "invalidSyntax", Detail: "op must be add, replace or remove"}
		}
		path := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(op.Path), " ", ""))
		path = strings.TrimPrefix(path, strings.ToLower(schemaUser)+":")
		switch path {
		case "displayname":
			if err := set("displayname", op.Value, kind == "remove"); err != nil {
				return pt, err
			}
		case "phonenumbers", `phonenumbers[typeeq"work"]`, `phonenumbers[typeeq"work"].value`, "phonenumbers.value":
			if kind == "remove" {
				if err := set("phonenumbers.work", nil, true); err != nil {
					return pt, err
				}
				continue
			}
			v, err := phoneValue(op.Value, path == "phonenumbers")
			if err != nil {
				return pt, err
			}
			if err := set("phonenumbers.work", v, false); err != nil {
				return pt, err
			}
		case "":
			if kind == "remove" {
				return pt, &SCIMError{Status: 400, Type: "noTarget", Detail: "remove needs a path"}
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal(op.Value, &obj) != nil {
				return pt, &SCIMError{Status: 400, Type: "invalidValue", Detail: "without a path the value must be an object"}
			}
			for k, v := range obj {
				switch strings.ToLower(k) {
				case "displayname":
					if err := set("displayname", v, false); err != nil {
						return pt, err
					}
				case "phonenumbers":
					pv, err := phoneValue(v, true)
					if err != nil {
						return pt, err
					}
					if err := set("phonenumbers.work", pv, false); err != nil {
						return pt, err
					}
				default:
					return pt, &SCIMError{Status: 400, Type: "mutability", Detail: "only displayName and phoneNumbers can be changed here"}
				}
			}
		case "active", "username", "name", "name.givenname", "name.familyname", "emails", "usertype", "id", "externalid":
			return pt, &SCIMError{Status: 400, Type: "mutability", Detail: "that attribute is managed in the people directory"}
		default:
			return pt, &SCIMError{Status: 400, Type: "noTarget", Detail: "the path does not name a supported attribute"}
		}
	}
	return pt, nil
}

// phoneValue extracts the work number from a plain string or from [{"value": "...", "type": "work"}].
func phoneValue(raw json.RawMessage, list bool) (json.RawMessage, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return raw, nil
	}
	var items []struct {
		Value string `json:"value"`
		Type  string `json:"type"`
	}
	if list && json.Unmarshal(raw, &items) == nil {
		for _, it := range items {
			if it.Type == "" || strings.EqualFold(it.Type, "work") {
				b, _ := json.Marshal(it.Value)
				return b, nil
			}
		}
		return nil, &SCIMError{Status: 400, Type: "invalidValue", Detail: "only the work phone number can be set"}
	}
	return nil, &SCIMError{Status: 400, Type: "invalidValue", Detail: "phoneNumbers has an invalid value"}
}

func (s *SCIM) patchUser(w http.ResponseWriter, r *http.Request, pr *scimPrincipal) error {
	req, err := readPatch(r)
	if err != nil {
		return err
	}
	pt, err := userPatch(req.Operations)
	if err != nil {
		return err
	}
	p, err := s.getPerson(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	res := s.userResource(p)
	cur := res["meta"].(map[string]any)["version"].(string)
	if err := s.checkIfMatch(r, cur); err != nil {
		return err
	}
	if pt.PreferredName != nil || pt.WorkPhone != nil {
		p, err = s.Dir.Update(r.Context(), p.ID, pt)
		if errors.Is(err, ErrInvalid) {
			return &SCIMError{Status: 400, Type: "invalidValue", Detail: "the directory refused the change"}
		}
		if err != nil {
			return err
		}
		fields := []string{}
		if pt.PreferredName != nil {
			fields = append(fields, "displayName")
		}
		if pt.WorkPhone != nil {
			fields = append(fields, "phoneNumbers")
		}
		// audit before the answer: a change that cannot be recorded is reported as failed
		if err := s.Store.Audit(r.Context(), "scim:"+pr.TokenID, "scim.user.patch", p.ID, map[string]any{"fields": fields}); err != nil {
			return err
		}
	}
	res = s.userResource(p)
	writeSCIM(w, 200, res, res["meta"].(map[string]any)["version"].(string))
	return nil
}

// ---- groups ------------------------------------------------------------------------------------

type groupRow struct {
	ID      string
	Name    string
	Version int64
	Created int64
	Updated int64
	Members []string
}

func (s *SCIM) groupResource(g groupRow) map[string]any {
	members := make([]map[string]any, 0, len(g.Members))
	for _, m := range g.Members {
		members = append(members, map[string]any{"value": m, "type": "User", "$ref": scimBase + "/Users/" + m})
	}
	return map[string]any{
		"schemas": []string{schemaGroup}, "id": g.ID, "displayName": g.Name, "members": members,
		"meta": map[string]any{"resourceType": "Group", "location": scimBase + "/Groups/" + g.ID, "created": ts(g.Created), "lastModified": ts(g.Updated), "version": groupETag(g)},
	}
}

func groupETag(g groupRow) string { return fmt.Sprintf(`W/"g%d"`, g.Version) }

func (st *Store) loadGroup(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id string) (groupRow, error) {
	var g groupRow
	err := q.QueryRowContext(ctx, `SELECT id, display_name, version, created_at, updated_at FROM groups WHERE id=?`, id).Scan(&g.ID, &g.Name, &g.Version, &g.Created, &g.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	if err != nil {
		return g, err
	}
	rows, err := q.QueryContext(ctx, `SELECT party_id FROM group_members WHERE group_id=? ORDER BY party_id LIMIT ?`, id, maxGroupMembers+1)
	if err != nil {
		return g, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return g, err
		}
		g.Members = append(g.Members, m)
	}
	return g, rows.Err()
}

func (s *SCIM) allGroups(ctx context.Context) ([]groupRow, error) {
	ctx, cancel := s.Store.read(ctx)
	defer cancel()
	rows, err := s.Store.db.QueryContext(ctx, `SELECT id FROM groups ORDER BY display_name LIMIT ?`, maxGroups+1)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	var out []groupRow
	for _, id := range ids {
		g, err := s.Store.loadGroup(ctx, s.Store.db, id)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func groupAttr(g groupRow) func(string) (string, bool) {
	return func(a string) (string, bool) {
		switch a {
		case "id":
			return g.ID, true
		case "displayname":
			return g.Name, true
		}
		return "", false
	}
}

func (s *SCIM) listGroups(w http.ResponseWriter, r *http.Request, _ *scimPrincipal) error {
	lp, err := parseListParams(r, groupFilterAttrs)
	if err != nil {
		return err
	}
	all, err := s.allGroups(r.Context())
	if err != nil {
		return err
	}
	var total int
	var window []any
	for _, g := range all {
		ok := true
		for _, t := range lp.filter {
			if t.Attr == "members.value" {
				ok = ok && containsStr(g.Members, t.Value)
			} else {
				ok = ok && Filter{t}.Match(groupAttr(g))
			}
		}
		if !ok {
			continue
		}
		total++
		if total >= lp.start && len(window) < lp.count {
			window = append(window, s.groupResource(g))
		}
	}
	return s.writeList(w, lp, total, window)
}

func (s *SCIM) getGroup(w http.ResponseWriter, r *http.Request, _ *scimPrincipal) error {
	g, err := s.Store.loadGroup(r.Context(), s.Store.db, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeSCIM(w, 200, s.groupResource(g), groupETag(g))
	return nil
}

type memberRef struct {
	Value string `json:"value"`
}

// checkMembers validates member ids against the directory: only people that exist and are visible.
func (s *SCIM) checkMembers(ctx context.Context, ids []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !partyRe.MatchString(id) {
			return nil, &SCIMError{Status: 400, Type: "invalidValue", Detail: "a member value is not a user id"}
		}
		if seen[id] {
			continue
		}
		if _, err := s.getPerson(ctx, id); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, &SCIMError{Status: 400, Type: "invalidValue", Detail: "a member is not a known user"}
			}
			return nil, err
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > maxGroupMembers {
		return nil, &SCIMError{Status: 400, Type: "invalidValue", Detail: fmt.Sprintf("a group has at most %d members", maxGroupMembers)}
	}
	return out, nil
}

func (s *SCIM) createGroup(w http.ResponseWriter, r *http.Request, pr *scimPrincipal) error {
	var in struct {
		Schemas     []string    `json:"schemas"`
		DisplayName string      `json:"displayName"`
		Members     []memberRef `json:"members"`
	}
	if err := decodeBody(r, &in); err != nil {
		return err
	}
	name, err := cleanGroupName(in.DisplayName)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(in.Members))
	for _, m := range in.Members {
		ids = append(ids, m.Value)
	}
	members, err := s.checkMembers(r.Context(), ids)
	if err != nil {
		return err
	}
	id := newID("grp_")
	now := s.now().Unix()
	err = s.Store.tx(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM groups`).Scan(&n); err != nil {
			return err
		}
		if n >= maxGroups {
			return &SCIMError{Status: 400, Type: "invalidValue", Detail: fmt.Sprintf("at most %d groups are supported", maxGroups)}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO groups(id,display_name,version,created_at,updated_at) VALUES (?,?,1,?,?)`, id, name, now, now); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return &SCIMError{Status: 409, Type: "uniqueness", Detail: "a group with that displayName exists"}
			}
			return err
		}
		for _, m := range members {
			if _, err := tx.ExecContext(ctx, `INSERT INTO group_members(group_id,party_id) VALUES (?,?)`, id, m); err != nil {
				return err
			}
		}
		return s.Store.audit(ctx, tx, "scim:"+pr.TokenID, "scim.group.create", id, map[string]any{"members": len(members)})
	})
	if err != nil {
		return err
	}
	s.membershipChanged(r.Context(), members)
	g, err := s.Store.loadGroup(r.Context(), s.Store.db, id)
	if err != nil {
		return err
	}
	w.Header().Set("Location", scimBase+"/Groups/"+id)
	writeSCIM(w, 201, s.groupResource(g), groupETag(g))
	return nil
}

func cleanGroupName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if !groupRe.MatchString(n) {
		return "", &SCIMError{Status: 400, Type: "invalidValue", Detail: "displayName must be 1 to 100 letters, digits, spaces or . _ : @ / + -"}
	}
	return n, nil
}

func (s *SCIM) membershipChanged(ctx context.Context, ids []string) {
	if s.OnMembership != nil && len(ids) > 0 {
		s.OnMembership(ctx, ids)
	}
}

func (s *SCIM) patchGroup(w http.ResponseWriter, r *http.Request, pr *scimPrincipal) error {
	req, err := readPatch(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	cur, err := s.Store.loadGroup(r.Context(), s.Store.db, id)
	if err != nil {
		return err
	}
	if err := s.checkIfMatch(r, groupETag(cur)); err != nil {
		return err
	}
	members := map[string]bool{}
	for _, m := range cur.Members {
		members[m] = true
	}
	name := cur.Name
	changed := map[string]bool{}
	apply := func(add bool, ids []string) error {
		ok, err := s.checkMembersMaybe(r.Context(), add, ids)
		if err != nil {
			return err
		}
		for _, m := range ok {
			if members[m] != add {
				changed[m] = true
			}
			if add {
				members[m] = true
			} else {
				delete(members, m)
			}
		}
		return nil
	}
	for _, op := range req.Operations {
		kind := strings.ToLower(op.Op)
		path := strings.TrimSpace(op.Path)
		lp := strings.ToLower(strings.ReplaceAll(path, " ", ""))
		switch {
		case lp == "displayname" && (kind == "replace" || kind == "add"):
			var v string
			if json.Unmarshal(op.Value, &v) != nil {
				return &SCIMError{Status: 400, Type: "invalidValue", Detail: "displayName must be a string"}
			}
			if name, err = cleanGroupName(v); err != nil {
				return err
			}
		case lp == "members" && kind == "add":
			ids, err := memberValues(op.Value)
			if err != nil {
				return err
			}
			if err := apply(true, ids); err != nil {
				return err
			}
		case lp == "members" && kind == "replace":
			ids, err := memberValues(op.Value)
			if err != nil {
				return err
			}
			for m := range members {
				changed[m] = true // everyone not re-listed leaves; re-listed ones are cleared below
			}
			members = map[string]bool{}
			if err := apply(true, ids); err != nil {
				return err
			}
			for _, id := range ids {
				if slices.Contains(cur.Members, id) {
					delete(changed, id)
				}
			}
		case lp == "members" && kind == "remove":
			ids, err := memberValues(op.Value)
			if err != nil || len(ids) == 0 {
				return &SCIMError{Status: 400, Type: "noTarget", Detail: "removing members needs a value list or members[value eq \"id\"]"}
			}
			if err := apply(false, ids); err != nil {
				return err
			}
		case strings.HasPrefix(lp, "members[value") && kind == "remove":
			inner := strings.TrimSpace(path[strings.Index(path, "[")+1:])
			f, err := ParseFilter(strings.TrimSuffix(inner, "]"), map[string]bool{"value": true})
			if err != nil || len(f) != 1 {
				return &SCIMError{Status: 400, Type: "invalidPath", Detail: "use members[value eq \"id\"]"}
			}
			if err := apply(false, []string{f[0].Value}); err != nil {
				return err
			}
		case path == "" && (kind == "add" || kind == "replace"):
			var obj struct {
				DisplayName *string     `json:"displayName"`
				Members     []memberRef `json:"members"`
			}
			if json.Unmarshal(op.Value, &obj) != nil {
				return &SCIMError{Status: 400, Type: "invalidValue", Detail: "the value must be an object"}
			}
			if obj.DisplayName != nil {
				if name, err = cleanGroupName(*obj.DisplayName); err != nil {
					return err
				}
			}
			ids := make([]string, 0, len(obj.Members))
			for _, m := range obj.Members {
				ids = append(ids, m.Value)
			}
			if err := apply(true, ids); err != nil {
				return err
			}
		default:
			return &SCIMError{Status: 400, Type: "invalidPath", Detail: "unsupported operation or path"}
		}
	}
	if len(members) > maxGroupMembers {
		return &SCIMError{Status: 400, Type: "invalidValue", Detail: fmt.Sprintf("a group has at most %d members", maxGroupMembers)}
	}
	err = s.Store.tx(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		// optimistic: the version must still be the one we read
		res, err := tx.ExecContext(ctx, `UPDATE groups SET display_name=?, version=version+1, updated_at=? WHERE id=? AND version=?`, name, s.now().Unix(), id, cur.Version)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return &SCIMError{Status: 409, Type: "uniqueness", Detail: "a group with that displayName exists"}
			}
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return &SCIMError{Status: 412, Detail: "the resource changed; fetch it again"}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM group_members WHERE group_id=?`, id); err != nil {
			return err
		}
		for m := range members {
			if _, err := tx.ExecContext(ctx, `INSERT INTO group_members(group_id,party_id) VALUES (?,?)`, id, m); err != nil {
				return err
			}
		}
		return s.Store.audit(ctx, tx, "scim:"+pr.TokenID, "scim.group.patch", id, map[string]any{"members": len(members), "changed": len(changed)})
	})
	if err != nil {
		return err
	}
	s.membershipChanged(r.Context(), slices.Sorted(maps.Keys(changed)))
	g, err := s.Store.loadGroup(r.Context(), s.Store.db, id)
	if err != nil {
		return err
	}
	writeSCIM(w, 200, s.groupResource(g), groupETag(g))
	return nil
}

// checkMembersMaybe validates members to add against the directory; members to remove are only
// shape-checked (a person erased from the directory must still be removable from groups).
func (s *SCIM) checkMembersMaybe(ctx context.Context, add bool, ids []string) ([]string, error) {
	if add {
		return s.checkMembers(ctx, ids)
	}
	for _, id := range ids {
		if !partyRe.MatchString(id) {
			return nil, &SCIMError{Status: 400, Type: "invalidValue", Detail: "a member value is not a user id"}
		}
	}
	return ids, nil
}

func memberValues(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var refs []memberRef
	if err := json.Unmarshal(raw, &refs); err != nil {
		var one memberRef
		if json.Unmarshal(raw, &one) != nil {
			return nil, &SCIMError{Status: 400, Type: "invalidValue", Detail: "members must be a list of {\"value\": id}"}
		}
		refs = []memberRef{one}
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Value)
	}
	return out, nil
}

func (s *SCIM) deleteGroup(w http.ResponseWriter, r *http.Request, pr *scimPrincipal) error {
	id := r.PathValue("id")
	cur, err := s.Store.loadGroup(r.Context(), s.Store.db, id)
	if err != nil {
		return err
	}
	if err := s.checkIfMatch(r, groupETag(cur)); err != nil {
		return err
	}
	err = s.Store.tx(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE id=?`, id); err != nil {
			return err
		}
		return s.Store.audit(ctx, tx, "scim:"+pr.TokenID, "scim.group.delete", id, map[string]any{"members": len(cur.Members)})
	})
	if err != nil {
		return err
	}
	s.membershipChanged(r.Context(), cur.Members)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- discovery ---------------------------------------------------------------------------------

func (s *SCIM) serviceProviderConfig(w http.ResponseWriter, _ *http.Request, _ *scimPrincipal) error {
	writeSCIM(w, 200, map[string]any{
		"schemas":          []string{schemaSPC},
		"documentationUri": "https://github.com/dodwmd/companyinacan/blob/main/docs/provisioning.md",
		"patch":            map[string]any{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": maxCount},
		"changePassword":   map[string]any{"supported": false},
		"sort":             map[string]any{"supported": false},
		"etag":             map[string]any{"supported": true},
		"authenticationSchemes": []map[string]any{{
			"type": "oauthbearertoken", "name": "Bearer token",
			"description": "A scoped bearer token created by an administrator (scim.read or scim.write).",
			"specUri":     "https://www.rfc-editor.org/rfc/rfc6750", "primary": true,
		}},
		"meta": map[string]any{"resourceType": "ServiceProviderConfig", "location": scimBase + "/ServiceProviderConfig"},
	}, "")
	return nil
}

func rtList() []map[string]any {
	return []map[string]any{
		{"schemas": []string{schemaRT}, "id": "User", "name": "User", "endpoint": "/Users", "description": "A person in the people directory (read-mostly)", "schema": schemaUser,
			"schemaExtensions": []map[string]any{{"schema": schemaEnterprise, "required": false}},
			"meta":             map[string]any{"resourceType": "ResourceType", "location": scimBase + "/ResourceTypes/User"}},
		{"schemas": []string{schemaRT}, "id": "Group", "name": "Group", "endpoint": "/Groups", "description": "An access group", "schema": schemaGroup,
			"meta": map[string]any{"resourceType": "ResourceType", "location": scimBase + "/ResourceTypes/Group"}},
	}
}

func (s *SCIM) resourceTypes(w http.ResponseWriter, _ *http.Request, _ *scimPrincipal) error {
	l := rtList()
	res := make([]any, len(l))
	for i := range l {
		res[i] = l[i]
	}
	writeSCIM(w, 200, map[string]any{"schemas": []string{schemaList}, "totalResults": len(l), "startIndex": 1, "itemsPerPage": len(l), "Resources": res}, "")
	return nil
}

func (s *SCIM) resourceType(w http.ResponseWriter, r *http.Request, _ *scimPrincipal) error {
	for _, rt := range rtList() {
		if rt["id"] == r.PathValue("id") {
			writeSCIM(w, 200, rt, "")
			return nil
		}
	}
	return ErrNotFound
}

func attr(name, typ, mut, desc string, mv bool) map[string]any {
	return map[string]any{"name": name, "type": typ, "multiValued": mv, "description": desc, "required": false, "caseExact": false, "mutability": mut, "returned": "default", "uniqueness": "none"}
}

func schemaList2() []map[string]any {
	return []map[string]any{
		{"schemas": []string{schemaSchema}, "id": schemaUser, "name": "User", "description": "A person. Read from the people directory; sensitive fields are never present.",
			"attributes": []map[string]any{
				attr("userName", "string", "readOnly", "The work email, or the person id when there is none", false),
				attr("displayName", "string", "readWrite", "Preferred name (PATCH changes the directory's preferred name)", false),
				attr("name", "complex", "readOnly", "Given and family name", false),
				attr("emails", "complex", "readOnly", "Work email", true),
				attr("phoneNumbers", "complex", "readWrite", "Work phone (PATCH changes the directory's work phone)", true),
				attr("userType", "string", "readOnly", "employee or contractor", false),
				attr("active", "boolean", "readOnly", "Whether the person currently holds access; leave counts as active", false),
			},
			"meta": map[string]any{"resourceType": "Schema", "location": scimBase + "/Schemas/" + schemaUser}},
		{"schemas": []string{schemaSchema}, "id": schemaGroup, "name": "Group", "description": "An access group whose members are directory users.",
			"attributes": []map[string]any{
				attr("displayName", "string", "readWrite", "Group name", false),
				attr("members", "complex", "readWrite", "Member user ids", true),
			},
			"meta": map[string]any{"resourceType": "Schema", "location": scimBase + "/Schemas/" + schemaGroup}},
		{"schemas": []string{schemaSchema}, "id": schemaEnterprise, "name": "EnterpriseUser", "description": "Manager only.",
			"attributes": []map[string]any{attr("manager", "complex", "readOnly", "The manager's user id", false)},
			"meta":       map[string]any{"resourceType": "Schema", "location": scimBase + "/Schemas/" + schemaEnterprise}},
	}
}

func (s *SCIM) schemas(w http.ResponseWriter, _ *http.Request, _ *scimPrincipal) error {
	l := schemaList2()
	res := make([]any, len(l))
	for i := range l {
		res[i] = l[i]
	}
	writeSCIM(w, 200, map[string]any{"schemas": []string{schemaList}, "totalResults": len(l), "startIndex": 1, "itemsPerPage": len(l), "Resources": res}, "")
	return nil
}

func (s *SCIM) schema(w http.ResponseWriter, r *http.Request, _ *scimPrincipal) error {
	id, _ := url.PathUnescape(r.PathValue("id"))
	for _, sc := range schemaList2() {
		if sc["id"] == id {
			writeSCIM(w, 200, sc, "")
			return nil
		}
	}
	return ErrNotFound
}
