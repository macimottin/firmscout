package postgres

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// SnapshotRepo exports and imports the catalogue's observed facts.
type SnapshotRepo struct{ db *DB }

// NewSnapshotRepo builds the repository.
func NewSnapshotRepo(db *DB) *SnapshotRepo { return &SnapshotRepo{db: db} }

var _ application.SnapshotRepository = (*SnapshotRepo)(nil)

// ExportReleaseFacts reads every published release with its evidence and mappings.
//
// The ordering is (vendor slug, release id) and it is load-bearing rather than tidy: a
// snapshot is a file in Git, so an export of an unchanged catalogue has to produce
// byte-identical output or every re-export becomes a diff nobody can review. Ordering by
// anything that moves -- an insertion order, a timestamp with ties, a version string
// (which this catalogue never orders anyway, ADR-0017) -- would churn the file.
//
// Withdrawn releases are exported. A consumer who has the identifier is entitled to know
// what happened to it, and dropping them would silently rewrite history: the withdrawal
// is itself the fact worth publishing.
func (r *SnapshotRepo) ExportReleaseFacts(ctx context.Context) ([]application.ReleaseFact, error) {
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT
             rel.id, rel.raw_version, rel.release_type, rel.channel,
             rel.release_date, rel.release_date_precision,
             rel.publication_date, rel.publication_date_precision,
             rel.release_notes_url, rel.recommended, rel.withdrawn, rel.withdrawn_reason,
             rel.first_observed_at, rel.last_verified_at, rel.published_at,
             v.slug,
             sv.slug, s.slug,
             ev.source_url, ev.source_type, ev.official, ev.retrieved_at,
             ev.content_hash, ev.excerpt, ev.raw_value, ev.normalized_value,
             ev.collector_id, ev.collector_version, ev.discovery_method,
             ev.ai_model_id, ev.ai_prompt_version, ev.confidence_score
           FROM releases rel
           JOIN vendors v  ON v.id = rel.vendor_id
           JOIN evidence ev ON ev.id = rel.evidence_id
           LEFT JOIN sources s  ON s.id = ev.source_id
           LEFT JOIN vendors sv ON sv.id = s.vendor_id
          ORDER BY v.slug, rel.id`)
	if err != nil {
		return nil, wrap("snapshot.ExportReleaseFacts", err)
	}
	defer rows.Close()

	facts := make([]application.ReleaseFact, 0, 128)
	index := map[string]int{}
	for rows.Next() {
		var (
			f                                  application.ReleaseFact
			channel, notesURL, withdrawnReason *string
			releaseDate, pubDate               *time.Time
			releasePrecision, pubPrecision     string
			recommended                        *bool
			sourceVendor, sourceSlug           *string
			sourceType, contentHash            *string
			rawValue, normalizedValue          *string
			collectorID, collectorVersion      *string
			aiModelID, aiPromptVersion         *string
			confidence                         *float64
		)
		if err := rows.Scan(
			&f.Release.ID, &f.Release.Version, &f.Release.ReleaseType, &channel,
			&releaseDate, &releasePrecision,
			&pubDate, &pubPrecision,
			&notesURL, &recommended, &f.Release.Withdrawn, &withdrawnReason,
			&f.Release.FirstObservedAt, &f.Release.LastVerifiedAt, &f.Release.PublishedAt,
			&f.Vendor,
			&sourceVendor, &sourceSlug,
			&f.Evidence.SourceURL, &sourceType, &f.Evidence.Official, &f.Evidence.RetrievedAt,
			&contentHash, &f.Evidence.Excerpt, &rawValue, &normalizedValue,
			&collectorID, &collectorVersion, &f.Evidence.DiscoveryMethod,
			&aiModelID, &aiPromptVersion, &confidence,
		); err != nil {
			return nil, wrap("snapshot.ExportReleaseFacts.scan", err)
		}

		f.Release.Channel = deref(channel)
		f.Release.ReleaseNotesURL = deref(notesURL)
		f.Release.WithdrawnReason = deref(withdrawnReason)
		f.Release.Recommended = recommended
		f.Release.ReleaseDate = snapshotDate(releaseDate, releasePrecision)
		if d := snapshotDate(pubDate, pubPrecision); d.Precision != string(domain.PrecisionUnknown) {
			f.Release.PublicationDate = &d
		}

		f.Evidence.SourceVendor = deref(sourceVendor)
		f.Evidence.SourceSlug = deref(sourceSlug)
		f.Evidence.SourceType = deref(sourceType)
		f.Evidence.ContentHash = deref(contentHash)
		f.Evidence.RawValue = deref(rawValue)
		f.Evidence.NormalizedValue = deref(normalizedValue)
		f.Evidence.CollectorID = deref(collectorID)
		f.Evidence.CollectorVersion = deref(collectorVersion)
		f.Evidence.AIModelID = deref(aiModelID)
		f.Evidence.AIPromptVersion = deref(aiPromptVersion)
		f.Evidence.Confidence = confidence

		// UTC, always. pgx returns a timestamptz in the session's zone, so an export
		// run in Sao Paulo and one run in Berlin would write the same instant with
		// different offsets and produce a diff on every re-export -- which defeats the
		// determinism the ORDER BY above exists to provide. The instant is identical
		// either way; only its rendering is being pinned.
		f.Release.FirstObservedAt = f.Release.FirstObservedAt.UTC()
		f.Release.LastVerifiedAt = f.Release.LastVerifiedAt.UTC()
		f.Release.PublishedAt = f.Release.PublishedAt.UTC()
		f.Evidence.RetrievedAt = f.Evidence.RetrievedAt.UTC()

		index[f.Release.ID] = len(facts)
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("snapshot.ExportReleaseFacts.rows", err)
	}

	if err := r.attachMappings(ctx, facts, index); err != nil {
		return nil, err
	}
	for i := range facts {
		if err := facts[i].Validate(); err != nil {
			// Exporting a fact the importer would reject would produce a file that
			// cannot be loaded into the catalogue it came from.
			return nil, fmt.Errorf("snapshot.ExportReleaseFacts: %w", err)
		}
	}
	return facts, nil
}

// attachMappings fills in each fact's products in one query rather than one per release.
func (r *SnapshotRepo) attachMappings(ctx context.Context, facts []application.ReleaseFact, index map[string]int) error {
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT m.release_id, p.slug, m.channel, m.hardware_revision, m.region,
                m.deployment_mode, m.is_latest_observed
           FROM release_product_mappings m
           JOIN products p ON p.id = m.product_id
          ORDER BY m.release_id, p.slug`)
	if err != nil {
		return wrap("snapshot.attachMappings", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			releaseID                         string
			m                                 application.SnapshotMapping
			channel, hardware, region, deploy *string
		)
		if err := rows.Scan(&releaseID, &m.Slug, &channel, &hardware, &region, &deploy, &m.IsLatestObserved); err != nil {
			return wrap("snapshot.attachMappings.scan", err)
		}
		i, ok := index[releaseID]
		if !ok {
			// A mapping targeting a family rather than a product, or a release the
			// first query did not return. Neither belongs to a fact being exported.
			continue
		}
		m.Channel, m.HardwareRevision, m.Region, m.DeploymentMode =
			deref(channel), deref(hardware), deref(region), deref(deploy)
		facts[i].Products = append(facts[i].Products, m)
	}
	return wrap("snapshot.attachMappings.rows", rows.Err())
}

