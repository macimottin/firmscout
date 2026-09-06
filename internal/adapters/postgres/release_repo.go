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

// releaseColumns qualifies every releases column with the "r" alias because
// LatestForProduct and ListForProduct now join evidence and sources, both of which
// have their own "id" (and evidence has its own "official" and "source_type") --
// unqualified names that used to be unambiguous against releases alone would now
// either fail to parse or, worse, silently resolve to the wrong table's column.
//
// The evidence and sources joins are LEFT JOINs for two different reasons. Joining
// evidence through releases.evidence_id is provably safe -- that column is NOT NULL
// with ON DELETE RESTRICT, so a matching row always exists -- and LEFT is used anyway
// as the defensive style this codebase already applies to provably-safe joins
// elsewhere. Joining sources through evidence.source_id is not provably safe: that
// column is nullable (ON DELETE SET NULL), so a source can be deleted out from under
// evidence that still cites it, and an INNER JOIN there would silently drop the
// release from every result instead of just leaving its source kind at the evidence's
// own recorded value. COALESCE(ev.source_type, src.source_type) is that fallback:
// evidence.source_type is nullable in the schema even though every real collector
// fills it in, sources.source_type is not, and the two are only ever consulted in that
// order, never merged any other way.
const releaseColumns = `
    r.id, r.vendor_id, r.raw_version, r.normalized_version, r.release_type, r.channel,
    r.release_date, r.release_date_precision, r.publication_date,
    r.first_observed_at, r.last_verified_at, r.published_at,
    r.release_notes_url, r.stable, r.recommended, r.withdrawn, r.withdrawn_at, r.withdrawn_reason,
    r.corrects_release_id, r.superseded_by_release_id,
    r.candidate_id, r.collector_run_id, r.evidence_id, r.source_confidence, r.approved_by, r.created_at,
    ev.source_url, COALESCE(ev.source_type, src.source_type), ev.official,
    ev.excerpt, ev.retrieved_at`

// releaseJoins is the FROM-clause tail every releaseColumns select needs. See
// releaseColumns for why both joins are LEFT and in this order.
const releaseJoins = `
    LEFT JOIN evidence ev ON ev.id = r.evidence_id
    LEFT JOIN sources src ON src.id = ev.source_id`

