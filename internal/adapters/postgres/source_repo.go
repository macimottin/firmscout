package postgres

import (
	"context"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// SourceRepo is the PostgreSQL implementation of application.SourceRepository.
//
// It is the adapter with the most safety-critical query in the package: ListDispatchable
// decides what FirmScout is allowed to fetch, and getting its predicate wrong means
// crawling a site whose robots.txt or terms forbid it.
type SourceRepo struct{ db *DB }

var _ application.SourceRepository = (*SourceRepo)(nil)

// NewSourceRepo returns a source repository bound to db.
func NewSourceRepo(db *DB) *SourceRepo { return &SourceRepo{db: db} }

// parserConfig is the JSONB shape of sources.parser_configuration. It is the stored
// form of domain.NormalizeConfig: which part of a document is hashed, and what is
// stripped before hashing. Getting this wrong is the difference between a source that
// reports a change once a month and one that reports a change on every check.
type parserConfig struct {
	SectionSelector string   `json:"section_selector,omitempty"`
	Strip           []string `json:"strip,omitempty"`
}

const sourceColumns = `
    id, vendor_id, product_id, product_family_id, slug, source_type, source_url,
    official, quality_class, robots_policy_status, robots_checked_at,
    terms_review_status, terms_review_note, authentication_type, enabled,
    collector_id, parser_configuration, expected_content_type,
    check_frequency_seconds, min_frequency_seconds,
    etag, last_modified_value, normalized_content_hash,
    last_checked_at, last_changed_at, last_success_at, next_check_at,
    consecutive_failures, consecutive_unchanged, retry_after_until,
    health_status, relocated_to_source_id, confidence_score, created_by, approved_by`

func scanSource(row interface{ Scan(...any) error }) (domain.Source, error) {
	var (
		s               domain.Source
		productID       *string
		familyID        *string
		sourceType      string
		robotsCheckedAt *time.Time
		termsNote       *string
		collectorID     *string
		parser          parserConfig
		contentType     *string
		minFrequency    *int
		etag            *string
		lastModified    *string
		contentHash     *string
		lastCheckedAt   *time.Time
		lastChangedAt   *time.Time
		lastSuccessAt   *time.Time
		retryAfterUntil *time.Time
		relocatedTo     *string
		createdBy       *string
		approvedBy      *string
		quality         string
		robots          string
		terms           string
		auth            string
		health          string
	)
	if err := row.Scan(
		&s.ID, &s.VendorID, &productID, &familyID, &s.Slug, &sourceType, &s.URL,
		&s.Official, &quality, &robots, &robotsCheckedAt,
		&terms, &termsNote, &auth, &s.Enabled,
		&collectorID, &parser, &contentType,
		&s.CheckFrequencySeconds, &minFrequency,
		&etag, &lastModified, &contentHash,
		&lastCheckedAt, &lastChangedAt, &lastSuccessAt, &s.NextCheckAt,
		&s.ConsecutiveFailures, &s.ConsecutiveUnchanged, &retryAfterUntil,
		&health, &relocatedTo, &s.Confidence, &createdBy, &approvedBy,
	); err != nil {
		return domain.Source{}, err
	}
	s.ProductID = str(productID)
	s.ProductFamilyID = str(familyID)
	s.SourceType = domain.SourceType(sourceType)
	s.QualityClass = domain.QualityClass(quality)
	s.RobotsPolicyStatus = domain.RobotsPolicyStatus(robots)
	s.RobotsCheckedAt = tim(robotsCheckedAt)
	s.TermsReviewStatus = domain.TermsReviewStatus(terms)
	s.TermsReviewNote = str(termsNote)
	s.AuthenticationType = domain.AuthenticationType(auth)
	s.CollectorID = str(collectorID)
	s.Normalize = domain.NormalizeConfig{SectionSelector: parser.SectionSelector, Strip: parser.Strip}
	s.ExpectedContentType = str(contentType)
	s.MinFrequencySeconds = integer(minFrequency)
	s.ETag = str(etag)
	s.LastModifiedValue = str(lastModified)
	s.NormalizedContentHash = str(contentHash)
	s.LastCheckedAt = tim(lastCheckedAt)
	s.LastChangedAt = tim(lastChangedAt)
	s.LastSuccessAt = tim(lastSuccessAt)
	s.RetryAfterUntil = tim(retryAfterUntil)
	s.Health = domain.SourceHealth(health)
	s.RelocatedToSourceID = str(relocatedTo)
	s.CreatedBy = str(createdBy)
	s.ApprovedBy = str(approvedBy)
	return s, nil
}

// GetByID returns the source with the given id, or domain.ErrNotFound.
func (r *SourceRepo) GetByID(ctx context.Context, id string) (domain.Source, error) {
	row := r.db.q(ctx).QueryRow(ctx, `SELECT`+sourceColumns+` FROM sources WHERE id = $1`, id)
	s, err := scanSource(row)
	if err != nil {
		return domain.Source{}, wrap("source.GetByID", err)
	}
	return s, nil
}

// GetBySlug returns a vendor's source by slug, or domain.ErrNotFound. Source slugs are
// unique per vendor, so both parts of the key are required.
func (r *SourceRepo) GetBySlug(ctx context.Context, vendorID, slug string) (domain.Source, error) {
	row := r.db.q(ctx).QueryRow(ctx,
		`SELECT`+sourceColumns+` FROM sources WHERE vendor_id = $1 AND slug = $2`, vendorID, slug)
	s, err := scanSource(row)
	if err != nil {
		return domain.Source{}, wrap("source.GetBySlug", err)
	}
	return s, nil
}

// Upsert inserts or updates a source definition.
//
// It writes the registration and compliance columns but leaves the change-detection
// state alone on update: re-synchronising a source's definition from the registry must
// not reset its ETag, its content hash or its next check time, because that would turn
// every registry sync into a full re-crawl of the fleet. UpdateCheckState is the only
// path that writes those columns.
func (r *SourceRepo) Upsert(ctx context.Context, s domain.Source) error {
	if err := s.Validate(); err != nil {
		return err
	}
	parser := parserConfig{SectionSelector: s.Normalize.SectionSelector, Strip: s.Normalize.Strip}

	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO sources (
            id, vendor_id, product_id, product_family_id, slug, source_type, source_url,
            official, quality_class, robots_policy_status, robots_checked_at,
            terms_review_status, terms_review_note, authentication_type, enabled,
            collector_id, parser_configuration, expected_content_type,
            check_frequency_seconds, min_frequency_seconds,
            next_check_at, health_status, relocated_to_source_id, confidence_score,
            created_by, approved_by
         ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
            $16, $17, $18, $19, $20,
            COALESCE($21::timestamptz, now()), $22, $23, $24, $25, $26
         )
         ON CONFLICT (id) DO UPDATE SET
            vendor_id               = EXCLUDED.vendor_id,
            product_id              = EXCLUDED.product_id,
            product_family_id       = EXCLUDED.product_family_id,
            slug                    = EXCLUDED.slug,
            source_type             = EXCLUDED.source_type,
            source_url              = EXCLUDED.source_url,
            official                = EXCLUDED.official,
            quality_class           = EXCLUDED.quality_class,
            robots_policy_status    = EXCLUDED.robots_policy_status,
            robots_checked_at       = EXCLUDED.robots_checked_at,
            terms_review_status     = EXCLUDED.terms_review_status,
            terms_review_note       = EXCLUDED.terms_review_note,
            authentication_type     = EXCLUDED.authentication_type,
            enabled                 = EXCLUDED.enabled,
            collector_id            = EXCLUDED.collector_id,
            parser_configuration    = EXCLUDED.parser_configuration,
            expected_content_type   = EXCLUDED.expected_content_type,
            check_frequency_seconds = EXCLUDED.check_frequency_seconds,
            min_frequency_seconds   = EXCLUDED.min_frequency_seconds,
            health_status           = EXCLUDED.health_status,
            relocated_to_source_id  = EXCLUDED.relocated_to_source_id,
            confidence_score        = EXCLUDED.confidence_score,
            created_by              = EXCLUDED.created_by,
            approved_by             = EXCLUDED.approved_by,
            updated_at              = now()`,
		s.ID, s.VendorID, nullString(s.ProductID), nullString(s.ProductFamilyID), s.Slug,
		string(s.SourceType), s.URL, s.Official, string(s.QualityClass),
		string(s.RobotsPolicyStatus), nullTime(s.RobotsCheckedAt),
		string(s.TermsReviewStatus), nullString(s.TermsReviewNote),
		string(s.AuthenticationType), s.Enabled,
		nullString(s.CollectorID), parser, nullString(s.ExpectedContentType),
		s.CheckFrequencySeconds, nullInt(s.MinFrequencySeconds),
		nullTime(s.NextCheckAt), string(s.Health), nullString(s.RelocatedToSourceID),
		s.Confidence, nullString(s.CreatedBy), nullString(s.ApprovedBy),
	)
	return wrap("source.Upsert", err)
}

// ListDispatchable returns sources that are due for a check at time now, oldest due
// first, capped at limit.
//
// The WHERE clause is written to match sources_dispatchable_idx exactly -- enabled,
// health in (active, degraded), robots in (allowed, not_applicable), terms in
// (approved, restricted) -- so the planner uses the partial index and, more
// importantly, so the compliance rule cannot be edited here without also failing to
// match the index that documents it. The same predicate is expressed in Go as
// domain.Source.Dispatchable; the two are kept identical on purpose, and the
// integration tests assert that a robots-disallowed, terms-pending, disabled or
// not-yet-due source is never returned.
func (r *SourceRepo) ListDispatchable(ctx context.Context, now time.Time, limit int) ([]domain.Source, error) {
	limit = clampLimit(limit, 100, 1000)
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT`+sourceColumns+`
           FROM sources
          WHERE enabled = true
            AND health_status IN ('active', 'degraded')
            AND robots_policy_status IN ('allowed', 'not_applicable')
            AND terms_review_status IN ('approved', 'restricted')
            AND next_check_at <= $1
            AND (retry_after_until IS NULL OR retry_after_until <= $1)
          ORDER BY next_check_at
          LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, wrap("source.ListDispatchable", err)
	}
	defer rows.Close()

	out := make([]domain.Source, 0, limit)
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, wrap("source.ListDispatchable.scan", err)
		}
		out = append(out, s)
	}
	return out, wrap("source.ListDispatchable.rows", rows.Err())
}

// UpdateCheckState writes back the change-detection and health columns after a check.
//
// It touches only the columns a check produces. The source's identity, compliance
// status and collection configuration are untouched, so a watcher cannot accidentally
// re-approve a source's terms by writing back a stale struct.
func (r *SourceRepo) UpdateCheckState(ctx context.Context, s domain.Source) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE sources SET
            etag                    = $2,
            last_modified_value     = $3,
            normalized_content_hash = $4,
            last_checked_at         = $5,
            last_changed_at         = $6,
            last_success_at         = $7,
            next_check_at           = COALESCE($8::timestamptz, next_check_at),
            consecutive_failures    = $9,
            consecutive_unchanged   = $10,
            retry_after_until       = $11,
            health_status           = $12,
            relocated_to_source_id  = $13,
            updated_at              = now()
          WHERE id = $1`,
		s.ID, nullString(s.ETag), nullString(s.LastModifiedValue),
		nullString(s.NormalizedContentHash), nullTime(s.LastCheckedAt),
		nullTime(s.LastChangedAt), nullTime(s.LastSuccessAt), nullTime(s.NextCheckAt),
		s.ConsecutiveFailures, s.ConsecutiveUnchanged, nullTime(s.RetryAfterUntil),
		string(s.Health), nullString(s.RelocatedToSourceID))
	if err != nil {
		return wrap("source.UpdateCheckState", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("source.UpdateCheckState")
	}
	return nil
}

// ProductsForSource returns every product the source covers: those linked explicitly
// through source_products, the source's own single product, and every product in the
// source's family.
//
// The three paths exist because one catalogue page can serve forty devices. Resolving
// them here rather than at every call site is what stops a collector from having to
// know which of the three shapes its source uses.
func (r *SourceRepo) ProductsForSource(ctx context.Context, sourceID string) ([]domain.Product, error) {
	rows, err := r.db.q(ctx).Query(ctx,
		productSelect+`
         WHERE p.id IN (
             SELECT sp.product_id FROM source_products sp WHERE sp.source_id = $1
             UNION
             SELECT s.product_id FROM sources s WHERE s.id = $1 AND s.product_id IS NOT NULL
             UNION
             SELECT p2.id FROM products p2
               JOIN sources s ON s.id = $1
              WHERE s.product_family_id IS NOT NULL
                AND p2.product_family_id = s.product_family_id
         )
         ORDER BY p.slug`, sourceID)
	if err != nil {
		return nil, wrap("source.ProductsForSource", err)
	}
	defer rows.Close()

	var out []domain.Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, wrap("source.ProductsForSource.scan", err)
		}
		out = append(out, p)
	}
	return out, wrap("source.ProductsForSource.rows", rows.Err())
}

