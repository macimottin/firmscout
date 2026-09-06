package postgres

import (
	"context"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// ConflictRepo is the PostgreSQL implementation of application.ConflictRepository.
//
// It holds the two tables that make a multi-source disagreement visible instead of
// silently resolved: source_observations (what each source currently claims) and
// source_conflicts (the disagreements the authority ladder could not settle). See
// ADR-0020 for why the ladder itself never runs in SQL.
type ConflictRepo struct{ db *DB }

var _ application.ConflictRepository = (*ConflictRepo)(nil)

// NewConflictRepo returns a conflict repository bound to db.
func NewConflictRepo(db *DB) *ConflictRepo { return &ConflictRepo{db: db} }

// ---------------------------------------------------------------------------
// Observations
// ---------------------------------------------------------------------------

// RecordObservation writes what a source currently reports for a product and channel,
// replacing that source's previous row.
//
// The caller has already decided this is the later observation (domain.LaterObservation);
// this method only stores it. The ON CONFLICT branch sets every claim column and
// observed_at, and deliberately leaves first_observed_at alone -- it is the moment this
// source first made this claim, not the moment it was last confirmed.
func (r *ConflictRepo) RecordObservation(ctx context.Context, obs domain.SourceObservation) error {
	if err := obs.Validate(); err != nil {
		return err
	}
	releaseDate, precision := dateParams(obs.ReleaseDate)

	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO source_observations (
            id, source_id, product_id, channel, raw_version, normalized_version,
            release_date, release_date_precision, candidate_id, evidence_id,
            observed_at, first_observed_at, updated_at
         ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
            COALESCE($11::timestamptz, now()), COALESCE($12::timestamptz, now()), now()
         )
         ON CONFLICT (source_id, product_id, channel) DO UPDATE SET
            raw_version            = EXCLUDED.raw_version,
            normalized_version     = EXCLUDED.normalized_version,
            release_date           = EXCLUDED.release_date,
            release_date_precision = EXCLUDED.release_date_precision,
            candidate_id           = EXCLUDED.candidate_id,
            evidence_id            = EXCLUDED.evidence_id,
            observed_at            = EXCLUDED.observed_at,
            updated_at             = now()`,
		r.db.newID("sobs"), obs.SourceID, obs.ProductID, obs.Channel,
		obs.RawVersion, obs.NormalizedVersion, releaseDate, precision,
		nullString(obs.CandidateID), nullString(obs.EvidenceID),
		nullTime(obs.ObservedAt), nullTime(obs.FirstObservedAt))
	return wrap("conflict.RecordObservation", err)
}

// scanObservation reads one row of the ObservationsForProduct join. Authority and
// eligibility come from the joined sources row rather than from source_observations
// itself, so a source that is reclassified or disabled stops counting immediately
// instead of waiting for its next observation to be recorded.
func scanObservation(row interface{ Scan(...any) error }) (domain.SourceObservation, error) {
	var (
		o            domain.SourceObservation
		releaseDate  *time.Time
		precision    string
		candidateID  *string
		evidenceID   *string
		qualityClass string
		official     bool
		eligible     bool
	)
	if err := row.Scan(
		&o.SourceID, &o.ProductID, &o.Channel, &o.RawVersion, &o.NormalizedVersion,
		&releaseDate, &precision, &candidateID, &evidenceID,
		&o.ObservedAt, &o.FirstObservedAt,
		&qualityClass, &official, &eligible,
	); err != nil {
		return domain.SourceObservation{}, err
	}

	rd, err := partialDate(releaseDate, precision)
	if err != nil {
		return domain.SourceObservation{}, err
	}
	o.ReleaseDate = rd
	o.CandidateID = str(candidateID)
	o.EvidenceID = str(evidenceID)
	o.QualityClass = domain.QualityClass(qualityClass)
	o.Official = official
	o.Eligible = eligible
	return o, nil
}

// ObservationsForProduct returns every source's current observation for a product and
// channel.
//
// Eligible is computed in SQL with exactly the predicate sources_dispatchable_idx uses
// -- enabled, health active or degraded, robots allowed or not applicable, terms
// approved or restricted -- character for character, so the Go rule
// (domain.Source.CompliancePermitsCollection) and this query cannot silently drift
// apart. Ineligible sources are returned rather than filtered out here: the domain
// decides what an ineligible observation means (domain.AssessSourceConflict ignores
// it), and filtering in SQL would make that decision untestable against the real
// adapter.
func (r *ConflictRepo) ObservationsForProduct(ctx context.Context, productID, channel string) ([]domain.SourceObservation, error) {
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT so.source_id, so.product_id, so.channel, so.raw_version, so.normalized_version,
                so.release_date, so.release_date_precision, so.candidate_id, so.evidence_id,
                so.observed_at, so.first_observed_at,
                s.quality_class, s.official,
                (s.enabled = true
                 AND s.health_status IN ('active', 'degraded')
                 AND s.robots_policy_status IN ('allowed', 'not_applicable')
                 AND s.terms_review_status IN ('approved', 'restricted')) AS eligible
           FROM source_observations so
           JOIN sources s ON s.id = so.source_id
          WHERE so.product_id = $1 AND so.channel = $2`, productID, channel)
	if err != nil {
		return nil, wrap("conflict.ObservationsForProduct", err)
	}
	defer rows.Close()

	var out []domain.SourceObservation
	for rows.Next() {
		o, err := scanObservation(rows)
		if err != nil {
			return nil, wrap("conflict.ObservationsForProduct.scan", err)
		}
		out = append(out, o)
	}
	return out, wrap("conflict.ObservationsForProduct.rows", rows.Err())
}

