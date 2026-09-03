package postgres

import (
	"context"
	"time"

	"github.com/macimottin/firmscout/internal/application"
)

// ReviewRepo is the PostgreSQL implementation of application.ReviewRepository.
//
// The review queue is where FirmScout puts everything it refuses to guess at: an
// ambiguous product match, an implausible version transition, a source that started
// redirecting off its registered host. Its value depends on staying short, which is
// why priority and SLA class are columns and the open-queue index carries them.
type ReviewRepo struct{ db *DB }

var _ application.ReviewRepository = (*ReviewRepo)(nil)

// NewReviewRepo returns a review repository bound to db.
func NewReviewRepo(db *DB) *ReviewRepo { return &ReviewRepo{db: db} }

const reviewColumns = `
    id, kind, subject_type, subject_id, vendor_id, product_id, title, detail, payload,
    priority_score, sla_class, created_at`

// Create enqueues an item for a human decision.
//
// The payload travels as JSONB rather than as a rendered string so a reviewer UI can
// present the specific fields -- the four products an alias matched, the two versions
// that disagree -- rather than a paragraph a human has to parse.
func (r *ReviewRepo) Create(ctx context.Context, item application.ReviewItem) error {
	id := item.ID
	if id == "" {
		id = r.db.newID("rev")
	}
	payload := item.Payload
	if payload == nil {
		payload = map[string]string{}
	}
	slaClass := item.SLAClass
	if slaClass == "" {
		slaClass = "standard"
	}

	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO review_items (
            id, kind, subject_type, subject_id, vendor_id, product_id, title, detail,
            payload, priority_score, sla_class, created_at, updated_at
         ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
            COALESCE($12::timestamptz, now()), now()
         )`,
		id, item.Kind, item.SubjectType, item.SubjectID, nullString(item.VendorID),
		nullString(item.ProductID), item.Title, nullString(item.Detail), payload,
		item.PriorityScore, slaClass, nullTime(item.CreatedAt))
	return wrap("review.Create", err)
}

// ListOpen returns the open queue, highest priority first and oldest first within a
// priority, capped at limit.
//
// In-progress items are included: an item somebody started and did not finish is still
// an open decision, and hiding it is how work gets silently dropped. The ordering
// matches review_items_queue_idx.
func (r *ReviewRepo) ListOpen(ctx context.Context, limit int) ([]application.ReviewItem, error) {
	limit = clampLimit(limit, 50, 500)
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT`+reviewColumns+`
           FROM review_items
          WHERE state IN ('open', 'in_progress')
          ORDER BY priority_score DESC, created_at
          LIMIT $1`, limit)
	if err != nil {
		return nil, wrap("review.ListOpen", err)
	}
	defer rows.Close()

	out := make([]application.ReviewItem, 0, limit)
	for rows.Next() {
		var (
			item      application.ReviewItem
			vendorID  *string
			productID *string
			detail    *string
			payload   map[string]string
		)
		if err := rows.Scan(
			&item.ID, &item.Kind, &item.SubjectType, &item.SubjectID, &vendorID,
			&productID, &item.Title, &detail, &payload, &item.PriorityScore,
			&item.SLAClass, &item.CreatedAt,
		); err != nil {
			return nil, wrap("review.ListOpen.scan", err)
		}
		item.VendorID = str(vendorID)
		item.ProductID = str(productID)
		item.Detail = str(detail)
		item.Payload = payload
		out = append(out, item)
	}
	return out, wrap("review.ListOpen.rows", rows.Err())
}

// Resolve closes a review item, recording the decision, who made it and when.
//
// It refuses to re-resolve an already-resolved or dismissed item -- the WHERE clause
// restricts to the open states and a zero row count becomes domain.ErrNotFound -- so
// two reviewers racing on the same item produce one decision and one visible failure
// rather than a silently overwritten verdict.
func (r *ReviewRepo) Resolve(ctx context.Context, id, resolution, resolvedBy string, at time.Time) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE review_items
            SET state       = 'resolved',
                resolution  = $2,
                resolved_by = $3,
                resolved_at = COALESCE($4::timestamptz, now()),
                updated_at  = now()
          WHERE id = $1 AND state IN ('open', 'in_progress')`,
		id, nullString(resolution), nullString(resolvedBy), nullTime(at))
	if err != nil {
		return wrap("review.Resolve", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("review.Resolve")
	}
	return nil
}