// ImportReleaseFacts writes facts the catalogue does not already have.
//
// Every slug is resolved against the local registry first and the whole import runs in
// one transaction, so a snapshot naming a product this installation has never heard of
// fails before writing anything rather than leaving a half-loaded catalogue. That is the
// same reason `registry sync` has to run first: a snapshot carries facts, not the
// catalogue's vocabulary.
//
// A release whose id is already present is counted as skipped rather than overwritten.
// Re-importing is therefore idempotent, and a snapshot can never silently rewrite a
// release that a local collection has since verified or withdrawn.
func (r *SnapshotRepo) ImportReleaseFacts(ctx context.Context, facts []application.ReleaseFact) (int, int, error) {
	for i := range facts {
		if err := facts[i].Validate(); err != nil {
			return 0, 0, fmt.Errorf("snapshot record %d: %w", i+1, err)
		}
	}

	var imported, skipped int
	touched := map[string]bool{}
	err := r.db.Within(ctx, func(ctx context.Context) error {
		vendors, err := r.slugIndex(ctx, "SELECT slug, id FROM vendors")
		if err != nil {
			return err
		}
		products, err := r.slugIndex(ctx, "SELECT slug, id FROM products")
		if err != nil {
			return err
		}
		sources, err := r.sourceIndex(ctx)
		if err != nil {
			return err
		}

		for i := range facts {
			f := facts[i]
			vendorID, ok := vendors[f.Vendor]
			if !ok {
				return fmt.Errorf("snapshot names vendor %q, which is not in this registry; run 'firmscout registry sync' first: %w",
					f.Vendor, domain.ErrNotFound)
			}
			for _, p := range f.Products {
				if _, ok := products[p.Slug]; !ok {
					return fmt.Errorf("snapshot names product %q, which is not in this registry; run 'firmscout registry sync' first: %w",
						p.Slug, domain.ErrNotFound)
				}
			}

			var exists bool
			if err := r.db.q(ctx).QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM releases WHERE id = $1)`, f.Release.ID).Scan(&exists); err != nil {
				return wrap("snapshot.ImportReleaseFacts.exists", err)
			}
			if exists {
				skipped++
				continue
			}

			// The source may legitimately be absent: a fact can outlive the registry
			// entry it came from, and the evidence's URL and hash still stand. A
			// missing source is a null reference, never a refusal to import.
			var sourceID *string
			if key := f.Evidence.SourceVendor + "/" + f.Evidence.SourceSlug; f.Evidence.SourceSlug != "" {
				if id, ok := sources[key]; ok {
					sourceID = &id
				}
			}
			evidenceID, err := r.insertSnapshotEvidence(ctx, f, sourceID)
			if err != nil {
				return err
			}
			if err := r.insertSnapshotRelease(ctx, f, vendorID, evidenceID, products); err != nil {
				return err
			}
			imported++
			for _, p := range f.Products {
				touched[p.Slug] = true
			}
		}

		// The product summary is a derived read model, and an import that left it stale
		// would load the catalogue into the tables and leave every product page empty --
		// present in the database, invisible in the API, which reads as an import that
		// did nothing. Only the products this import touched are refreshed; rebuilding
		// the whole catalogue would make a one-release import as expensive as a full one.
		releases := NewReleaseRepo(r.db)
		for slug := range touched {
			id, ok := products[slug]
			if !ok {
				continue
			}
			if err := releases.RefreshProductSummary(ctx, id); err != nil {
				return fmt.Errorf("refresh summary for %s: %w", slug, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return imported, skipped, nil
}

func (r *SnapshotRepo) slugIndex(ctx context.Context, query string) (map[string]string, error) {
	rows, err := r.db.q(ctx).Query(ctx, query)
	if err != nil {
		return nil, wrap("snapshot.slugIndex", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var slug, id string
		if err := rows.Scan(&slug, &id); err != nil {
			return nil, wrap("snapshot.slugIndex.scan", err)
		}
		out[slug] = id
	}
	return out, wrap("snapshot.slugIndex.rows", rows.Err())
}

// sourceIndex keys sources by "vendor-slug/source-slug", which is what makes them
// addressable at all: a source slug is unique per vendor, never globally.
func (r *SnapshotRepo) sourceIndex(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT v.slug, s.slug, s.id FROM sources s JOIN vendors v ON v.id = s.vendor_id`)
	if err != nil {
		return nil, wrap("snapshot.sourceIndex", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var vendorSlug, sourceSlug, id string
		if err := rows.Scan(&vendorSlug, &sourceSlug, &id); err != nil {
			return nil, wrap("snapshot.sourceIndex.scan", err)
		}
		out[vendorSlug+"/"+sourceSlug] = id
	}
	return out, wrap("snapshot.sourceIndex.rows", rows.Err())
}

func (r *SnapshotRepo) insertSnapshotEvidence(ctx context.Context, f application.ReleaseFact, sourceID *string) (string, error) {
	id := r.db.newID("evd")
	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO evidence (
            id, source_id, source_url, source_type, official, retrieved_at,
            content_hash, excerpt, raw_value, normalized_value,
            collector_id, collector_version, discovery_method,
            ai_model_id, ai_prompt_version, confidence_score
         ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		id, sourceID, f.Evidence.SourceURL, nullString(f.Evidence.SourceType),
		f.Evidence.Official, f.Evidence.RetrievedAt,
		nullString(f.Evidence.ContentHash), f.Evidence.Excerpt,
		nullString(f.Evidence.RawValue), nullString(f.Evidence.NormalizedValue),
		nullString(f.Evidence.CollectorID), nullString(f.Evidence.CollectorVersion),
		f.Evidence.DiscoveryMethod,
		nullString(f.Evidence.AIModelID), nullString(f.Evidence.AIPromptVersion),
		f.Evidence.Confidence)
	if err != nil {
		return "", wrap("snapshot.insertEvidence", err)
	}
	return id, nil
}

func (r *SnapshotRepo) insertSnapshotRelease(
	ctx context.Context, f application.ReleaseFact, vendorID, evidenceID string, products map[string]string,
) error {
	// Through the domain type rather than storing the raw string twice: the normalised
	// form is what search and duplicate detection compare, and deriving it here with a
	// second rule would let an imported release fail to match a collected one.
	version, err := domain.NewVersionString(f.Release.Version)
	if err != nil {
		return fmt.Errorf("release %s: %w", f.Release.ID, err)
	}
	releaseDate, releasePrecision, err := parseSnapshotDate(f.Release.ReleaseDate)
	if err != nil {
		return fmt.Errorf("release %s: %w", f.Release.ID, err)
	}
	var pubDate *time.Time
	pubPrecision := string(domain.PrecisionUnknown)
	if f.Release.PublicationDate != nil {
		pubDate, pubPrecision, err = parseSnapshotDate(*f.Release.PublicationDate)
		if err != nil {
			return fmt.Errorf("release %s publication date: %w", f.Release.ID, err)
		}
	}

	if _, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO releases (
            id, vendor_id, raw_version, normalized_version, release_type, channel,
            release_date, release_date_precision, publication_date, publication_date_precision,
            release_notes_url, recommended, withdrawn, withdrawn_reason,
            evidence_id, first_observed_at, last_verified_at, published_at
         ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		f.Release.ID, vendorID, version.Raw(), version.Normalized(), f.Release.ReleaseType,
		nullString(f.Release.Channel),
		releaseDate, releasePrecision, pubDate, pubPrecision,
		nullString(f.Release.ReleaseNotesURL), f.Release.Recommended,
		f.Release.Withdrawn, nullString(f.Release.WithdrawnReason),
		evidenceID, f.Release.FirstObservedAt, f.Release.LastVerifiedAt, f.Release.PublishedAt,
	); err != nil {
		return wrap("snapshot.insertRelease", err)
	}

	for _, p := range f.Products {
		if _, err := r.db.q(ctx).Exec(ctx,
			`INSERT INTO release_product_mappings (
                id, release_id, product_id, hardware_revision, region, channel,
                deployment_mode, is_latest_observed
             ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			r.db.newID("rpm"), f.Release.ID, products[p.Slug],
			nullString(p.HardwareRevision), nullString(p.Region), nullString(p.Channel),
			nullString(p.DeploymentMode), p.IsLatestObserved,
		); err != nil {
			return wrap("snapshot.insertMapping", err)
		}
	}
	return nil
}

// snapshotDate renders a stored date at exactly its recorded precision, going through
// domain.PartialDate rather than formatting the timestamp directly. That is what stops
// a month-precision date being written to the file as a full day: the domain type has
// no Day() accessor and its String() already renders at the known precision.
func snapshotDate(t *time.Time, precision string) application.SnapshotDate {
	p := domain.DatePrecision(precision)
	if t == nil || p == domain.PrecisionUnknown || !domain.ValidDatePrecision(p) {
		return application.SnapshotDate{Precision: string(domain.PrecisionUnknown)}
	}
	// NewPartialDate re-applies the anchoring rule the schema's CHECK constraints
	// enforce, so a stored row that somehow violated it renders as unknown rather than
	// as a day nobody published.
	d, err := domain.NewPartialDate(t.UTC(), p)
	if err != nil {
		return application.SnapshotDate{Precision: string(domain.PrecisionUnknown)}
	}
	return application.SnapshotDate{Value: d.String(), Precision: string(p)}
}

// parseSnapshotDate turns a snapshot date back into the anchored value the schema's
// CHECK constraints require: day 1 for a month, 1 January for a year.
func parseSnapshotDate(d application.SnapshotDate) (*time.Time, string, error) {
	p := domain.DatePrecision(d.Precision)
	if p == domain.PrecisionUnknown || strings.TrimSpace(d.Value) == "" {
		return nil, string(domain.PrecisionUnknown), nil
	}
	layout := map[domain.DatePrecision]string{
		domain.PrecisionExactDay:  "2006-01-02",
		domain.PrecisionMonthOnly: "2006-01",
		domain.PrecisionYearOnly:  "2006",
	}[p]
	if layout == "" {
		return nil, "", fmt.Errorf("precision %q: %w", d.Precision, domain.ErrValidation)
	}
	t, err := time.Parse(layout, d.Value)
	if err != nil {
		return nil, "", fmt.Errorf("date %q at precision %q: %w", d.Value, d.Precision, domain.ErrValidation)
	}
	t = t.UTC()
	return &t, string(p), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// SnapshotSlugs returns the sorted vendor and product slugs a set of facts references,
// for the manifest's index.
func SnapshotSlugs(facts []application.ReleaseFact) (vendors, products []string) {
	vs, ps := map[string]bool{}, map[string]bool{}
	for _, f := range facts {
		vs[f.Vendor] = true
		for _, p := range f.Products {
			ps[p.Slug] = true
		}
	}
	for v := range vs {
		vendors = append(vendors, v)
	}
	for p := range ps {
		products = append(products, p)
	}
	sort.Strings(vendors)
	sort.Strings(products)
	return vendors, products
}
