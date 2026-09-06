package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

// newObservation builds a source's claim about a product and channel, with both
// timestamps set to at so a test can assert exactly what moves and what does not on a
// second call.
func newObservation(sourceID, productID, channel, rawVersion string, releaseDate domain.PartialDate, at time.Time) domain.SourceObservation {
	return domain.SourceObservation{
		SourceID:          sourceID,
		ProductID:         productID,
		Channel:           channel,
		RawVersion:        rawVersion,
		NormalizedVersion: rawVersion,
		ReleaseDate:       releaseDate,
		ObservedAt:        at,
		FirstObservedAt:   at,
	}
}

func TestSourceObservationsKeepOneRowPerSource(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	conflicts := NewConflictRepo(db)

	firstAt := time.Now().UTC().Add(-time.Hour)
	first := newObservation(e.SourceID, p.ID, "", "7.24.1", mustExactDate(t, 2026, time.January, 1), firstAt)
	if err := conflicts.RecordObservation(ctx, first); err != nil {
		t.Fatalf("first RecordObservation: %v", err)
	}

	secondAt := time.Now().UTC()
	second := newObservation(e.SourceID, p.ID, "", "7.24.2", mustExactDate(t, 2026, time.February, 14), secondAt)
	if err := conflicts.RecordObservation(ctx, second); err != nil {
		t.Fatalf("second RecordObservation: %v", err)
	}

	var n int
	if err := pool(db).QueryRow(ctx,
		`SELECT count(*) FROM source_observations WHERE source_id = $1 AND product_id = $2`,
		e.SourceID, p.ID).Scan(&n); err != nil {
		t.Fatalf("count observations: %v", err)
	}
	if n != 1 {
		t.Errorf("observation rows for one source = %d, want 1 (this is a projection, not a log)", n)
	}

	obs, err := conflicts.ObservationsForProduct(ctx, p.ID, "")
	if err != nil {
		t.Fatalf("ObservationsForProduct: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("observations = %d, want 1", len(obs))
	}
	if obs[0].RawVersion != "7.24.2" {
		t.Errorf("current observation = %q, want the second call's value 7.24.2", obs[0].RawVersion)
	}
	if got := obs[0].FirstObservedAt.Sub(firstAt).Abs(); got > time.Millisecond {
		t.Errorf("FirstObservedAt moved to %v, want it to stay at the first call's %v", obs[0].FirstObservedAt, firstAt)
	}
	if got := obs[0].ObservedAt.Sub(secondAt).Abs(); got > time.Millisecond {
		t.Errorf("ObservedAt = %v, want the second call's %v", obs[0].ObservedAt, secondAt)
	}
}

// TestSourceObservationDatePrecisionCheck proves the schema, not the adapter, refuses a
// month-precision observation date anchored anywhere but the first of the month -- the
// same discipline 00001 already enforces on candidate_releases and releases.
func TestSourceObservationDatePrecisionCheck(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)

	_, err := pool(db).Exec(ctx,
		`INSERT INTO source_observations (
            id, source_id, product_id, channel, raw_version, normalized_version,
            release_date, release_date_precision
         ) VALUES (
            'sobs_bad', $1, $2, '', '7.24.2', '7.24.2',
            DATE '2026-02-02', 'month_only'
         )`, e.SourceID, p.ID)
	if err == nil {
		t.Fatal("the database accepted a month-precision observation date anchored on day 2")
	}
	if !errors.Is(translate(err), domain.ErrValidation) {
		t.Errorf("check violation translated to %v, want domain.ErrValidation", translate(err))
	}

	// The correctly anchored value is accepted.
	if _, err := pool(db).Exec(ctx,
		`INSERT INTO source_observations (
            id, source_id, product_id, channel, raw_version, normalized_version,
            release_date, release_date_precision
         ) VALUES (
            'sobs_ok', $1, $2, '', '7.24.2', '7.24.2',
            DATE '2026-02-01', 'month_only'
         )`, e.SourceID, p.ID); err != nil {
		t.Errorf("a correctly anchored month-precision date was rejected: %v", err)
	}
}