func scanRelease(row interface{ Scan(...any) error }) (domain.Release, error) {
	var (
		r              domain.Release
		rawVersion     string
		normVersion    string
		releaseType    string
		channel        *string
		releaseDate    *time.Time
		precision      string
		pubDate        *time.Time
		notesURL       *string
		stable         *bool
		recommended    *bool
		withdrawnAt    *time.Time
		withdrawnWhy   *string
		corrects       *string
		superseded     *string
		candidateID    *string
		runID          *string
		approvedBy     *string
		sourceURL      *string
		sourceType     *string
		sourceOfficial *bool
		excerpt        *string
		retrievedAt    *time.Time
	)
	if err := row.Scan(
		&r.ID, &r.VendorID, &rawVersion, &normVersion, &releaseType, &channel,
		&releaseDate, &precision, &pubDate,
		&r.FirstObservedAt, &r.LastVerifiedAt, &r.PublishedAt,
		&notesURL, &stable, &recommended, &r.Withdrawn, &withdrawnAt, &withdrawnWhy,
		&corrects, &superseded,
		&candidateID, &runID, &r.EvidenceID, &r.SourceConfidence, &approvedBy, &r.CreatedAt,
		&sourceURL, &sourceType, &sourceOfficial, &excerpt, &retrievedAt,
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
	r.Source = domain.ReleaseSource{
		URL:      str(sourceURL),
		Kind:     domain.PublicSourceKind(domain.SourceType(str(sourceType))),
		Official: sourceOfficial != nil && *sourceOfficial,
	}
	r.Evidence = domain.ReleaseEvidence{
		Excerpt:     str(excerpt),
		RetrievedAt: tim(retrievedAt),
	}
	return r, nil
}

// GetByID returns the release with the given id, or domain.ErrNotFound. Withdrawn
// releases are returned: a consumer who has the identifier is entitled to see what
// happened to it.
func (r *ReleaseRepo) GetByID(ctx context.Context, id string) (domain.Release, error) {
	row := r.db.q(ctx).QueryRow(ctx,
		`SELECT`+releaseColumns+` FROM releases r`+releaseJoins+` WHERE r.id = $1`, id)
	rel, err := scanRelease(row)
	if err != nil {
		return domain.Release{}, wrap("release.GetByID", err)
	}
	return rel, nil
}

// ProductRefForRelease resolves the product a release is mapped to, for GetByID's
// caller (GET /releases/{id}) which has no product context of its own to build a
// ProductRef from the way a product-scoped read already can. Ordered by slug so a
// release mapped to more than one product resolves the same way on every call.
func (r *ReleaseRepo) ProductRefForRelease(ctx context.Context, releaseID string) (application.ReleaseProductRef, error) {
	var ref application.ReleaseProductRef
	err := r.db.q(ctx).QueryRow(ctx,
		`SELECT p.slug, p.name
           FROM release_product_mappings m
           JOIN products p ON p.id = m.product_id
          WHERE m.release_id = $1
          ORDER BY p.slug
          LIMIT 1`, releaseID).Scan(&ref.Slug, &ref.Name)
	if err != nil {
		return application.ReleaseProductRef{}, wrap("release.ProductRefForRelease", err)
	}
	return ref, nil
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

// latestOrdering is domain.LatestComparison written as an ORDER BY over the releases
// table under the given alias.
//
// It is a function rather than a literal repeated in each query because two statements
// in this file have to agree about which release is "the latest": LatestForProduct,
// which answers GET /products/{slug}/latest, and the LATERAL in RefreshProductSummary
// that fills product_summaries.latest_release_id, which is the headline version on the
// product page. Those two used to ask different questions -- per channel and across all
// channels -- so the same product could be told to run one version by the endpoint and
// shown another on its own page, in the same second, with nothing in either answer
// saying which was wrong. One definition is what stops that drifting back; if a future
// change touches the rule, it touches it for both readers or for neither.
//
// The rule is release date first, then first-observed time. Never the version string:
// version strings are opaque under ADR-0017 and have no order at all. NULLS LAST is
// what makes a known date beat an unknown one, matching domain.LatestComparison's first
// two cases -- an undated observation is weaker evidence of recency than a dated one.
// The id is a third key purely for determinism, covering the case where the domain rule
// returns 0 and has no opinion: without it PostgreSQL may return either row, and the
// endpoint and the product page could disagree again through nothing but plan choice.
// Identifiers are ULIDs, so the later-minted row wins, which is the direction the other
// two keys already point.
//
// The rule now lives in two languages -- here and in domain.LatestComparison, which the
// publication use case applies when it sets is_latest_observed. That is the same
// duplication ListForProduct documents for PartialDate.PeriodEnd, and it is pinned the
// same way: TestLatestForProductOrdersLikeDomainLatestComparison asserts the two agree
// against a real database. If they ever drift, the flag would mark one release and this
// ORDER BY would pick another among the flagged rows of different channels.
//
// The alias is always a literal written in this file, never caller data; see the
// package comment's second rule.
func latestOrdering(alias string) string {
	return ` ORDER BY ` + alias + `.release_date DESC NULLS LAST, ` +
		alias + `.first_observed_at DESC, ` + alias + `.id DESC`
}

// The two spellings this file needs: "r" is how LatestForProduct aliases releases in
// its own FROM clause, "rr" how RefreshProductSummary's LATERAL joins alias it.
var (
	latestOrderingR  = latestOrdering("r")
	latestOrderingRR = latestOrdering("rr")
)

// LatestForProduct returns the latest-observed release for a product, restricted to one
// channel when a channel is given, or domain.ErrNotFound when there is none to serve.
//
// An empty channel means "any channel", not "the channel whose name is the empty
// string". openapi.yaml documents ?channel as optional, and this is the endpoint whose
// whole purpose is answering "what version should I be on?", so its own default call
// must not fail. Comparing $2 for equality against the mapping's channel coalesced to
// the empty string -- what this predicate used to do unconditionally -- turned an
// omitted channel into a demand for a mapping carrying no channel at all, so every
// product whose releases are all channelled answered 404 to exactly the call the
// documentation tells a caller to make: RouterOS, with 44 published releases across
// stable, long_term and testing, returned 404 without ?channel and 200 with it. Worse, that 404 was once read as a
// deliberate consequence of the unresolved RouterOS source conflict, so the bug cost a
// wrong diagnosis before it cost a fix.
//
// When a channel is given the predicate is still COALESCE of the mapping's channel with
// the empty string, character for character the expression in
// release_mappings_latest_idx, so this query and that uniqueness guarantee agree about
// what "the same channel" means. The accepted consequence is that no caller can ask for
// the channel-less mapping specifically. That costs nothing over HTTP, where an omitted
// parameter and an empty one arrive identically, and "any channel" subsumes it. It does
// change one thing for the publication use case, which passes the candidate's own
// channel here: for a channel-less candidate on a product that also has channelled
// releases, the comparison is now against the newest release of any channel rather than
// the newest channel-less one. That is the honest reading of "an omitted channel does
// not restrict", and it cannot corrupt the flag -- domain.LatestComparison still decides,
// ClearLatestFlag still scopes its UPDATE to one channel through the same COALESCE, and
// the partial unique index is per channel either way.
//
// Several channels can each hold a latest-observed flag, so an unrestricted call has to
// choose between them, and it chooses with latestOrdering -- the same rule
// product_summaries.latest_release_id is built with, so the endpoint and the product
// page can no longer name different releases for the same product at the same moment.
// The flag is read rather than the answer derived from scratch, because deriving it
// would mean ordering version strings, which FirmScout never does; the flag itself is
// maintained by the publication use case through domain.LatestComparison.
//
// Withdrawn releases are excluded, in SQL and again through domain.Release.Serveable.
// A withdrawn release is one the vendor pulled, and offering it as the answer to "what
// should I be running" repeats a retraction as advice -- the most dangerous single
// answer this API can give, and one the product page already refused to give while this
// query still gave it. Both checks are kept rather than one: the SQL predicate is what
// makes the *choice* correct, because without it the ordering can settle on a withdrawn
// release and never look at the serveable one behind it, while the Serveable call after
// the scan is what keeps the meaning of "serveable" in the domain, so that a future
// qualification of the rule is written once instead of grepped for. Serveable had no
// caller at all before this; a rule nothing calls is a rule nothing enforces.
func (r *ReleaseRepo) LatestForProduct(ctx context.Context, productID, channel string) (domain.Release, error) {
	row := r.db.q(ctx).QueryRow(ctx,
		`SELECT`+releaseColumns+`
           FROM releases r`+releaseJoins+`
          WHERE r.withdrawn = false
            AND EXISTS (SELECT 1 FROM release_product_mappings m
                         WHERE m.release_id = r.id
                           AND m.product_id = $1
                           AND ($2::text = '' OR COALESCE(m.channel, '') = $2)
                           AND m.is_latest_observed = true)`+latestOrderingR+`
          LIMIT 1`, productID, channel)
	rel, err := scanRelease(row)
	if err != nil {
		return domain.Release{}, wrap("release.LatestForProduct", err)
	}
	if !rel.Serveable() {
		return domain.Release{}, notFound("release.LatestForProduct")
	}
	return rel, nil
}

// ListForProduct returns a page of a product's releases, newest observed first, with
// the cursor for the next page.
//
// The ordering is (first_observed_at, id) descending rather than by version, because
// version strings have no order. The id is part of the sort key so the keyset cursor
// is unambiguous when two releases were observed in the same instant.
//
// opts.Since windows the page to what a caller's plan entitles them to see (api.md §2).
// A release is inside the window when the vendor's own release date could fall on or
// after Since, and -- only when the vendor published no date at all -- when FirmScout
// first observed it on or after Since. The window is applied here, by the query, rather
// than by filtering a page this method already returned: filtering afterwards would
// consume a cursor for rows the caller never sees, and a windowed consumer would page
// through short, unexplained results. See application.ReleaseListOptions.
//
// The date comparison uses the end of the period the stored precision denotes, not the
// stored anchor. release_date is anchored to the earliest day the precision could mean
// -- day 1 for a month, 1 January for a year, which is what the schema's CHECK
// constraints enforce -- so comparing the anchor against the boundary drops a release
// dated only "2025" from a window opening in September, and a release dated "2025-09"
// from a window opening on the 5th. Those are hidden releases, and hiding one is worse
// than showing one that might be a few days early. The CASE below is the SQL spelling
// of domain.PartialDate.PeriodEnd, and TestListForProductWindowKeepsReducedPrecisionDates
// asserts the two agree against a real database, because the rule now lives in two
// languages.
//
// The boundary is reduced to a date AT TIME ZONE 'UTC' rather than with a bare ::date
// cast. A bare cast resolves a timestamptz in the *session's* TimeZone, so a server
// running in America/Sao_Paulo turns a boundary of 2025-09-05T00:00:00Z into the date
// 2025-09-04 and quietly widens every window by a day, while a server in Asia/Tokyo
// narrows it by one and hides releases. release_date holds UTC anchors and Since is
// normalised to UTC above, so UTC is the only frame in which the two sides of this
// comparison mean the same thing; leaving it to a deployment's TimeZone setting makes
// the answer depend on where the database happens to run.
func (r *ReleaseRepo) ListForProduct(ctx context.Context, productID string, opts application.ReleaseListOptions) ([]domain.Release, string, error) {
	// The page bounds are the application's, not this adapter's: api.md §2 documents
	// them for the endpoint, application.ListReleases enforces them for every caller,
	// and restating them as literals here is how the documented default drifted the
	// last time. See internal/adapters/postgres/review_repo.go for the same pattern.
	limit := clampLimit(opts.Limit, application.DefaultReleasePageSize, application.MaxReleasePageSize)
	parts, err := decodeCursor(opts.Cursor, 2)
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
	var since *time.Time
	if !opts.Since.IsZero() {
		s := opts.Since.UTC()
		since = &s
	}

	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT`+releaseColumns+`
           FROM releases r`+releaseJoins+`
          WHERE EXISTS (SELECT 1 FROM release_product_mappings m
                         WHERE m.release_id = r.id AND m.product_id = $1)
            AND ($2::timestamptz IS NULL
                 OR (r.first_observed_at, r.id) < ($2::timestamptz, $3::text))
            AND ($5::timestamptz IS NULL
                 OR CASE WHEN r.release_date IS NOT NULL AND r.release_date_precision <> 'unknown'
                         THEN (CASE r.release_date_precision
                                 WHEN 'month_only' THEN (r.release_date + INTERVAL '1 month' - INTERVAL '1 day')::date
                                 WHEN 'year_only'  THEN (r.release_date + INTERVAL '1 year'  - INTERVAL '1 day')::date
                                 ELSE r.release_date
                               END) >= (($5::timestamptz) AT TIME ZONE 'UTC')::date
                         ELSE r.first_observed_at >= $5::timestamptz END)
          ORDER BY r.first_observed_at DESC, r.id DESC
          LIMIT $4`, productID, afterTime, afterID, limit, since)
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
// Three things are treated differently. has_source_conflict is set from
// source_conflicts by asking one question with no policy in it -- EXISTS an open
// conflict for this product -- rather than by re-deriving whether a disagreement
// counts as a conflict: that decision belongs to domain.AssessSourceConflict and the
// authority ladder it implements (ADR-0020), and a second copy of that ladder in a
// CASE expression is exactly the drift that would let this page say "sources agree"
// while the pipeline disagrees. The four conflict_* columns are filled in by the same
// kind of fresh, no-policy lookup -- the open conflict with the latest detected_at, if
// there is one -- and are therefore reset to NULL by this statement exactly when
// has_source_conflict resets to false, on the same refresh, for the same reason: a
// resolved conflict should stop being reported as one, on both fields at once, never
// leaving a "resolved but the detail still says otherwise" state for a reader to
// trust by mistake. That LATERAL join surfaces one conflict, not all: a product where
// two different channels are independently disputed at the same moment reports only
// the more recently detected one, which is an accepted v1 gap (the has_source_conflict
// boolean is still true and correct; only the extra detail is partial) rather than a
// reason to make this column a list before anything needs it to be.
// advisory_count remains underivable -- there is still no advisories table, CVE
// correlation being Phase 4 scope -- so it is omitted from the update list and
// whatever is already there survives a refresh rather than being reset to a
// fabricated zero. search_vector is GENERATED ALWAYS and PostgreSQL computes it from
// the columns this statement does set.
//
// latest_release_id -- the headline version a fleet manager reads off the product page
// -- is chosen by latestOrdering, deliberately the same rule LatestForProduct applies
// to GET /products/{slug}/latest, and deliberately the same rule again in a second
// language as domain.LatestComparison, which the publication use case applies when it
// decides which mapping carries is_latest_observed. Three spellings, one rule: release
// date, then first-observed time, never the version string. They must not drift,
// because the page and the endpoint answer the same question for the same reader and a
// disagreement between them has no tie-breaker -- the page would show one version while
// the API told the same fleet to install another, each perfectly self-consistent, with
// nothing in either response admitting a second answer exists. The SQL pair cannot
// drift while latestOrdering is their single definition; that the SQL and the Go agree
// is asserted against a real database by
// TestLatestForProductOrdersLikeDomainLatestComparison. The recommended LATERAL below
// uses the same ordering for the same reason, because "which of these is the later
// observation" is the same question there.
//
// Two of the columns it sets are registry facts rather than release facts:
// model_identifier and runs. That makes this statement the point at which a hardware
// model becomes visible at all -- a product with no summary row is a 404 on the public
// API and absent from search -- and it makes the refresh order matter, because a
// summary rebuilt before a product's relationships are written records that the device
// runs nothing. The registry sync therefore refreshes last, after its relationships
// pass. model_identifier is deliberately not fed into the generated search_vector: a
// device is found by the model_number alias, which array_agg already flattens into
// aliases_text at weight B, so indexing the identifier again would index the same
// string twice.
func (r *ReleaseRepo) RefreshProductSummary(ctx context.Context, productID string) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO product_summaries (
            product_id, vendor_slug, vendor_name, product_slug, product_name, family_name,
            aliases, aliases_text, category_slugs, latest_release_id, latest_raw_version,
            latest_release_type, latest_channel, latest_release_date,
            latest_release_date_precision, recommended_release_id, release_count,
            lifecycle_status, model_identifier, runs, has_source_conflict, official_sources,
            conflict_channel, conflict_versions, conflict_source_count, conflict_detected_at,
            last_verified_at, refreshed_at
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
            p.model_identifier,
            COALESCE(runs.entries, '[]'::jsonb),
            EXISTS (SELECT 1 FROM source_conflicts sc
                     WHERE sc.product_id = p.id AND sc.state = 'open'),
            COALESCE(official.sources, '[]'::jsonb),
            conflict.channel,
            conflict.versions,
            conflict.source_count,
            conflict.detected_at,
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
                AND rr.withdrawn = false`+latestOrderingRR+`
              LIMIT 1
         ) latest ON true
         LEFT JOIN LATERAL (
             SELECT rr.id
               FROM release_product_mappings mm
               JOIN releases rr ON rr.id = mm.release_id
              WHERE mm.product_id = p.id
                AND rr.recommended = true
                AND rr.withdrawn = false`+latestOrderingRR+`
              LIMIT 1
         ) rec ON true
         -- The products this product runs -- for a hardware model, the operating system
         -- whose releases a fleet manager is actually looking for. Modelled on the
         -- official LATERAL directly below, and denormalised here for the same reason:
         -- product_summaries exists so that a product page is one indexed read, and a
         -- join to products at read time would give that up for a list that is one
         -- element long in every row this phase writes. The relation kind travels with
         -- each entry rather than being assumed to be runs_os, so that a second kind,
         -- when one is ever evidenced, cannot silently render as the first. Ordered by
         -- the target's slug so the array is stable across refreshes rather than
         -- depending on join order.
         LEFT JOIN LATERAL (
             SELECT jsonb_agg(
                        jsonb_build_object('slug', tp.slug, 'name', tp.name, 'kind', pr.relation_kind)
                        ORDER BY tp.slug
                    ) AS entries
               FROM product_relationships pr
               JOIN products tp ON tp.id = pr.to_product_id
              WHERE pr.from_product_id = p.id
         ) runs ON true
         -- The distinct sources that have actually contributed a currently-mapped,
         -- non-withdrawn release to this product, ordered by slug so the array is
         -- stable across refreshes rather than depending on join order. "Kind" is
         -- stored as the raw sources.source_type value here, on purpose -- see the
         -- migration's comment for why the source_type -> public-kind mapping lives
         -- in exactly one place (domain.PublicSourceKind), applied when this column
         -- is read, not when it is written. A source registered for this product
         -- that has never produced an evidence-backed, mapped, non-withdrawn release
         -- has no row to contribute here and correctly never appears.
         LEFT JOIN LATERAL (
             SELECT jsonb_agg(contrib.entry ORDER BY contrib.slug) AS sources
               FROM (
                   SELECT DISTINCT src2.slug,
                          jsonb_build_object(
                              'slug', src2.slug,
                              'url', src2.source_url,
                              'kind', src2.source_type,
                              'official', src2.official
                          ) AS entry
                     FROM release_product_mappings mm2
                     JOIN releases rr2 ON rr2.id = mm2.release_id
                     JOIN evidence ev2 ON ev2.id = rr2.evidence_id
                     JOIN sources src2 ON src2.id = ev2.source_id
                    WHERE mm2.product_id = p.id AND rr2.withdrawn = false
               ) contrib
         ) official ON true
         -- The single open conflict for this product with the latest detected_at. See
         -- this function's doc comment for the "surfaces one, not all" limitation.
         LEFT JOIN LATERAL (
             SELECT sc.channel, sc.versions,
                    cardinality(sc.source_ids) AS source_count,
                    sc.detected_at
               FROM source_conflicts sc
              WHERE sc.product_id = p.id AND sc.state = 'open'
              ORDER BY sc.detected_at DESC
              LIMIT 1
         ) conflict ON true
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
            model_identifier              = EXCLUDED.model_identifier,
            runs                          = EXCLUDED.runs,
            has_source_conflict           = EXCLUDED.has_source_conflict,
            official_sources              = EXCLUDED.official_sources,
            conflict_channel              = EXCLUDED.conflict_channel,
            conflict_versions             = EXCLUDED.conflict_versions,
            conflict_source_count         = EXCLUDED.conflict_source_count,
            conflict_detected_at          = EXCLUDED.conflict_detected_at,
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
