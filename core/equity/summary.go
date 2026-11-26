package equity

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// The `summary` capability (specs/053-business-overview): how many grants wait for approval and how
// many active grants have vesting due soon. Counts only: no holder, quantity or price leaves the
// module. Only someone who may list grants across holders gets them (a holder's own vesting is not
// reported), and the read is audited.

// vestingHorizonDays is how far ahead vesting is "due".
const vestingHorizonDays = 30

// SummaryCounts counts the grants waiting for approval and the active grants whose vested quantity
// increases between today and 30 days from now.
func (s *Service) SummaryCounts(ctx context.Context, a Actor) (pending, vesting int64, err error) {
	if !Decide(a, ActGrantList, Fact{}) {
		return 0, 0, ErrForbidden
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM grants WHERE status = ?`, GrantPending).Scan(&pending); err != nil {
		return 0, 0, err
	}
	if vesting, err = s.vestingDue(ctx); err != nil {
		return 0, 0, err
	}
	if err := s.auditRead(ctx, a, "grants/summary-counts", []string{FieldGrantTerms}); err != nil {
		return 0, 0, err
	}
	return pending, vesting, nil
}

// vestingDue reads the active grants and their recorded facts in three queries (not per grant) and
// applies the vesting calculator to each.
func (s *Service) vestingDue(ctx context.Context) (int64, error) {
	t, err := time.Parse(dateLayout, s.today())
	if err != nil {
		return 0, err
	}
	// Vesting dated today is still due: compare with the day before.
	before, until := t.AddDate(0, 0, -1).Format(dateLayout), t.AddDate(0, 0, vestingHorizonDays).Format(dateLayout)

	each := func(q string, row func(scan func(...any) error) error) error {
		rows, err := s.db.QueryContext(ctx, q, GrantActive)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			if err := row(rows.Scan); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	achieved := map[string]map[int]string{}
	if err := each(`SELECT m.grant_id, m.tranche_idx, m.achieved_on FROM grant_milestones m JOIN grants g ON g.id = m.grant_id WHERE g.status = ?`, func(scan func(...any) error) error {
		var id, on string
		var i int
		if err := scan(&id, &i, &on); err != nil {
			return err
		}
		if achieved[id] == nil {
			achieved[id] = map[int]string{}
		}
		achieved[id][i] = on
		return nil
	}); err != nil {
		return 0, err
	}
	events := map[string][]AccelEvent{}
	if err := each(`SELECT e.grant_id, e.trigger, e.occurred_on FROM grant_accel_events e JOIN grants g ON g.id = e.grant_id WHERE g.status = ?`, func(scan func(...any) error) error {
		var id string
		var e AccelEvent
		if err := scan(&id, &e.Trigger, &e.Date); err != nil {
			return err
		}
		events[id] = append(events[id], e)
		return nil
	}); err != nil {
		return 0, err
	}
	var n int64
	err = each(`SELECT id, quantity, vest_start, schedule, COALESCE(termination_date, '') FROM grants WHERE status = ?`, func(scan func(...any) error) error {
		var id, sched string
		var qty int64
		in := VestInput{AsOf: before}
		if err := scan(&id, &qty, &in.Start, &sched, &in.Termination); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(sched), &in.Sched); err != nil {
			return err
		}
		in.Total, in.Achieved, in.Events = Qty(qty), achieved[id], events[id]
		was, err := Vested(in)
		if err != nil {
			return err
		}
		in.AsOf = until
		will, err := Vested(in)
		if err != nil {
			return err
		}
		if will > was {
			n++
		}
		return nil
	})
	return n, err
}

// summaryBuilder answers for a person as can-core describes them, with the same mapping to roles as
// their personal access tokens and launch tickets (canRoles): the board only with equity.board. The
// answer is counts only, and nothing can be approved through it.
func summaryBuilder(s *Service) modclient.SummaryBuilder {
	return func(ctx context.Context, subject string, permissions []string) ([]modclient.SummaryFigure, error) {
		a := Actor{Subject: subject, Roles: rolesFromPermissions(permissions), CanCore: true}
		if !canSubjectRe.MatchString(subject) || !Decide(a, ActGrantList, Fact{}) {
			return nil, nil
		}
		pending, vesting, err := s.SummaryCounts(ctx, a)
		if err != nil {
			return nil, err
		}
		return []modclient.SummaryFigure{{Key: "grants.pending_approval", Count: pending}, {Key: "vesting.upcoming", Count: vesting}}, nil
	}
}

// withSummary adds GET /v1/summary to the handler of an installed module.
func withSummary(next http.Handler, can *modclient.Client, s *Service, log *slog.Logger, limit modclient.SummaryLimit) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(modclient.SummaryPath, modclient.SummaryHandler(can, log, limit, summaryBuilder(s)))
	mux.Handle("/", next)
	return mux
}

// summaryLimit counts failures on /v1/summary like every other failed authentication.
func (a *API) summaryLimit() modclient.SummaryLimit {
	return modclient.SummaryLimit{
		Client: clientHost,
		Failed: func(ip string) bool { a.limiters(); return a.failIP.allow(ip) },
	}
}