// TestObservationsForProductResolvesEligibility proves the eligibility predicate this
// package computes in SQL agrees with the source registry: a source whose terms review
// is pending is ineligible, and a fully compliant one is eligible, without either
// observation being filtered out of the result.
func TestObservationsForProductResolvesEligibility(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	sources := NewSourceRepo(db)
	conflicts := NewConflictRepo(db)

	compliant := newSource("src_compliant", v.ID, "compliant")
	if err := sources.Upsert(ctx, compliant); err != nil {
		t.Fatalf("seed compliant source: %v", err)
	}
	pending := newSource("src_pending", v.ID, "pending")
	pending.TermsReviewStatus = domain.TermsPending
	pending.Enabled = false
	if err := sources.Upsert(ctx, pending); err != nil {
		t.Fatalf("seed pending source: %v", err)
	}

	now := time.Now().UTC()
	if err := conflicts.RecordObservation(ctx,
		newObservation(compliant.ID, p.ID, "", "7.24.2", mustExactDate(t, 2026, time.February, 14), now)); err != nil {
		t.Fatalf("record compliant observation: %v", err)
	}
	if err := conflicts.RecordObservation(ctx,
		newObservation(pending.ID, p.ID, "", "7.24.1", mustExactDate(t, 2026, time.February, 10), now)); err != nil {
		t.Fatalf("record pending observation: %v", err)
	}

	obs, err := conflicts.ObservationsForProduct(ctx, p.ID, "")
	if err != nil {
		t.Fatalf("ObservationsForProduct: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("observations = %d, want 2 (an ineligible source's claim is returned, not filtered)", len(obs))
	}
	byID := map[string]domain.SourceObservation{}
	for _, o := range obs {
		byID[o.SourceID] = o
	}
	if !byID[compliant.ID].Eligible {
		t.Error("the fully compliant source came back ineligible")
	}
	if byID[pending.ID].Eligible {
		t.Error("the pending-terms-review source came back eligible")
	}
}

// newConflict builds the conflict UpsertOpenConflict is asked to open or refresh. It
// does not set State: the repository always writes the literal 'open' for a call that
// reaches the insert branch, so what State the caller sets here is not the point of the
// test.
func newConflict(productID, channel string, versions, sourceIDs []string) domain.SourceConflict {
	return domain.SourceConflict{
		ProductID:     productID,
		Channel:       channel,
		AuthorityRank: domain.QualityOfficialManufacturer.Authority(),
		Versions:      versions,
		SourceIDs:     sourceIDs,
	}
}

// TestUpsertOpenConflictIsIdempotent is the revert detector for D5: without the partial
// unique index and the read-before-write this method does, every re-check of two
// disagreeing sources would open a second conflict.
func TestUpsertOpenConflictIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	conflicts := NewConflictRepo(db)
	reviews := NewReviewRepo(db)

	c := newConflict(p.ID, "", []string{"7.24.1", "7.24.2"}, []string{"src_a", "src_b"})
	first, created, err := conflicts.UpsertOpenConflict(ctx, c)
	if err != nil {
		t.Fatalf("first UpsertOpenConflict: %v", err)
	}
	if !created {
		t.Error("first upsert reported created=false, want true")
	}
	if first.State != domain.ConflictOpen {
		t.Errorf("state = %q, want open", first.State)
	}

	second, created, err := conflicts.UpsertOpenConflict(ctx, c)
	if err != nil {
		t.Fatalf("second UpsertOpenConflict: %v", err)
	}
	if created {
		t.Error("second upsert reported created=true, want false: the conflict already existed")
	}
	if second.ID != first.ID {
		t.Errorf("second upsert returned a different conflict %q, want the existing %q", second.ID, first.ID)
	}
	if second.LastSeenAt.Before(first.LastSeenAt) {
		t.Errorf("last_seen_at regressed from %v to %v", first.LastSeenAt, second.LastSeenAt)
	}

	var n int
	if err := pool(db).QueryRow(ctx,
		`SELECT count(*) FROM source_conflicts WHERE product_id = $1 AND state = 'open'`, p.ID).Scan(&n); err != nil {
		t.Fatalf("count open conflicts: %v", err)
	}
	if n != 1 {
		t.Errorf("open conflict rows = %d, want 1: the partial unique index must prevent a duplicate", n)
	}

	// A different channel on the same product may carry its own open conflict at the
	// same time: the partial unique index is scoped to (product_id, channel).
	beta := newConflict(p.ID, "beta", []string{"7.24.0-beta1", "7.24.0-beta2"}, []string{"src_a", "src_b"})
	if _, created, err := conflicts.UpsertOpenConflict(ctx, beta); err != nil {
		t.Fatalf("UpsertOpenConflict for a different channel: %v", err)
	} else if !created {
		t.Error("a different channel's conflict was not reported as newly created")
	}

	// LinkReviewItem, ResolveConflict, GetConflict and OpenConflictFor round-trip.
	// review_item_id is a real foreign key, so the review item has to exist first.
	item := applicationReviewItem(v.ID, p.ID)
	item.ID = "rev_1"
	if err := reviews.Create(ctx, item); err != nil {
		t.Fatalf("seed review item: %v", err)
	}
	if err := conflicts.LinkReviewItem(ctx, first.ID, "rev_1"); err != nil {
		t.Fatalf("LinkReviewItem: %v", err)
	}
	got, err := conflicts.GetConflict(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetConflict: %v", err)
	}
	if got.ReviewItemID != "rev_1" {
		t.Errorf("ReviewItemID = %q, want rev_1", got.ReviewItemID)
	}

	open, err := conflicts.OpenConflictFor(ctx, p.ID, "")
	if err != nil {
		t.Fatalf("OpenConflictFor: %v", err)
	}
	if open.ID != first.ID {
		t.Errorf("OpenConflictFor = %q, want %q", open.ID, first.ID)
	}

	if err := conflicts.ResolveConflict(ctx, first.ID, "sources agree", "system", time.Now().UTC()); err != nil {
		t.Fatalf("ResolveConflict: %v", err)
	}
	if err := conflicts.ResolveConflict(ctx, first.ID, "again", "system", time.Now().UTC()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("re-resolving an already-resolved conflict = %v, want domain.ErrNotFound", err)
	}
	if _, err := conflicts.OpenConflictFor(ctx, p.ID, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("OpenConflictFor after resolution = %v, want domain.ErrNotFound", err)
	}

	all, err := conflicts.ListOpenConflicts(ctx, 10)
	if err != nil {
		t.Fatalf("ListOpenConflicts: %v", err)
	}
	if len(all) != 1 || all[0].Channel != "beta" {
		t.Errorf("ListOpenConflicts = %+v, want only the still-open beta conflict", all)
	}

	closed, err := conflicts.CloseOpenConflict(ctx, p.ID, "beta", "resolved by source authority", "system", time.Now().UTC())
	if err != nil {
		t.Fatalf("CloseOpenConflict: %v", err)
	}
	if !closed {
		t.Error("CloseOpenConflict reported closed=false for a conflict that was open")
	}
	closedAgain, err := conflicts.CloseOpenConflict(ctx, p.ID, "beta", "resolved by source authority", "system", time.Now().UTC())
	if err != nil {
		t.Fatalf("CloseOpenConflict on an already-closed channel: %v", err)
	}
	if closedAgain {
		t.Error("CloseOpenConflict reported closed=true when there was nothing open; a no-op must not be an error, but it must not lie either")
	}
}

// TestRefreshProductSummarySetsHasSourceConflict is the revert detector for W1's read
// path: has_source_conflict must ask source_conflicts, not carry a value nobody ever
// updates.
func TestRefreshProductSummarySetsHasSourceConflict(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, p := seedCatalog(t, db)
	releases := NewReleaseRepo(db)
	summaries := NewSummaryRepo(db)
	conflicts := NewConflictRepo(db)

	if err := releases.RefreshProductSummary(ctx, p.ID); err != nil {
		t.Fatalf("initial RefreshProductSummary: %v", err)
	}
	sum, err := summaries.Get(ctx, p.Slug)
	if err != nil {
		t.Fatalf("summary Get: %v", err)
	}
	if sum.HasSourceConflict {
		t.Error("a freshly refreshed product with no conflict reports HasSourceConflict=true")
	}

	c := newConflict(p.ID, "", []string{"7.24.1", "7.24.2"}, []string{"src_a", "src_b"})
	if _, _, err := conflicts.UpsertOpenConflict(ctx, c); err != nil {
		t.Fatalf("UpsertOpenConflict: %v", err)
	}
	if err := releases.RefreshProductSummary(ctx, p.ID); err != nil {
		t.Fatalf("RefreshProductSummary after opening a conflict: %v", err)
	}
	sum, err = summaries.Get(ctx, p.Slug)
	if err != nil {
		t.Fatalf("summary Get: %v", err)
	}
	if !sum.HasSourceConflict {
		t.Error("HasSourceConflict is false with an open conflict on the product")
	}

	if _, err := conflicts.CloseOpenConflict(ctx, p.ID, "", "sources agree", "system", time.Now().UTC()); err != nil {
		t.Fatalf("CloseOpenConflict: %v", err)
	}
	if err := releases.RefreshProductSummary(ctx, p.ID); err != nil {
		t.Fatalf("RefreshProductSummary after closing the conflict: %v", err)
	}
	sum, err = summaries.Get(ctx, p.Slug)
	if err != nil {
		t.Fatalf("summary Get: %v", err)
	}
	if sum.HasSourceConflict {
		t.Error("HasSourceConflict stayed true after the only conflict was resolved")
	}
}
