package adminapi

import (
	"errors"
	"net/http"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/notify/webpush"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Web Push registration (epic E19a). Contract: specs/005-can-core-mvp/contracts/admin-api.yaml,
// specs/025-pwa-webpush. Every route needs a session; the unsafe ones need the CSRF header.

const maxPushBody = 8 << 10

func (s *Server) pushRoutes() []route {
	return []route{
		{"GET /api/v1/push/vapid-public-key", s.authed(s.vapidPublicKey)},
		{"GET /api/v1/push/subscriptions", s.authed(s.listPushSubscriptions)},
		{"POST /api/v1/push/subscriptions", s.authed(s.addPushSubscription)},
		{"DELETE /api/v1/push/subscriptions", s.authed(s.deletePushSubscription)},
		{"POST /api/v1/push/test", s.authed(s.pushTest)},
		{"POST /api/v1/push/vapid-key/rotate", s.authedPerm(rbac.PermManage, s.rotateVAPID)},
	}
}

func (s *Server) vapidPublicKey(w http.ResponseWriter, r *http.Request, _ principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	k, err := s.Notify.VAPIDPublicKey(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"public_key": k})
}

func (s *Server) listPushSubscriptions(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	d, err := s.Notify.Subscriptions(r.Context(), p.Subject())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"subscriptions": d})
}

// pushSubscriptionBody is PushSubscription.toJSON() as the browser produces it.
type pushSubscriptionBody struct {
	Endpoint       string `json:"endpoint"`
	ExpirationTime *int64 `json:"expirationTime"`
	Keys           struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (s *Server) addPushSubscription(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	var b pushSubscriptionBody
	if !httpx.DecodeJSONMax(w, r, &b, maxPushBody) {
		return
	}
	id, err := s.Notify.Subscribe(r.Context(), p.Subject(), webpush.Subscription{Endpoint: b.Endpoint, P256dh: b.Keys.P256dh, Auth: b.Keys.Auth}, notify.DeviceLabel(r.UserAgent()))
	switch {
	case errors.Is(err, notify.ErrTooManySubscriptions):
		httpx.Problem(w, http.StatusConflict, "this account already has the maximum number of push devices; remove one first")
	case notify.ErrInvalid(err):
		httpx.Problem(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.internal(w, r, err)
	default:
		httpx.WriteJSON(w, http.StatusCreated, map[string]int64{"id": id})
	}
}

func (s *Server) deletePushSubscription(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	var b struct {
		Endpoint string `json:"endpoint"`
	}
	if !httpx.DecodeJSONMax(w, r, &b, maxPushBody) {
		return
	}
	switch err := s.Notify.Unsubscribe(r.Context(), p.Subject(), b.Endpoint); {
	case errors.Is(err, store.ErrNotFound):
		httpx.Problem(w, http.StatusNotFound, "no such push subscription")
	case err != nil:
		s.internal(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) pushTest(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	n, err := s.Notify.SendTest(r.Context(), p.Subject())
	switch {
	case errors.Is(err, notify.ErrNoSubscriptions):
		httpx.Problem(w, http.StatusConflict, "no push device is registered for this account")
	case errors.Is(err, notify.ErrPushRateLimited):
		w.Header().Set("Retry-After", "30")
		httpx.Problem(w, http.StatusTooManyRequests, "one test notification every 30 seconds")
	case err != nil:
		s.Log.Error("test push failed", "request_id", httpx.RequestID(r.Context()), "error", err)
		httpx.Problem(w, http.StatusBadGateway, "no device accepted the test notification")
	default:
		httpx.WriteJSON(w, http.StatusOK, map[string]int{"sent": n})
	}
}

func (s *Server) rotateVAPID(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Notify == nil {
		s.unavailable(w)
		return
	}
	var b struct{}
	if !httpx.DecodeJSONMax(w, r, &b, 64) {
		return
	}
	if err := s.Notify.RotateVAPID(r.Context()); errors.Is(err, notify.ErrKeyFile) {
		httpx.Problem(w, http.StatusConflict, err.Error())
		return
	} else if err != nil {
		s.internal(w, r, err)
		return
	}
	if _, err := s.Audit.Append(r.Context(), audit.Entry{Actor: p.User.Username, Action: "push.vapid.rotate", Target: "vapid"}); err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
