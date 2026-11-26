package work

import (
	"context"
	"time"
)

// EventPriorityChanged (SPEC.md section 5, `issue.priority_changed`) is published when the priority
// of an existing issue changes, next to work.issue.updated.v1, so a consumer that cares about
// priority need not read every update.
const EventPriorityChanged = "work.issue.priority_changed.v1"

type priorityChangedData struct {
	IssueKey  string    `json:"issue_key"`
	Project   string    `json:"project"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Version   int64     `json:"version"`
	Actor     string    `json:"actor"`
	Assignee  *string   `json:"assignee"`
	ChangedAt time.Time `json:"changed_at"`
}

// priorityChanged records when the issue got its new priority (for the ageing view) and queues the
// event, in the caller's transaction.
func (s *Store) priorityChanged(ctx context.Context, tx dbq, a *Principal, id int64, from string, out *Issue) error {
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET priority_since = ? WHERE id = ?`, ts(out.UpdatedAt), id); err != nil {
		return err
	}
	return s.emit(ctx, tx, EventPriorityChanged, priorityChangedData{
		IssueKey: out.Key, Project: out.Project, From: from, To: out.Priority, Version: out.Version,
		Actor: a.Subject, Assignee: out.Assignee, ChangedAt: out.UpdatedAt,
	})
}
