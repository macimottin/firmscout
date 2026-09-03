package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/macimottin/firmscout/internal/application"
)

// SummaryRepo is the PostgreSQL implementation of application.SummaryRepository. It
// reads product_summaries only: every query in this file is a single-table read, which
// is the entire reason the summary table exists.
type SummaryRepo struct{ db *DB }

var _ application.SummaryRepository = (*SummaryRepo)(nil)

// NewSummaryRepo returns a summary repository bound to db.
func NewSummaryRepo(db *DB) *SummaryRepo { return &SummaryRepo{db: db} }

// MaxSearchQueryLength bounds a search term. A tsquery built from an unbounded string
// is a cheap way to make the database do expensive work on behalf of an anonymous
// caller.
const MaxSearchQueryLength = 200

// MaxSearchResults bounds a search page for the same reason.
const MaxSearchResults = 50

const summaryColumns = `
    product_id, vendor_slug, vendor_name, product_slug, product_name, family_name,
    aliases, category_slugs, latest_release_id, latest_raw_version, latest_release_type,
    latest_channel, latest_release_date, latest_release_date_precision,
    recommended_release_id, release_count, lifecycle_status, has_source_conflict,
    advisory_count, last_verified_at, refreshed_at`

func scanSummary(row interface{ Scan(...any) error }) (application.ProductSummary, error) {
	var (
		s           application.ProductSummary
		familyName  *string
		latestID    *string
		latestRaw   *string
		latestType  *string
		latestChan  *string
		latestDate  *time.Time
		latestPrec  *string
		recommended *string
		verifiedAt  *time.Time
	)
	if err := row.Scan(
		&s.ProductID, &s.VendorSlug, &s.VendorName, &s.ProductSlug, &s.ProductName,
		&familyName, &s.Aliases, &s.CategorySlugs, &latestID, &latestRaw, &latestType,
		&latestChan, &latestDate, &latestPrec, &recommended, &s.ReleaseCount,
		&s.LifecycleStatus, &s.HasSourceConflict, &s.AdvisoryCount, &verifiedAt,
		&s.RefreshedAt,
	); err != nil {
		return application.ProductSummary{}, err
	}

	date, err := partialDate(latestDate, str(latestPrec))
	if err != nil {
		return application.ProductSummary{}, err
	}

	s.FamilyName = str(familyName)
	s.LatestReleaseID = str(latestID)
	s.LatestRawVersion = str(latestRaw)
	s.LatestReleaseType = str(latestType)
	s.LatestChannel = str(latestChan)
	s.LatestReleaseDate = date
	s.RecommendedReleaseID = str(recommended)
	s.LastVerifiedAt = tim(verifiedAt)
	return s, nil
}

// Get returns the precomputed summary for a product slug, or domain.ErrNotFound.
//
// A missing summary means the product exists but has never been refreshed, which is
// reported as not-found rather than as an empty summary: serving a product page that
// claims no releases exist is worse than serving an error, because the caller cannot
// tell the difference between "no releases" and "we have not looked yet".
func (r *SummaryRepo) Get(ctx context.Context, productSlug string) (application.ProductSummary, error) {
	row := r.db.q(ctx).QueryRow(ctx,
		`SELECT`+summaryColumns+` FROM product_summaries WHERE product_slug = $1`, productSlug)
	s, err := scanSummary(row)
	if err != nil {
		return application.ProductSummary{}, wrap("summary.Get", err)
	}
	return s, nil
}

// Search returns summaries matching a free-text query, best match first.
//
// Two mechanisms run together, because they fail in opposite directions:
//
//   - Full-text search over the generated search_vector, which weights the product
//     name above the vendor name and aliases above the family name. It is precise and
//     it is what answers "mikrotik routeros".
//   - A pg_trgm similarity fallback on the product name, which is what answers
//     "routerbaord" -- a typo that produces no lexeme match at all but is obviously
//     the same product to a human.
//
// They are OR-ed rather than tried in sequence so that one round trip serves both, and
// the ranking adds the two scores so an exact lexeme match always outranks a fuzzy one.
// The tsquery uses the 'simple' configuration to match the generated column: stemming
// would map "routing" and "router" together, which is wrong for product names.
//
// The query is truncated to MaxSearchQueryLength and the limit capped at
// MaxSearchResults; an empty query returns no rows rather than the whole catalogue.
func (r *SummaryRepo) Search(ctx context.Context, query string, limit int) ([]application.ProductSummary, error) {
	q := strings.TrimSpace(query)
	if len(q) > MaxSearchQueryLength {
		q = q[:MaxSearchQueryLength]
	}
	if q == "" {
		return nil, nil
	}
	limit = clampLimit(limit, 20, MaxSearchResults)

	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT`+summaryColumns+`
           FROM product_summaries s,
                websearch_to_tsquery('simple', $1) AS q (tsq)
          WHERE s.search_vector @@ q.tsq
             OR s.product_name % $1
          ORDER BY (ts_rank(s.search_vector, q.tsq) + similarity(s.product_name, $1)) DESC,
                   s.product_name
          LIMIT $2`, q, limit)
	if err != nil {
		return nil, wrap("summary.Search", err)
	}
	defer rows.Close()

	out := make([]application.ProductSummary, 0, limit)
	for rows.Next() {
		s, err := scanSummary(rows)
		if err != nil {
			return nil, wrap("summary.Search.scan", err)
		}
		out = append(out, s)
	}
	return out, wrap("summary.Search.rows", rows.Err())
}

// ListByVendor returns a page of a vendor's product summaries ordered by product slug,
// with the cursor for the next page.
func (r *SummaryRepo) ListByVendor(ctx context.Context, vendorSlug string, limit int, cursor string) ([]application.ProductSummary, string, error) {
	limit = clampLimit(limit, 50, 500)
	parts, err := decodeCursor(cursor, 1)
	if err != nil {
		return nil, "", err
	}
	after := ""
	if parts != nil {
		after = parts[0]
	}

	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT`+summaryColumns+`
           FROM product_summaries
          WHERE vendor_slug = $1 AND ($2::text = '' OR product_slug > $2)
          ORDER BY product_slug
          LIMIT $3`, vendorSlug, after, limit)
	if err != nil {
		return nil, "", wrap("summary.ListByVendor", err)
	}
	defer rows.Close()

	out := make([]application.ProductSummary, 0, limit)
	for rows.Next() {
		s, err := scanSummary(rows)
		if err != nil {
			return nil, "", wrap("summary.ListByVendor.scan", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, "", wrap("summary.ListByVendor.rows", err)
	}

	next := ""
	if len(out) == limit {
		next = encodeCursor(out[len(out)-1].ProductSlug)
	}
	return out, next, nil
}
