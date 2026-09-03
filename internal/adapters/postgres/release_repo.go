package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// ReleaseRepo is the PostgreSQL implementation of application.ReleaseRepository.
//
// The releases table is append-only, and this file is where that rule is either kept
// or broken. There is exactly one UPDATE against releases in the whole package --
// TouchVerified, which writes last_verified_at and nothing else. A withdrawal or a
// correction is a new row that references the original, so history is never rewritten
// and a consumer who cached yesterday's answer can still see what FirmScout said
// yesterday and why it changed.
type ReleaseRepo struct{ db *DB }

var _ application.ReleaseRepository = (*ReleaseRepo)(nil)

// NewReleaseRepo returns a release repository bound to db.
func NewReleaseRepo(db *DB) *ReleaseRepo { return &ReleaseRepo{db: db} }

const releaseColumns = `
    id, vendor_id, raw_version, normalized_version, release_type, channel,
    release_date, release_date_precision, publication_date,
    first_observed_at, last_verified_at, published_at,
    release_notes_url, stable, recommended, withdrawn, withdrawn_at, withdrawn_reason,
    corrects_release_id, superseded_by_release_id,
    candidate_id, collector_run_id, evidence_id, source_confidence, approved_by, created_at`

func scanRelease(row interface{ Scan(...any) error }) (domain.Release, error) {
	var (
		r            domain.Release
		rawVersion   string
		normVersion  string
		releaseType  string
		channel      *string
		releaseDate  *time.Time
		precision    string
		pubDate      *time.Time
		notesURL     *string
		stable       *bool
		recommended  *bool
		withdrawnAt  *time.Time
		withdrawnWhy *string
		corrects     *string
		superseded   *string
		candidateID  *string
		runID        *string
		approvedBy   *string
	)
	if err := row.Scan(
		&r.ID, &r.VendorID, &rawVersion, &normVersion, &releaseType, &channel,
		&releaseDate, &precision, &pubDate,
		&r.FirstObservedAt, &r.LastVerifiedAt, &r.PublishedAt,
		&notesURL, &stable, &recommended, &r.Withdrawn, &withdrawnAt, &withdrawnWhy,
		&corrects, &superseded,
		&candidateID, &runID, &r.EvidenceID, &r.SourceConfidence, &approvedBy, &r.CreatedAt,
	); err != nil {
		return domain.Release{}, err
	}

	v, err := version(rawVersion, normVersion)
	if err != nil {
		return domain.Release{}, err
	}
	rd, err := partialDate(releaseDate, precision)
	if err != nil {
		return domain.Release{}, err
	}
	pd, err := publicationDate(pubDate)
	if err != nil {
		return domain.Release{}, err
	}

	r.Version = v
	r.ReleaseType = domain.ReleaseType(releaseType)
	r.Channel = str(channel)
	r.ReleaseDate = rd
	r.PublicationDate = pd
	r.ReleaseNotesURL = str(notesURL)
	r.Stable = stable
	r.Recommended = recommended
	r.WithdrawnAt = tim(withdrawnAt)
	r.WithdrawnReason = str(withdrawnWhy)
	r.CorrectsReleaseID = str(corrects)
	r.SupersededByReleaseID = str(superseded)
	r.CandidateID = str(candidateID)
	r.CollectorRunID = str(runID)
	r.ApprovedBy = str(approvedBy)
	return r, nil
}

// GetByID returns the release with the given id, or domain.ErrNotFound. Withdrawn
// releases are returned: a consumer who has the identifier is entitled to see what
// happened to it.
func (r *ReleaseRepo) GetByID(ctx context.Context, id string) (domain.Release, error) {
	row := r.db.q(ctx).QueryRow(ctx, `SELECT`+releaseColumns+` FROM releases WHERE id = $1`, id)
	rel, err := scanRelease(row)
	if err != nil {
		return domain.Release{}, wrap("release.GetByID", err)
	}
	return rel, nil
}

