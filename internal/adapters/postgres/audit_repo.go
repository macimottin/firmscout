package postgres

import (
	"context"

	"github.com/macimottin/firmscout/internal/application"
)

// AuditRepo is the PostgreSQL implementation of application.AuditRepository.
//
// audit_events has existed since the initial migration with exactly the right columns
// (Blueprint §16: "the audit trail of who approved it") and nothing has ever written to
// it. This phase's review decisions are its first writer.
type AuditRepo struct{ db *DB }

var _ application.AuditRepository = (*AuditRepo)(nil)

// NewAuditRepo returns an audit repository bound to db.
func NewAuditRepo(db *DB) *AuditRepo { return &AuditRepo{db: db} }

const auditColumns = `
    id, actor_type, actor_id, actor_authenticated, action, subject_type, subject_id,
    before_state, after_state, reason, request_id, trace_id, occurred_at`

// Record persists one audit event.
//
// actor_authenticated is written exactly as the caller set it, never hardcoded here:
// this phase always calls it with false (there is no login to verify against -- see
// ADR-0021), but the column exists precisely so a future authenticated caller is not
// forced through the same honestly-false path.
func (r *AuditRepo) Record(ctx context.Context, e application.AuditEvent) error {
	id := e.ID
	if id == "" {
		id = r.db.newID("aud")
	}
	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO audit_events (
            id, actor_type, actor_id, actor_authenticated, action, subject_type, subject_id,
            before_state, after_state, reason, request_id, trace_id, occurred_at
         ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
            COALESCE($13::timestamptz, now())
         )`,
		id, e.ActorType, nullString(e.ActorID), e.ActorAuthenticated, e.Action,
		e.SubjectType, e.SubjectID, e.BeforeState, e.AfterState, nullString(e.Reason),
		nullString(e.RequestID), nullString(e.TraceID), nullTime(e.OccurredAt))
	return wrap("audit.Record", err)
}

// ListForSubject returns the recorded decisions against one subject, most recent first,
// capped at limit. It is what lets a reviewer see "who last touched this" without a
// separate admin tool.
func (r *AuditRepo) ListForSubject(ctx context.Context, subjectType, subjectID string, limit int) ([]application.AuditEvent, error) {
	limit = clampLimit(limit, 50, 500)
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT`+auditColumns+`
           FROM audit_events
          WHERE subject_type = $1 AND subject_id = $2
          ORDER BY occurred_at DESC
          LIMIT $3`, subjectType, subjectID, limit)
	if err != nil {
		return nil, wrap("audit.ListForSubject", err)
	}
	defer rows.Close()

	out := make([]application.AuditEvent, 0, limit)
	for rows.Next() {
		var (
			e         application.AuditEvent
			actorID   *string
			reason    *string
			requestID *string
			traceID   *string
			before    map[string]string
			after     map[string]string
		)
		if err := rows.Scan(
			&e.ID, &e.ActorType, &actorID, &e.ActorAuthenticated, &e.Action,
			&e.SubjectType, &e.SubjectID, &before, &after, &reason, &requestID, &traceID,
			&e.OccurredAt,
		); err != nil {
			return nil, wrap("audit.ListForSubject.scan", err)
		}
		e.ActorID = str(actorID)
		e.BeforeState = before
		e.AfterState = after
		e.Reason = str(reason)
		e.RequestID = str(requestID)
		e.TraceID = str(traceID)
		out = append(out, e)
	}
	return out, wrap("audit.ListForSubject.rows", rows.Err())
}