// RecordCheck appends one watcher run to the check history.
//
// The history is append-only and is what makes the fleet's cost profile measurable:
// change_signal records which mechanism decided the outcome, so "how many of our
// checks needed a full body download" is a query rather than a guess.
func (r *SourceRepo) RecordCheck(ctx context.Context, c application.SourceCheck) error {
	id := c.ID
	if id == "" {
		id = r.db.newID("chk")
	}
	var durationMS *int
	if d := c.Duration(); d > 0 {
		ms := int(d.Milliseconds())
		durationMS = &ms
	}
	var signal *string
	if c.ChangeSignal != "" {
		s := string(c.ChangeSignal)
		signal = &s
	}
	var httpStatus *int
	if c.HTTPStatus != 0 {
		httpStatus = &c.HTTPStatus
	}
	var bytesFetched *int64
	if c.BytesFetched != 0 {
		bytesFetched = &c.BytesFetched
	}

	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO source_checks (
            id, source_id, started_at, finished_at, duration_ms, outcome, change_signal,
            http_status, response_etag, response_last_modified, redirect_location,
            normalized_content_hash, artifact_id, bytes_fetched, error_message,
            trace_id, request_id
         ) VALUES (
            $1, $2, COALESCE($3::timestamptz, now()), $4, $5, $6, $7, $8, $9, $10, $11,
            $12, $13, $14, $15, $16, $17
         )`,
		id, c.SourceID, nullTime(c.StartedAt), nullTime(c.FinishedAt), durationMS,
		string(c.Outcome), signal, httpStatus, nullString(c.ResponseETag),
		nullString(c.ResponseLastModified), nullString(c.RedirectLocation),
		nullString(c.NormalizedContentHash), nullString(c.ArtifactID), bytesFetched,
		nullString(c.ErrorMessage), nullString(c.TraceID), nullString(c.RequestID))
	return wrap("source.RecordCheck", err)
}