// Insert writes a release together with the product and family mappings that say what
// it applies to, in one transaction.
//
// A release with no mapping is unreachable -- nothing would ever find it -- so the two
// writes have to be atomic. The is_latest_observed flag on a mapping is written as the
// caller set it; keeping the partial unique index satisfied is the publication use
// case's job, through ClearLatestFlag, because only it knows whether the new release
// is actually the later observation.
func (r *ReleaseRepo) Insert(ctx context.Context, rel domain.Release, mappings []domain.ReleaseProductMapping) error {
	if err := rel.Validate(); err != nil {
		return err
	}
	for _, m := range mappings {
		if m.ReleaseID == "" {
			m.ReleaseID = rel.ID
		}
		if err := m.Validate(); err != nil {
			return err
		}
	}
	releaseDate, precision := dateParams(rel.ReleaseDate)

	return r.db.Within(ctx, func(ctx context.Context) error {
		q := r.db.q(ctx)

		if _, err := q.Exec(ctx,
			`INSERT INTO releases (
                id, vendor_id, raw_version, normalized_version, release_type, channel,
                release_date, release_date_precision, publication_date,
                first_observed_at, last_verified_at, published_at,
                release_notes_url, stable, recommended, withdrawn, withdrawn_at,
                withdrawn_reason, corrects_release_id, superseded_by_release_id,
                candidate_id, collector_run_id, evidence_id, source_confidence,
                approved_by, created_at
             ) VALUES (
                $1, $2, $3, $4, $5, $6, $7, $8, $9,
                COALESCE($10::timestamptz, now()),
                COALESCE($11::timestamptz, now()),
                COALESCE($12::timestamptz, now()),
                $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25,
                COALESCE($26::timestamptz, now())
             )`,
			rel.ID, rel.VendorID, rel.Version.Raw(), rel.Version.Normalized(),
			string(rel.ReleaseType), nullString(rel.Channel),
			releaseDate, precision, publicationDateParam(rel.PublicationDate),
			nullTime(rel.FirstObservedAt), nullTime(rel.LastVerifiedAt), nullTime(rel.PublishedAt),
			nullString(rel.ReleaseNotesURL), nullBool(rel.Stable), nullBool(rel.Recommended),
			rel.Withdrawn, nullTime(rel.WithdrawnAt), nullString(rel.WithdrawnReason),
			nullString(rel.CorrectsReleaseID), nullString(rel.SupersededByReleaseID),
			nullString(rel.CandidateID), nullString(rel.CollectorRunID), rel.EvidenceID,
			rel.SourceConfidence, nullString(rel.ApprovedBy), nullTime(rel.CreatedAt),
		); err != nil {
			return wrap("release.Insert", err)
		}

		for _, m := range mappings {
			id := m.ID
			if id == "" {
				id = r.db.newID("rmap")
			}
			releaseID := m.ReleaseID
			if releaseID == "" {
				releaseID = rel.ID
			}
			if _, err := q.Exec(ctx,
				`INSERT INTO release_product_mappings (
                    id, release_id, product_id, product_family_id, hardware_revision,
                    region, channel, deployment_mode, applicability_note,
                    is_latest_observed, created_at
                 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
                           COALESCE($11::timestamptz, now()))`,
				id, releaseID, nullString(m.ProductID), nullString(m.ProductFamilyID),
				nullString(m.Applicability.HardwareRevision), nullString(m.Applicability.Region),
				nullString(m.Applicability.Channel), nullString(m.Applicability.DeploymentMode),
				nullString(m.Applicability.Note), m.IsLatestObserved, nullTime(m.CreatedAt),
			); err != nil {
				return wrap("release.Insert.mapping", err)
			}
		}
		return nil
	})
}

// FindDuplicate returns the id of an already-published release with the same product,
// normalised version and channel, or "" when there is none.
//
// Absence is reported as an empty string rather than domain.ErrNotFound because "there
// is no duplicate" is the expected, common answer, not a failure. Withdrawn releases
// are excluded: a version that was published, withdrawn and then genuinely re-released
// is a new fact, not a duplicate of a retracted one.
func (r *ReleaseRepo) FindDuplicate(ctx context.Context, productID, normalizedVersion, channel string) (string, error) {
	var id string
	err := r.db.q(ctx).QueryRow(ctx,
		`SELECT r.id
           FROM releases r
          WHERE r.normalized_version = $2
            AND COALESCE(r.channel, '') = $3
            AND r.withdrawn = false
            AND EXISTS (SELECT 1 FROM release_product_mappings m
                         WHERE m.release_id = r.id AND m.product_id = $1)
          ORDER BY r.first_observed_at
          LIMIT 1`, productID, normalizedVersion, channel).Scan(&id)
	if err != nil {
		if errors.Is(translate(err), domain.ErrNotFound) {
			return "", nil
		}
		return "", wrap("release.FindDuplicate", err)
	}
	return id, nil
}