// ---------------------------------------------------------------------------
// Conflicts
// ---------------------------------------------------------------------------

const conflictColumns = `
    id, product_id, channel, state, authority_rank, versions, source_ids,
    review_item_id, detected_at, last_seen_at, resolved_at, resolved_by, resolution`

func scanConflict(row interface{ Scan(...any) error }) (domain.SourceConflict, error) {
	var (
		c            domain.SourceConflict
		state        string
		reviewItemID *string
		resolvedAt   *time.Time
		resolvedBy   *string
		resolution   *string
	)
	if err := row.Scan(
		&c.ID, &c.ProductID, &c.Channel, &state, &c.AuthorityRank, &c.Versions, &c.SourceIDs,
		&reviewItemID, &c.DetectedAt, &c.LastSeenAt, &resolvedAt, &resolvedBy, &resolution,
	); err != nil {
		return domain.SourceConflict{}, err
	}
	c.State = domain.ConflictState(state)
	c.ReviewItemID = str(reviewItemID)
	c.ResolvedAt = tim(resolvedAt)
	c.ResolvedBy = str(resolvedBy)
	c.Resolution = str(resolution)
	return c, nil
}

// UpsertOpenConflict opens the conflict for a product and channel, or refreshes the one
// already open with the current participants.
//
// The read-then-write runs as SELECT ... FOR UPDATE followed by an explicit INSERT or
// UPDATE, inside one transaction, rather than as an ON CONFLICT ... RETURNING
// (xmax = 0). The xmax idiom would work, but it is undocumented behaviour, and created
// is what decides whether a human gets a new queue item (D5) -- it deserves a query a
// reader does not have to look up PostgreSQL internals to trust.
//
// c is not passed through domain.SourceConflict.Validate() here: the state this method
// writes is always the literal 'open', regardless of what c.State holds, so the
// invariant that matters -- an open conflict needs at least two versions from at least
// two sources -- is enforced by source_conflicts_participants, the same CHECK
// constraint that would catch a bug in this method itself.
func (r *ConflictRepo) UpsertOpenConflict(ctx context.Context, c domain.SourceConflict) (domain.SourceConflict, bool, error) {
	var (
		out     domain.SourceConflict
		created bool
	)
	err := r.db.Within(ctx, func(ctx context.Context) error {
		q := r.db.q(ctx)

		var existingID string
		err := q.QueryRow(ctx,
			`SELECT id FROM source_conflicts
              WHERE product_id = $1 AND channel = $2 AND state = 'open'
                FOR UPDATE`, c.ProductID, c.Channel).Scan(&existingID)
		switch {
		case isNoRows(err):
			id := c.ID
			if id == "" {
				id = r.db.newID("cflt")
			}
			row := q.QueryRow(ctx,
				`INSERT INTO source_conflicts (
                    id, product_id, channel, state, authority_rank, versions, source_ids,
                    detected_at, last_seen_at, updated_at
                 ) VALUES (
                    $1, $2, $3, 'open', $4, $5, $6,
                    COALESCE($7::timestamptz, now()), now(), now()
                 )
                 RETURNING`+conflictColumns,
				id, c.ProductID, c.Channel, c.AuthorityRank, c.Versions, c.SourceIDs,
				nullTime(c.DetectedAt))
			inserted, serr := scanConflict(row)
			if serr != nil {
				return wrap("conflict.UpsertOpenConflict.insert", serr)
			}
			out = inserted
			created = true
			return nil
		case err != nil:
			return wrap("conflict.UpsertOpenConflict.select", err)
		default:
			row := q.QueryRow(ctx,
				`UPDATE source_conflicts
                    SET authority_rank = $2,
                        versions       = $3,
                        source_ids     = $4,
                        last_seen_at   = now(),
                        updated_at     = now()
                  WHERE id = $1
                  RETURNING`+conflictColumns,
				existingID, c.AuthorityRank, c.Versions, c.SourceIDs)
			refreshed, serr := scanConflict(row)
			if serr != nil {
				return wrap("conflict.UpsertOpenConflict.update", serr)
			}
			out = refreshed
			created = false
			return nil
		}
	})
	if err != nil {
		return domain.SourceConflict{}, false, err
	}
	return out, created, nil
}

