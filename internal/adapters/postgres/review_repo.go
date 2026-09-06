package postgres

import (
	"context"
	"strconv"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// ReviewRepo is the PostgreSQL implementation of application.ReviewRepository.
//
// The review queue is where FirmScout puts everything it refuses to guess at: an
// ambiguous product match, an implausible version transition, a source that started
// redirecting off its registered host, two sources that disagree about a version. Its
// value depends on staying short, which is why priority and SLA class are columns and
// the open-queue index carries them.
type ReviewRepo struct{ db *DB }

var _ application.ReviewRepository = (*ReviewRepo)(nil)

// NewReviewRepo returns a review repository bound to db.
func NewReviewRepo(db *DB) *ReviewRepo { return &ReviewRepo{db: db} }

const reviewColumns = `
    id, kind, subject_type, subject_id, vendor_id, product_id, title, detail, payload,
    priority_score, sla_class, state, resolution, assigned_to, resolved_by, resolved_at,
    created_at, updated_at`

// scanReviewItem reads one review_items row in full. GetByID and List share it so a
// reader of either always sees the same shape of item.
func scanReviewItem(row interface{ Scan(...any) error }) (application.ReviewItem, error) {
	var (
		item       application.ReviewItem
		vendorID   *string
		productID  *string
		detail     *string
		payload    map[string]string
		state      string
		resolution *string
		assignedTo *string
		resolvedBy *string
		resolvedAt *time.Time
	)
	if err := row.Scan(
		&item.ID, &item.Kind, &item.SubjectType, &item.SubjectID, &vendorID, &productID,
		&item.Title, &detail, &payload, &item.PriorityScore, &item.SLAClass, &state,
		&resolution, &assignedTo, &resolvedBy, &resolvedAt, &item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return application.ReviewItem{}, err
	}
	item.VendorID = str(vendorID)
	item.ProductID = str(productID)
	item.Detail = str(detail)
	item.Payload = payload
	item.State = state
	item.Resolution = str(resolution)
	item.AssignedTo = str(assignedTo)
	item.ResolvedBy = str(resolvedBy)
	item.ResolvedAt = tim(resolvedAt)
	return item, nil
}

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

// GetByID returns the review item with the given id, or domain.ErrNotFound.
func (r *ReviewRepo) GetByID(ctx context.Context, id string) (application.ReviewItem, error) {
	row := r.db.q(ctx).QueryRow(ctx, `SELECT`+reviewColumns+` FROM review_items WHERE id = $1`, id)
	item, err := scanReviewItem(row)
	if err != nil {
		return application.ReviewItem{}, wrap("review.GetByID", err)
	}
	return item, nil
}

// FindOpenBySubject returns the item already waiting on a decision about a subject, or
// domain.ErrNotFound.
//
// review_items_open_subject_idx (00003) makes at most one row match, which is the whole
// point: without it, a validation job delivered twice filed a second item and the caller
// had no way to notice. The ORDER BY is not redundant with the index -- it decides which
// row this returns for any subject whose duplicates predate the index -- and LIMIT 1
// keeps that case a stable answer rather than a query that fails.
func (r *ReviewRepo) FindOpenBySubject(ctx context.Context, subjectType, subjectID string) (application.ReviewItem, error) {
	row := r.db.q(ctx).QueryRow(ctx,
		`SELECT`+reviewColumns+`
           FROM review_items
          WHERE subject_type = $1 AND subject_id = $2
            AND state IN ('open', 'in_progress')
          ORDER BY created_at ASC, id ASC
          LIMIT 1`, subjectType, subjectID)
	item, err := scanReviewItem(row)
	if err != nil {
		return application.ReviewItem{}, wrap("review.FindOpenBySubject", err)
	}
	return item, nil
}

// Retarget rewrites an open item so it describes the decision as it stands now.
//
// Everything a reviewer reads is rewritten; nothing that locates the item in the queue
// is. created_at stays put because the item's age belongs to the disagreement, not to
// whichever candidate last surfaced it, and assigned_to stays put because reassigning
// somebody's work is not a side effect a scheduler run may have.
//
// The WHERE clause restricts to the open states for the same reason Resolve's does: an
// item a human has already decided is not something the pipeline may quietly reword, and
// a zero row count becomes domain.ErrNotFound rather than a silent no-op.
func (r *ReviewRepo) Retarget(ctx context.Context, item application.ReviewItem) error {
	payload := item.Payload
	if payload == nil {
		payload = map[string]string{}
	}
	slaClass := item.SLAClass
	if slaClass == "" {
		slaClass = "standard"
	}

	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE review_items
            SET kind           = $2,
                subject_type   = $3,
                subject_id     = $4,
                vendor_id      = $5,
                product_id     = $6,
                title          = $7,
                detail         = $8,
                payload        = $9,
                priority_score = $10,
                sla_class      = $11,
                updated_at     = now()
          WHERE id = $1 AND state IN ('open', 'in_progress')`,
		item.ID, item.Kind, item.SubjectType, item.SubjectID, nullString(item.VendorID),
		nullString(item.ProductID), item.Title, nullString(item.Detail), payload,
		item.PriorityScore, slaClass)
	if err != nil {
		return wrap("review.Retarget", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("review.Retarget")
	}
	return nil
}

// nullStringSlice maps an empty slice to nil so an optional array filter can be tested
// in SQL with "$n::text[] IS NULL" rather than needing a separate boolean flag per
// filter alongside it.
func nullStringSlice(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// List returns a filtered, cursor-paginated page of the queue.
//
// The ordering -- priority_score DESC, created_at ASC, id ASC -- matches
// review_items_queue_idx exactly, so the index that exists to keep this query fast is
// also the index that defines what "the queue" means. Because the leading column sorts
// descending while the tie-breakers sort ascending, the keyset predicate cannot be a
// single row comparison; it is spelled out as the three OR'd conditions a mixed-order
// cursor requires.
//
// f.States defaults to {"open", "in_progress"}: an item somebody started and did not
// finish is still an open decision, and hiding it is how work gets silently dropped.
func (r *ReviewRepo) List(ctx context.Context, f application.ReviewQueueFilter) ([]application.ReviewItem, string, error) {
	limit := clampLimit(f.Limit, application.DefaultReviewPageSize, application.MaxReviewPageSize)
	states := f.States
	if len(states) == 0 {
		states = []string{application.ReviewStateOpen, application.ReviewStateInProgress}
	}

	parts, err := decodeCursor(f.Cursor, 3)
	if err != nil {
		return nil, "", err
	}
	var (
		afterPriority *int
		afterCreated  *time.Time
		afterID       *string
	)
	if parts != nil {
		p, perr := strconv.Atoi(parts[0])
		if perr != nil {
			return nil, "", wrap("review.List.cursor", domain.ErrValidation)
		}
		t, terr := time.Parse(time.RFC3339Nano, parts[1])
		if terr != nil {
			return nil, "", wrap("review.List.cursor", domain.ErrValidation)
		}
		id := parts[2]
		afterPriority = &p
		afterCreated = &t
		afterID = &id
	}

	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT`+reviewColumns+`
           FROM review_items
          WHERE state = ANY($1::text[])
            AND ($2::text[] IS NULL OR kind = ANY($2::text[]))
            AND ($3::text[] IS NULL OR sla_class = ANY($3::text[]))
            AND ($4::text = '' OR vendor_id = $4)
            AND ($5::text = '' OR product_id = $5)
            AND (
                $6::int IS NULL
                OR priority_score < $6
                OR (priority_score = $6 AND created_at > $7::timestamptz)
                OR (priority_score = $6 AND created_at = $7::timestamptz AND id > $8::text)
            )
          ORDER BY priority_score DESC, created_at ASC, id ASC
          LIMIT $9`,
		states, nullStringSlice(f.Kinds), nullStringSlice(f.SLAClasses),
		f.VendorID, f.ProductID, afterPriority, afterCreated, afterID, limit)
	if err != nil {
		return nil, "", wrap("review.List", err)
	}
	defer rows.Close()

	out := make([]application.ReviewItem, 0, limit)
	for rows.Next() {
		item, err := scanReviewItem(rows)
		if err != nil {
			return nil, "", wrap("review.List.scan", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", wrap("review.List.rows", err)
	}

	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(strconv.Itoa(last.PriorityScore),
			last.CreatedAt.UTC().Format(time.RFC3339Nano), last.ID)
	}
	return out, next, nil
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