// LatestForProduct returns the release currently flagged latest-observed for a product
// and channel, or domain.ErrNotFound when the product has no releases.
//
// It reads the derived flag rather than deriving the answer, because deriving it would
// mean ordering version strings, which FirmScout never does. The flag is maintained by
// the publication use case using domain.LatestComparison, which orders by release date
// and first-observed time.
//
// The channel predicate is COALESCE of the mapping's channel with the empty string,
// exactly matching the
// expression in release_mappings_latest_idx, so this query and the uniqueness
// guarantee agree about what "the same channel" means.
func (r *ReleaseRepo) LatestForProduct(ctx context.Context, productID, channel string) (domain.Release, error) {
	row := r.db.q(ctx).QueryRow(ctx,
		`SELECT`+releaseColumns+`
           FROM releases r
          WHERE EXISTS (SELECT 1 FROM release_product_mappings m
                         WHERE m.release_id = r.id
                           AND m.product_id = $1
                           AND COALESCE(m.channel, '') = $2
                           AND m.is_latest_observed = true)
          LIMIT 1`, productID, channel)
	rel, err := scanRelease(row)
	if err != nil {
		return domain.Release{}, wrap("release.LatestForProduct", err)
	}
	return rel, nil
}

// ListForProduct returns a product's releases newest-observed first, with the cursor
// for the next page.
//
// The ordering is (first_observed_at, id) descending rather than by version, because
// version strings have no order. The id is part of the sort key so the keyset cursor
// is unambiguous when two releases were observed in the same instant.
func (r *ReleaseRepo) ListForProduct(ctx context.Context, productID string, limit int, cursor string) ([]domain.Release, string, error) {
	limit = clampLimit(limit, 50, 500)
	parts, err := decodeCursor(cursor, 2)
	if err != nil {
		return nil, "", err
	}
	var (
		afterTime *time.Time
		afterID   *string
	)
	if parts != nil {
		t, perr := time.Parse(time.RFC3339Nano, parts[0])
		if perr != nil {
			return nil, "", wrap("release.ListForProduct.cursor", domain.ErrValidation)
		}
		afterTime = &t
		afterID = &parts[1]
	}

	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT`+releaseColumns+`
           FROM releases r
          WHERE EXISTS (SELECT 1 FROM release_product_mappings m
                         WHERE m.release_id = r.id AND m.product_id = $1)
            AND ($2::timestamptz IS NULL
                 OR (r.first_observed_at, r.id) < ($2::timestamptz, $3::text))
          ORDER BY r.first_observed_at DESC, r.id DESC
          LIMIT $4`, productID, afterTime, afterID, limit)
	if err != nil {
		return nil, "", wrap("release.ListForProduct", err)
	}
	defer rows.Close()

	out := make([]domain.Release, 0, limit)
	for rows.Next() {
		rel, err := scanRelease(rows)
		if err != nil {
			return nil, "", wrap("release.ListForProduct.scan", err)
		}
		out = append(out, rel)
	}
	if err := rows.Err(); err != nil {
		return nil, "", wrap("release.ListForProduct.rows", err)
	}

	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.FirstObservedAt.UTC().Format(time.RFC3339Nano), last.ID)
	}
	return out, next, nil
}

// ClearLatestFlag clears the latest-observed marker for a product and channel.
//
// It updates release_product_mappings, never releases: the flag is a derived marker
// about which release is currently newest, not a fact about the release itself.
//
// The predicate coalesces a NULL channel to the empty string, character for character
// the expression
// in the release_mappings_latest_idx partial unique index. Anything else would clear
// the wrong row and the next SetLatestFlag would fail with a uniqueness violation --
// for instance a mapping with a NULL channel is indexed under the empty string and can
// only be found
// by that same expression.
//
// Clearing a flag that is not set is not an error; publication calls this
// unconditionally before setting a new latest, and a product with no releases yet is
// the normal first case.
func (r *ReleaseRepo) ClearLatestFlag(ctx context.Context, productID, channel string) error {
	_, err := r.db.q(ctx).Exec(ctx,
		`UPDATE release_product_mappings
            SET is_latest_observed = false
          WHERE product_id = $1
            AND COALESCE(channel, '') = $2
            AND is_latest_observed = true`, productID, channel)
	return wrap("release.ClearLatestFlag", err)
}

// TouchVerified refreshes last_verified_at, and is the only statement in this package
// that updates the releases table.
//
// It is the correct response to a duplicate candidate: seeing the same version again
// is evidence that the fact is still true, which is worth recording, and it changes
// nothing else about the release.
func (r *ReleaseRepo) TouchVerified(ctx context.Context, releaseID string, at time.Time) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE releases SET last_verified_at = COALESCE($2::timestamptz, now()) WHERE id = $1`,
		releaseID, nullTime(at))
	if err != nil {
		return wrap("release.TouchVerified", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("release.TouchVerified")
	}
	return nil
}