// LinkReviewItem attaches the review item a human will act on to an open conflict.
func (r *ConflictRepo) LinkReviewItem(ctx context.Context, conflictID, reviewItemID string) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE source_conflicts SET review_item_id = $2, updated_at = now() WHERE id = $1`,
		conflictID, reviewItemID)
	if err != nil {
		return wrap("conflict.LinkReviewItem", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("conflict.LinkReviewItem")
	}
	return nil
}

// CloseOpenConflict resolves whatever conflict is open for a product and channel.
// closed reports whether there was one; a product with no open conflict is the normal
// case, not an error, which is what lets ValidateCandidate call this unconditionally
// every time sources agree.
func (r *ConflictRepo) CloseOpenConflict(ctx context.Context, productID, channel, resolution, resolvedBy string, at time.Time) (bool, error) {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE source_conflicts
            SET state       = 'resolved',
                resolution  = $3,
                resolved_by = $4,
                resolved_at = COALESCE($5::timestamptz, now()),
                updated_at  = now()
          WHERE product_id = $1 AND channel = $2 AND state = 'open'`,
		productID, channel, nullString(resolution), nullString(resolvedBy), nullTime(at))
	if err != nil {
		return false, wrap("conflict.CloseOpenConflict", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ResolveConflict closes one conflict by id, for a human decision that names it
// directly rather than by product and channel.
func (r *ConflictRepo) ResolveConflict(ctx context.Context, id, resolution, resolvedBy string, at time.Time) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE source_conflicts
            SET state       = 'resolved',
                resolution  = $2,
                resolved_by = $3,
                resolved_at = COALESCE($4::timestamptz, now()),
                updated_at  = now()
          WHERE id = $1 AND state = 'open'`,
		id, nullString(resolution), nullString(resolvedBy), nullTime(at))
	if err != nil {
		return wrap("conflict.ResolveConflict", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("conflict.ResolveConflict")
	}
	return nil
}

// GetConflict returns the conflict with the given id, or domain.ErrNotFound.
func (r *ConflictRepo) GetConflict(ctx context.Context, id string) (domain.SourceConflict, error) {
	row := r.db.q(ctx).QueryRow(ctx, `SELECT`+conflictColumns+` FROM source_conflicts WHERE id = $1`, id)
	c, err := scanConflict(row)
	if err != nil {
		return domain.SourceConflict{}, wrap("conflict.GetConflict", err)
	}
	return c, nil
}

// OpenConflictFor returns the open conflict for a product and channel, or
// domain.ErrNotFound. source_conflicts_open_idx guarantees there is at most one.
func (r *ConflictRepo) OpenConflictFor(ctx context.Context, productID, channel string) (domain.SourceConflict, error) {
	row := r.db.q(ctx).QueryRow(ctx,
		`SELECT`+conflictColumns+`
           FROM source_conflicts
          WHERE product_id = $1 AND channel = $2 AND state = 'open'`, productID, channel)
	c, err := scanConflict(row)
	if err != nil {
		return domain.SourceConflict{}, wrap("conflict.OpenConflictFor", err)
	}
	return c, nil
}

// ListOpenConflicts returns the open conflict queue, most recently detected first,
// capped at limit.
func (r *ConflictRepo) ListOpenConflicts(ctx context.Context, limit int) ([]domain.SourceConflict, error) {
	limit = clampLimit(limit, 50, 500)
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT`+conflictColumns+`
           FROM source_conflicts
          WHERE state = 'open'
          ORDER BY detected_at DESC
          LIMIT $1`, limit)
	if err != nil {
		return nil, wrap("conflict.ListOpenConflicts", err)
	}
	defer rows.Close()

	out := make([]domain.SourceConflict, 0, limit)
	for rows.Next() {
		c, err := scanConflict(rows)
		if err != nil {
			return nil, wrap("conflict.ListOpenConflicts.scan", err)
		}
		out = append(out, c)
	}
	return out, wrap("conflict.ListOpenConflicts.rows", rows.Err())
}
