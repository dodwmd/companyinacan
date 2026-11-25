package people

import (
	"context"
	"net/http"
)

// routesE14b registers the review and time-off routes (contract:
// specs/012-people-directory/contracts/people-api.yaml, extended by specs/022-reviews-timeoff).
// No handler sets an ETag or Last-Modified header: a validator derived from feedback could tell a
// reader when an answer arrived.
func (a *API) routesE14b(route func(pattern string, fn handlerFn)) {
	items := func(v any, err error) (int, any, error) {
		if err != nil {
			return 0, nil, err
		}
		return http.StatusOK, map[string]any{"items": v}, nil
	}

	// ---- reviews
	route("GET /v1/review-templates", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return items(a.Svc.ListReviewTemplates(ctx, ac))
	})
	route("POST /v1/review-templates", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in TemplateIn
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		t, err := a.Svc.CreateReviewTemplate(ctx, ac, in)
		return http.StatusCreated, t, err
	})
	route("GET /v1/review-cycles", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return items(a.Svc.ListCycles(ctx, ac))
	})
	route("POST /v1/review-cycles", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in CycleIn
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		c, err := a.Svc.CreateCycle(ctx, ac, in)
		return http.StatusCreated, c, err
	})
	route("GET /v1/review-cycles/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		c, err := a.Svc.GetCycle(ctx, ac, r.PathValue("id"))
		return http.StatusOK, c, err
	})
	route("POST /v1/review-cycles/{id}/participants", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			PersonIDs []string `json:"person_ids"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		ids, err := a.Svc.AddParticipants(ctx, ac, r.PathValue("id"), in.PersonIDs)
		return http.StatusOK, map[string]any{"review_ids": ids}, err
	})
	route("POST /v1/review-cycles/{id}/state", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			State string `json:"state"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		c, err := a.Svc.SetCycleState(ctx, ac, r.PathValue("id"), in.State)
		return http.StatusOK, c, err
	})
	route("GET /v1/reviews", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return items(a.Svc.ListReviews(ctx, ac, r.URL.Query().Get("cycle_id")))
	})
	route("GET /v1/reviews/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		v, err := a.Svc.GetReview(ctx, ac, r.PathValue("id"))
		return http.StatusOK, v, err
	})
	route("PUT /v1/reviews/{id}/self", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Answers Answers `json:"answers"`
			Submit  bool    `json:"submit"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Svc.SaveSelf(ctx, ac, r.PathValue("id"), in.Answers, in.Submit)
	})
	route("PUT /v1/reviews/{id}/manager", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in ManagerIn
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Svc.SaveManager(ctx, ac, r.PathValue("id"), in)
	})
	route("PUT /v1/reviews/{id}/calibration", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Rating int    `json:"rating"`
			Notes  string `json:"notes"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Svc.Calibrate(ctx, ac, r.PathValue("id"), in.Rating, in.Notes)
	})
	route("GET /v1/reviews/{id}/nominations", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return items(a.Svc.ListNominations(ctx, ac, r.PathValue("id")))
	})
	route("POST /v1/reviews/{id}/nominations", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			ReviewerID string `json:"reviewer_id"`
			Kind       string `json:"kind"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		n, err := a.Svc.Nominate(ctx, ac, r.PathValue("id"), in.ReviewerID, in.Kind)
		return http.StatusCreated, n, err
	})
	route("POST /v1/review-nominations/{id}/decision", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Approve bool `json:"approve"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Svc.DecideNomination(ctx, ac, r.PathValue("id"), in.Approve)
	})
	route("POST /v1/reviews/{id}/release-request", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return http.StatusNoContent, nil, a.Svc.RequestRelease(ctx, ac, r.PathValue("id"))
	})
	route("POST /v1/reviews/{id}/release-decision", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Approve bool   `json:"approve"`
			Reason  string `json:"reason"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Svc.DecideRelease(ctx, ac, r.PathValue("id"), in.Approve, in.Reason)
	})
	route("POST /v1/reviews/{id}/acknowledge", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Comment string `json:"comment"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Svc.Acknowledge(ctx, ac, r.PathValue("id"), in.Comment)
	})
	route("GET /v1/feedback-requests", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return items(a.Svc.MyFeedbackRequests(ctx, ac))
	})
	route("POST /v1/feedback-requests/{id}/answers", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Answers Answers `json:"answers"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Svc.SubmitFeedback(ctx, ac, r.PathValue("id"), in.Answers)
	})

	// ---- time off
	route("GET /v1/leave-types", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return items(a.Svc.ListLeaveTypes(ctx, ac))
	})
	route("POST /v1/leave-types", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in LeaveTypeIn
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		t, err := a.Svc.CreateLeaveType(ctx, ac, in)
		return http.StatusCreated, t, err
	})
	route("PATCH /v1/leave-types/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in LeaveTypePatch
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		t, err := a.Svc.UpdateLeaveType(ctx, ac, r.PathValue("id"), in)
		return http.StatusOK, t, err
	})
	route("GET /v1/holiday-calendars", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return items(a.Svc.ListCalendars(ctx, ac))
	})
	route("POST /v1/holiday-calendars", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in CalendarIn
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		c, err := a.Svc.SaveCalendar(ctx, ac, in)
		return http.StatusOK, c, err
	})
	route("GET /v1/blackouts", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return items(a.Svc.ListBlackouts(ctx, ac))
	})
	route("POST /v1/blackouts", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in BlackoutIn
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		b, err := a.Svc.CreateBlackout(ctx, ac, in)
		return http.StatusCreated, b, err
	})
	route("DELETE /v1/blackouts/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return http.StatusNoContent, nil, a.Svc.DeleteBlackout(ctx, ac, r.PathValue("id"))
	})
	route("GET /v1/leave-requests", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		q := r.URL.Query()
		return items(a.Svc.ListLeaveRequests(ctx, ac, LeaveFilter{PersonID: q.Get("person_id"), Status: q.Get("status")}))
	})
	route("POST /v1/leave-requests", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in LeaveRequestIn
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		lr, err := a.Svc.CreateLeaveRequest(ctx, ac, in)
		return http.StatusCreated, lr, err
	})
	route("GET /v1/leave-requests/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		lr, err := a.Svc.GetLeaveRequest(ctx, ac, r.PathValue("id"))
		return http.StatusOK, lr, err
	})
	route("PATCH /v1/leave-requests/{id}", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in LeaveRequestIn
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		lr, err := a.Svc.UpdateLeaveRequest(ctx, ac, r.PathValue("id"), in)
		return http.StatusOK, lr, err
	})
	route("POST /v1/leave-requests/{id}/submit", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		lr, err := a.Svc.SubmitLeaveRequest(ctx, ac, r.PathValue("id"))
		return http.StatusOK, lr, err
	})
	route("POST /v1/leave-requests/{id}/decision", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			Approve bool `json:"approve"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		lr, err := a.Svc.DecideLeaveRequest(ctx, ac, r.PathValue("id"), in.Approve)
		return http.StatusOK, lr, err
	})
	route("POST /v1/leave-requests/{id}/cancel", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		lr, err := a.Svc.CancelLeaveRequest(ctx, ac, r.PathValue("id"))
		return http.StatusOK, lr, err
	})
	route("GET /v1/people/{id}/leave-balances", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return items(a.Svc.LeaveBalances(ctx, ac, r.PathValue("id"), r.URL.Query().Get("as_of")))
	})
	route("GET /v1/people/{id}/leave-ledger", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		return items(a.Svc.LeaveLedger(ctx, ac, r.PathValue("id"), r.URL.Query().Get("type_id")))
	})
	route("POST /v1/people/{id}/leave-adjustments", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		var in struct {
			TypeID string `json:"type_id"`
			Halves int    `json:"halves"`
			Reason string `json:"reason"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		e, err := a.Svc.AdjustLeave(ctx, ac, r.PathValue("id"), in.TypeID, in.Halves, in.Reason)
		return http.StatusCreated, e, err
	})
	route("GET /v1/timeoff/calendar", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		q := r.URL.Query()
		return items(a.Svc.LeaveCalendar(ctx, ac, q.Get("from"), q.Get("to"), q.Get("team_id")))
	})
	route("POST /v1/timeoff/accruals/run", func(ctx context.Context, ac Actor, r *http.Request) (int, any, error) {
		n, err := a.Svc.RunAccruals(ctx, ac)
		return http.StatusOK, map[string]int{"entries": n}, err
	})
}