// RefreshProductSummary rebuilds the precomputed row the public site and search read.
//
// The whole point of product_summaries is that a product page is one indexed read
// rather than a six-way join, so this is the one place that join is written. It runs as
// a single INSERT ... ON CONFLICT so the summary is never briefly absent while being
// rebuilt.
//
// Two things are deliberately not written. search_vector is GENERATED ALWAYS and
// PostgreSQL computes it from the columns this statement does set. has_source_conflict
// and advisory_count are not derivable from this schema -- there is no conflict or
// advisory table yet -- so they are omitted from the update list and whatever is
// already there survives a refresh rather than being reset to a fabricated zero.
func (r *ReleaseRepo) RefreshProductSummary(ctx context.Context, productID string) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO product_summaries (
            product_id, vendor_slug, vendor_name, product_slug, product_name, family_name,
            aliases, aliases_text, category_slugs, latest_release_id, latest_raw_version,
            latest_release_type, latest_channel, latest_release_date,
            latest_release_date_precision, recommended_release_id, release_count,
            lifecycle_status, last_verified_at, refreshed_at
         )
         SELECT
            p.id,
            v.slug,
            v.name,
            p.slug,
            p.name,
            f.name,
            COALESCE(al.aliases, '{}'::text[]),
            -- Flattened here rather than in the generated column: array_to_string is
            -- only STABLE, and a generated column's expression must be IMMUTABLE.
            COALESCE(array_to_string(al.aliases, ' '), ''),
            COALESCE(cat.slugs, '{}'::text[]),
            latest.id,
            latest.raw_version,
            latest.release_type,
            latest.channel,
            latest.release_date,
            latest.release_date_precision,
            rec.id,
            COALESCE(stats.release_count, 0),
            p.lifecycle_status,
            stats.last_verified_at,
            now()
         FROM products p
         JOIN vendors v ON v.id = p.vendor_id
         LEFT JOIN product_families f ON f.id = p.product_family_id
         LEFT JOIN LATERAL (
             SELECT array_agg(a.alias ORDER BY a.alias) AS aliases
               FROM product_aliases a WHERE a.product_id = p.id
         ) al ON true
         LEFT JOIN LATERAL (
             SELECT array_agg(c.slug ORDER BY c.slug) AS slugs
               FROM product_categories pc
               JOIN categories c ON c.id = pc.category_id
              WHERE pc.product_id = p.id
         ) cat ON true
         LEFT JOIN LATERAL (
             SELECT count(DISTINCT rr.id) AS release_count,
                    max(rr.last_verified_at) AS last_verified_at
               FROM release_product_mappings mm
               JOIN releases rr ON rr.id = mm.release_id
              WHERE mm.product_id = p.id AND rr.withdrawn = false
         ) stats ON true
         LEFT JOIN LATERAL (
             SELECT rr.id, rr.raw_version, rr.release_type,
                    COALESCE(mm.channel, rr.channel) AS channel,
                    rr.release_date, rr.release_date_precision
               FROM release_product_mappings mm
               JOIN releases rr ON rr.id = mm.release_id
              WHERE mm.product_id = p.id
                AND mm.is_latest_observed = true
                AND rr.withdrawn = false
              ORDER BY rr.release_date DESC NULLS LAST, rr.first_observed_at DESC
              LIMIT 1
         ) latest ON true
         LEFT JOIN LATERAL (
             SELECT rr.id
               FROM release_product_mappings mm
               JOIN releases rr ON rr.id = mm.release_id
              WHERE mm.product_id = p.id
                AND rr.recommended = true
                AND rr.withdrawn = false
              ORDER BY rr.release_date DESC NULLS LAST, rr.first_observed_at DESC
              LIMIT 1
         ) rec ON true
         WHERE p.id = $1
         ON CONFLICT (product_id) DO UPDATE SET
            vendor_slug                   = EXCLUDED.vendor_slug,
            vendor_name                   = EXCLUDED.vendor_name,
            product_slug                  = EXCLUDED.product_slug,
            product_name                  = EXCLUDED.product_name,
            family_name                   = EXCLUDED.family_name,
            aliases                       = EXCLUDED.aliases,
            aliases_text                  = EXCLUDED.aliases_text,
            category_slugs                = EXCLUDED.category_slugs,
            latest_release_id             = EXCLUDED.latest_release_id,
            latest_raw_version            = EXCLUDED.latest_raw_version,
            latest_release_type           = EXCLUDED.latest_release_type,
            latest_channel                = EXCLUDED.latest_channel,
            latest_release_date           = EXCLUDED.latest_release_date,
            latest_release_date_precision = EXCLUDED.latest_release_date_precision,
            recommended_release_id        = EXCLUDED.recommended_release_id,
            release_count                 = EXCLUDED.release_count,
            lifecycle_status              = EXCLUDED.lifecycle_status,
            last_verified_at              = EXCLUDED.last_verified_at,
            refreshed_at                  = EXCLUDED.refreshed_at`, productID)
	if err != nil {
		return wrap("release.RefreshProductSummary", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("release.RefreshProductSummary")
	}
	return nil
}
