package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// newCandidate builds a candidate whose dedupe key is computed the way the SDK
// computes it, so the uniqueness the schema enforces is the uniqueness the domain
// intends.
func newCandidate(t *testing.T, sourceID, productID, rawVersion string) domain.CandidateRelease {
	t.Helper()
	v := mustVersion(t, rawVersion)
	app := domain.Applicability{Channel: "stable"}
	return domain.CandidateRelease{
		SourceID:           sourceID,
		ProductID:          productID,
		ProductMatchHint:   "CCR2004",
		ProductMatchStatus: domain.MatchUnique,
		Version:            v,
		ReleaseType:        domain.ReleaseTypeFirmware,
		Applicability:      app,
		ReleaseDate:        mustExactDate(t, 2026, time.February, 14),
		ReleaseNotesURL:    "https://mikrotik.example/changelog#7-24-2",
		Confidence:         0.9,
		DedupeKey:          domain.ComputeDedupeKey("CCR2004", v, app),
		State:              domain.CandidateExtracted,
	}
}

func TestUpsertByDedupeKeyReportsCreation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	candidates := NewCandidateRepo(db)

	c := newCandidate(t, e.SourceID, p.ID, "7.24.2")
	c.ID = "cand_1"
	c.EvidenceID = e.ID

	first, created, err := candidates.UpsertByDedupeKey(ctx, c)
	if err != nil {
		t.Fatalf("first UpsertByDedupeKey: %v", err)
	}
	if !created {
		t.Error("first upsert reported created=false, want true")
	}
	if first.ID != "cand_1" || first.Version.Raw() != "7.24.2" {
		t.Errorf("returned candidate = %+v", first)
	}
	if first.Applicability.Channel != "stable" {
		t.Errorf("channel = %q, want stable", first.Applicability.Channel)
	}
	if got := first.ReleaseDate.String(); got != "2026-02-14" {
		t.Errorf("release date = %q, want 2026-02-14", got)
	}

	// The same observation seen again on a later check: no new row.
	repeat := newCandidate(t, e.SourceID, p.ID, "7.24.2")
	repeat.ID = "cand_2" // a different id must not create a second row
	repeat.EvidenceID = e.ID

	second, created, err := candidates.UpsertByDedupeKey(ctx, repeat)
	if err != nil {
		t.Fatalf("second UpsertByDedupeKey: %v", err)
	}
	if created {
		t.Error("second upsert reported created=true; re-running a check must be idempotent")
	}
	if second.ID != "cand_1" {
		t.Errorf("second upsert returned id %q, want the existing cand_1", second.ID)
	}

	var n int
	if err := pool(db).QueryRow(ctx, `SELECT count(*) FROM candidate_releases`).Scan(&n); err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if n != 1 {
		t.Errorf("candidate rows = %d, want 1", n)
	}

	// A genuinely different version is a different dedupe key and therefore a new row.
	other := newCandidate(t, e.SourceID, p.ID, "7.25.0")
	other.EvidenceID = e.ID
	if _, created, err = candidates.UpsertByDedupeKey(ctx, other); err != nil {
		t.Fatalf("third UpsertByDedupeKey: %v", err)
	}
	if !created {
		t.Error("a different version reported created=false, want true")
	}
}

func TestCandidateStateAndValidation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	candidates := NewCandidateRepo(db)

	c := newCandidate(t, e.SourceID, p.ID, "7.24.2")
	c.ID = "cand_1"
	c.EvidenceID = e.ID
	if _, _, err := candidates.UpsertByDedupeKey(ctx, c); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}

	if err := candidates.UpdateState(ctx, c.ID, domain.CandidateRejected, "version already published"); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}
	got, err := candidates.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != domain.CandidateRejected || got.RejectionReason != "version already published" {
		t.Errorf("state = %q / %q", got.State, got.RejectionReason)
	}

	rejected, err := candidates.ListByState(ctx, domain.CandidateRejected, 10)
	if err != nil {
		t.Fatalf("ListByState: %v", err)
	}
	if len(rejected) != 1 {
		t.Errorf("rejected candidates = %d, want 1", len(rejected))
	}

	now := time.Now().UTC()
	results := []domain.GateResult{
		{Gate: domain.GateProductIdentity, Outcome: domain.GatePassed, Detail: "unique match", EvaluatedAt: now},
		{Gate: domain.GateDuplicate, Outcome: domain.GateRejected, Detail: "rel_1", EvaluatedAt: now},
	}
	if err := candidates.RecordValidation(ctx, c.ID, results); err != nil {
		t.Fatalf("RecordValidation: %v", err)
	}
	// Re-running replaces rather than appends.
	if err := candidates.RecordValidation(ctx, c.ID, results); err != nil {
		t.Fatalf("second RecordValidation: %v", err)
	}

	var n int
	if err := pool(db).QueryRow(ctx,
		`SELECT count(*) FROM validation_results WHERE candidate_id = $1`, c.ID).Scan(&n); err != nil {
		t.Fatalf("count validation results: %v", err)
	}
	if n != 2 {
		t.Errorf("validation rows = %d, want 2 after two identical runs", n)
	}

	var order int
	if err := pool(db).QueryRow(ctx,
		`SELECT gate_order FROM validation_results WHERE candidate_id = $1 AND gate = $2`,
		c.ID, string(domain.GateDuplicate)).Scan(&order); err != nil {
		t.Fatalf("read gate order: %v", err)
	}
	if order != domain.GateIndex(domain.GateDuplicate) {
		t.Errorf("gate_order = %d, want %d", order, domain.GateIndex(domain.GateDuplicate))
	}

	if err := candidates.UpdateState(ctx, "cand_missing", domain.CandidateRejected, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("UpdateState on a missing candidate = %v, want domain.ErrNotFound", err)
	}
}

// TestListValidationResultsReturnsGateOrder proves the read side RecordValidation had no
// caller for: a reviewer needs to see which gate routed a candidate to review, not just
// that it was routed, and needs to see the gates in the order they ran rather than in
// whatever order the rows happened to land.
func TestListValidationResultsReturnsGateOrder(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	candidates := NewCandidateRepo(db)

	c := newCandidate(t, e.SourceID, p.ID, "7.24.2")
	c.ID = "cand_gates"
	c.EvidenceID = e.ID
	if _, _, err := candidates.UpsertByDedupeKey(ctx, c); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}

	now := time.Now().UTC()
	// Deliberately out of gate order, so the test proves ORDER BY gate_order does the
	// work rather than insertion order happening to agree with it.
	results := []domain.GateResult{
		{Gate: domain.GateDuplicate, Outcome: domain.GatePassed, Detail: "no duplicate", EvaluatedAt: now},
		{Gate: domain.GateProductIdentity, Outcome: domain.GatePassed, Detail: "unique match", EvaluatedAt: now},
		{Gate: domain.GateVersionPresent, Outcome: domain.GatePassed, Detail: "version present", EvaluatedAt: now},
	}
	if err := candidates.RecordValidation(ctx, c.ID, results); err != nil {
		t.Fatalf("RecordValidation: %v", err)
	}

	got, err := candidates.ListValidationResults(ctx, c.ID)
	if err != nil {
		t.Fatalf("ListValidationResults: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("results = %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Order > got[i].Order {
			t.Fatalf("results not in gate order: %+v", got)
		}
	}
	if got[0].Gate != domain.GateProductIdentity {
		t.Errorf("first gate = %q, want %q (the lowest gate order)", got[0].Gate, domain.GateProductIdentity)
	}
	if got[0].Detail != "unique match" {
		t.Errorf("detail = %q, want it to round-trip", got[0].Detail)
	}

	empty, err := candidates.ListValidationResults(ctx, "cand_missing")
	if err != nil {
		t.Fatalf("ListValidationResults for a candidate with no rows: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("results for an unknown candidate = %d, want 0", len(empty))
	}
}

// TestDatePrecisionCheckRejectsMisanchoredDate proves the schema, not the adapter, is
// what makes a month-precision date impossible to fake. The domain constructor refuses
// to build such a value at all, so the only way to attempt it is to write the row
// directly -- which is exactly what a future adapter bug or a manual UPDATE would do.
func TestDatePrecisionCheckRejectsMisanchoredDate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)

	_, err := pool(db).Exec(ctx,
		`INSERT INTO candidate_releases (
            id, source_id, product_id, raw_version, normalized_version,
            release_date, release_date_precision, dedupe_key, state
         ) VALUES (
            'cand_bad', $1, $2, '7.24.2', '7.24.2',
            DATE '2026-02-14', 'month_only', 'k1', 'extracted'
         )`, e.SourceID, p.ID)
	if err == nil {
		t.Fatal("the database accepted a month-precision date anchored on day 14")
	}
	if !errors.Is(translate(err), domain.ErrValidation) {
		t.Errorf("check violation translated to %v, want domain.ErrValidation", translate(err))
	}

	// A year-precision date must be anchored on 1 January, not merely on day 1.
	_, err = pool(db).Exec(ctx,
		`INSERT INTO candidate_releases (
            id, source_id, product_id, raw_version, normalized_version,
            release_date, release_date_precision, dedupe_key, state
         ) VALUES (
            'cand_bad2', $1, $2, '7.24.2', '7.24.2',
            DATE '2026-02-01', 'year_only', 'k2', 'extracted'
         )`, e.SourceID, p.ID)
	if err == nil {
		t.Fatal("the database accepted a year-precision date anchored in February")
	}

	// The correctly anchored value goes in, and comes back at the precision it was
	// stored with rather than as a day.
	candidates := NewCandidateRepo(db)
	c := newCandidate(t, e.SourceID, p.ID, "7.24.2")
	c.ID = "cand_month"
	c.EvidenceID = e.ID
	c.ReleaseDate = mustMonthDate(t, 2026, time.February)
	if _, _, err := candidates.UpsertByDedupeKey(ctx, c); err != nil {
		t.Fatalf("month-precision upsert: %v", err)
	}
	got, err := candidates.GetByID(ctx, "cand_month")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ReleaseDate.Precision() != domain.PrecisionMonthOnly {
		t.Errorf("precision = %q, want month_only", got.ReleaseDate.Precision())
	}
	if s := got.ReleaseDate.String(); s != "2026-02" {
		t.Errorf("date rendered as %q, want 2026-02", s)
	}
	if _, ok := got.ReleaseDate.ExactDay(); ok {
		t.Error("a month-precision date came back with a day component")
	}
}

func newRelease(t *testing.T, id, vendorID, evidenceID, rawVersion, channel string, observed time.Time) domain.Release {
	t.Helper()
	return domain.Release{
		ID:               id,
		VendorID:         vendorID,
		Version:          mustVersion(t, rawVersion),
		ReleaseType:      domain.ReleaseTypeFirmware,
		Channel:          channel,
		ReleaseDate:      mustExactDate(t, 2026, time.February, 14),
		FirstObservedAt:  observed,
		LastVerifiedAt:   observed,
		PublishedAt:      observed,
		EvidenceID:       evidenceID,
		SourceConfidence: 0.9,
	}
}

func TestReleaseInsertMappingAndLatestFlag(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)

	observed := time.Now().UTC().Add(-time.Hour)
	r1 := newRelease(t, "rel_1", v.ID, e.ID, "7.24.2", "stable", observed)
	m1 := domain.ReleaseProductMapping{
		ID:               "rmap_1",
		ReleaseID:        r1.ID,
		ProductID:        p.ID,
		Applicability:    domain.Applicability{Channel: "stable"},
		IsLatestObserved: true,
	}
	if err := releases.Insert(ctx, r1, []domain.ReleaseProductMapping{m1}); err != nil {
		t.Fatalf("Insert r1: %v", err)
	}

	got, err := releases.GetByID(ctx, r1.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Version.Raw() != "7.24.2" || got.Channel != "stable" ||
		got.ReleaseType != domain.ReleaseTypeFirmware || got.EvidenceID != e.ID {
		t.Errorf("round trip lost data: %+v", got)
	}

	latest, err := releases.LatestForProduct(ctx, p.ID, "stable")
	if err != nil {
		t.Fatalf("LatestForProduct: %v", err)
	}
	if latest.ID != r1.ID {
		t.Errorf("latest = %q, want rel_1", latest.ID)
	}

	if id, err := releases.FindDuplicate(ctx, p.ID, "7.24.2", "stable"); err != nil || id != r1.ID {
		t.Errorf("FindDuplicate = (%q, %v), want (rel_1, nil)", id, err)
	}
	if id, err := releases.FindDuplicate(ctx, p.ID, "7.25.0", "stable"); err != nil || id != "" {
		t.Errorf("FindDuplicate for an unseen version = (%q, %v), want (\"\", nil)", id, err)
	}

	// A second latest-observed mapping for the same product and channel must be
	// refused by the partial unique index, not silently accepted.
	r2 := newRelease(t, "rel_2", v.ID, e.ID, "7.25.0", "stable", observed.Add(time.Minute))
	m2 := domain.ReleaseProductMapping{
		ID:               "rmap_2",
		ReleaseID:        r2.ID,
		ProductID:        p.ID,
		Applicability:    domain.Applicability{Channel: "stable"},
		IsLatestObserved: true,
	}
	err = releases.Insert(ctx, r2, []domain.ReleaseProductMapping{m2})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second latest-observed mapping = %v, want domain.ErrConflict", err)
	}

	// The failed insert must have rolled back entirely: no orphaned release row.
	if _, err := releases.GetByID(ctx, r2.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("rel_2 exists after a failed Insert (%v); the transaction did not roll back", err)
	}

	// Clearing the flag first is what publication does, and then the insert succeeds.
	if err := releases.ClearLatestFlag(ctx, p.ID, "stable"); err != nil {
		t.Fatalf("ClearLatestFlag: %v", err)
	}
	if err := releases.Insert(ctx, r2, []domain.ReleaseProductMapping{m2}); err != nil {
		t.Fatalf("Insert r2 after clearing: %v", err)
	}
	latest, err = releases.LatestForProduct(ctx, p.ID, "stable")
	if err != nil {
		t.Fatalf("LatestForProduct after publication: %v", err)
	}
	if latest.ID != r2.ID {
		t.Errorf("latest = %q, want rel_2", latest.ID)
	}

	// Clearing a flag that is not set is not an error.
	if err := releases.ClearLatestFlag(ctx, p.ID, "beta"); err != nil {
		t.Errorf("ClearLatestFlag on an unused channel: %v", err)
	}

	list, _, err := releases.ListForProduct(ctx, p.ID, application.ReleaseListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListForProduct: %v", err)
	}
	if len(list) != 2 || list[0].ID != r2.ID {
		t.Errorf("ListForProduct = %d rows, newest first? got %+v", len(list), list)
	}
}

// TestReleaseSourceAndEvidencePopulatedOnEveryReadPath is the other half of
// domain.Release's Source/Evidence contract (see the domain package's
// TestReleaseSourceAndEvidenceAreZeroUntilTheReadPath): every method that reads a
// published release through releaseColumns must come back with real, non-fabricated
// source and evidence detail, joined from the real evidence and sources rows through
// EvidenceID -- never nil, never a placeholder.
//
// It also pins that Source.URL is the evidence's own recorded URL, not the source's
// currently registered one: seedEvidence's fixture deliberately gives them different
// values so a test that accidentally read the wrong table would fail here rather than
// pass by coincidence.
func TestReleaseSourceAndEvidencePopulatedOnEveryReadPath(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)

	observed := time.Now().UTC().Add(-time.Hour)
	r := newRelease(t, "rel_1", v.ID, e.ID, "7.24.2", "stable", observed)
	m := domain.ReleaseProductMapping{
		ID:               "rmap_1",
		ReleaseID:        r.ID,
		ProductID:        p.ID,
		Applicability:    domain.Applicability{Channel: "stable"},
		IsLatestObserved: true,
	}
	if err := releases.Insert(ctx, r, []domain.ReleaseProductMapping{m}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	checkSourceAndEvidence := func(t *testing.T, label string, got domain.Release) {
		t.Helper()
		if got.Source.URL != e.SourceURL {
			t.Errorf("%s: Source.URL = %q, want the evidence's own URL %q (not the source's registered URL)", label, got.Source.URL, e.SourceURL)
		}
		if got.Source.Kind != domain.PublicKindHTML {
			t.Errorf("%s: Source.Kind = %q, want %q for an html_page source", label, got.Source.Kind, domain.PublicKindHTML)
		}
		if !got.Source.Official {
			t.Errorf("%s: Source.Official = false, want true", label)
		}
		if got.Evidence.Excerpt != e.Excerpt {
			t.Errorf("%s: Evidence.Excerpt = %q, want %q", label, got.Evidence.Excerpt, e.Excerpt)
		}
		if got.Evidence.RetrievedAt.IsZero() {
			t.Errorf("%s: Evidence.RetrievedAt is zero, want the real retrieval time", label)
		}
	}

	byID, err := releases.GetByID(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	checkSourceAndEvidence(t, "GetByID", byID)

	latest, err := releases.LatestForProduct(ctx, p.ID, "stable")
	if err != nil {
		t.Fatalf("LatestForProduct: %v", err)
	}
	checkSourceAndEvidence(t, "LatestForProduct", latest)

	list, _, err := releases.ListForProduct(ctx, p.ID, application.ReleaseListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListForProduct: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListForProduct returned %d releases, want 1", len(list))
	}
	checkSourceAndEvidence(t, "ListForProduct", list[0])
}

// TestProductRefForRelease is the revert detector for GET /releases/{id}'s second,
// previously-undiagnosed crash: the handler always called PresentRelease(release, nil)
// because GetByID's own request path carries no product context, and the release page
// dereferences release.product.slug with no guard -- an HTTP 500 this task's own
// background did not describe, found only by loading the page against the real dev
// database after the source/evidence fix, not by reading a diagram.
func TestProductRefForRelease(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)

	mapped := newRelease(t, "rel_mapped", v.ID, e.ID, "7.24.2", "stable", time.Now().UTC().Add(-time.Hour))
	if err := releases.Insert(ctx, mapped, []domain.ReleaseProductMapping{
		{ID: "rmap_mapped", ReleaseID: mapped.ID, ProductID: p.ID, Applicability: domain.Applicability{Channel: "stable"}},
	}); err != nil {
		t.Fatalf("Insert mapped release: %v", err)
	}
	ref, err := releases.ProductRefForRelease(ctx, mapped.ID)
	if err != nil {
		t.Fatalf("ProductRefForRelease: %v", err)
	}
	if ref.Slug != p.Slug || ref.Name != p.Name {
		t.Errorf("ProductRefForRelease = %+v, want slug %q name %q (the real product row, not a placeholder)", ref, p.Slug, p.Name)
	}

	// Insert's own invariant says this should be unreachable in production (a release
	// with no mapping is unreachable), but the method must still answer honestly
	// rather than panic or fabricate a ref if it ever happens.
	unmapped := newRelease(t, "rel_unmapped", v.ID, e.ID, "7.25.0", "stable", time.Now().UTC())
	if err := releases.Insert(ctx, unmapped, nil); err != nil {
		t.Fatalf("Insert unmapped release: %v", err)
	}
	if _, err := releases.ProductRefForRelease(ctx, unmapped.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("ProductRefForRelease for an unmapped release = %v, want domain.ErrNotFound", err)
	}
}

// TestReleaseSourceFallsBackToEvidenceWhenSourceIsGone proves the defensive half of
// releaseColumns' LEFT JOIN sources: evidence.source_id can be set NULL by a source
// deletion (ON DELETE SET NULL) after the evidence was recorded, and that must degrade
// the release's reported kind to whatever evidence itself recorded, never crash the
// scan or silently drop the release from a list.
func TestReleaseSourceFallsBackToEvidenceWhenSourceIsGone(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)

	observed := time.Now().UTC().Add(-time.Hour)
	r := newRelease(t, "rel_1", v.ID, e.ID, "7.24.2", "stable", observed)
	m := domain.ReleaseProductMapping{
		ID: "rmap_1", ReleaseID: r.ID, ProductID: p.ID,
		Applicability: domain.Applicability{Channel: "stable"}, IsLatestObserved: true,
	}
	if err := releases.Insert(ctx, r, []domain.ReleaseProductMapping{m}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Delete the source out from under the evidence. ON DELETE SET NULL is what the
	// schema does here, so this models a real, reachable state rather than an
	// artificial one.
	if _, err := pool(db).Exec(ctx, `DELETE FROM sources WHERE id = $1`, e.SourceID); err != nil {
		t.Fatalf("delete source: %v", err)
	}

	got, err := releases.GetByID(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetByID after the source was deleted: %v", err)
	}
	if got.Source.URL != e.SourceURL {
		t.Errorf("Source.URL = %q, want the evidence's own recorded URL %q to survive", got.Source.URL, e.SourceURL)
	}
	// evidence.source_type is still set (seedEvidence's fixture always sets it), so the
	// COALESCE never has to reach for the now-absent sources row.
	if got.Source.Kind != domain.PublicKindHTML {
		t.Errorf("Source.Kind = %q, want %q read from evidence's own source_type", got.Source.Kind, domain.PublicKindHTML)
	}
}

// TestListForProductWindow is the revert detector for W4's windowing: a release dated
// inside the window is returned regardless of when FirmScout observed it, a release
// dated outside it is excluded, and an undated release falls back to first_observed_at
// because there is no other honest signal to window it on.
func TestListForProductWindow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)

	now := time.Now().UTC()
	threeMonthsAgo := now.AddDate(0, -3, 0)
	eighteenMonthsAgo := now.AddDate(0, -18, 0)
	twoMonthsAgo := now.AddDate(0, -2, 0)

	recent := newRelease(t, "rel_recent", v.ID, e.ID, "7.24.2", "stable", now)
	recent.ReleaseDate = mustExactDate(t, threeMonthsAgo.Year(), threeMonthsAgo.Month(), threeMonthsAgo.Day())

	old := newRelease(t, "rel_old", v.ID, e.ID, "7.20.0", "stable", now)
	old.ReleaseDate = mustExactDate(t, eighteenMonthsAgo.Year(), eighteenMonthsAgo.Month(), eighteenMonthsAgo.Day())

	// No release date at all, but observed inside the window: windowing on
	// first_observed_at is the only honest signal available for it.
	undated := newRelease(t, "rel_undated", v.ID, e.ID, "7.25.0", "stable", twoMonthsAgo)
	undated.ReleaseDate = domain.UnknownDate

	for _, r := range []domain.Release{recent, old, undated} {
		m := domain.ReleaseProductMapping{
			ID: "rmap_" + r.ID, ReleaseID: r.ID, ProductID: p.ID,
			Applicability: domain.Applicability{Channel: "stable"},
		}
		if err := releases.Insert(ctx, r, []domain.ReleaseProductMapping{m}); err != nil {
			t.Fatalf("Insert %s: %v", r.ID, err)
		}
	}

	since := now.AddDate(0, -12, 0)
	windowed, _, err := releases.ListForProduct(ctx, p.ID, application.ReleaseListOptions{Limit: 10, Since: since})
	if err != nil {
		t.Fatalf("ListForProduct windowed: %v", err)
	}
	got := map[string]bool{}
	for _, r := range windowed {
		got[r.ID] = true
	}
	if !got["rel_recent"] {
		t.Error("a release dated inside the window was excluded")
	}
	if !got["rel_undated"] {
		t.Error("an undated release first observed inside the window was excluded")
	}
	if got["rel_old"] {
		t.Error("a release dated 18 months ago was returned by a 12-month window")
	}
	if len(windowed) != 2 {
		t.Errorf("windowed ListForProduct = %d releases, want 2", len(windowed))
	}

	// The zero value of Since means the complete archive: no window at all.
	all, _, err := releases.ListForProduct(ctx, p.ID, application.ReleaseListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListForProduct unwindowed: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unwindowed ListForProduct = %d releases, want 3", len(all))
	}
}

// TestListForProductWindowKeepsReducedPrecisionDates is the revert detector for the
// windowing correction. A vendor who dated a release only "2025" may have shipped it in
// December, and a vendor who dated one "2025-09" may have shipped it on the 30th, so
// neither may vanish from a window that opens on 5 September 2025. Comparing the stored
// anchor -- 1 January and 1 September respectively, which is where the schema's CHECK
// constraints put them -- drops both, and a dropped release is invisible to the caller:
// the page looks complete and nothing says a row was removed.
//
// It also pins the direction of the error. A release whose period ends before the
// boundary is still excluded, so the fix widens the window by exactly the width of the
// published precision and no further.
func TestListForProductWindowKeepsReducedPrecisionDates(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)

	since := time.Date(2025, time.September, 5, 0, 0, 0, 0, time.UTC)
	// Every release is first observed long before the window opens, so a row can only
	// be returned on the strength of its release date. A predicate that fell back to
	// first_observed_at for a dated release would return nothing at all.
	observed := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		id      string
		version string
		date    domain.PartialDate
		want    bool
		why     string
	}{
		{"rel_year_2025", "1.0.0", mustYearDate(t, 2025), true,
			"a release dated only 2025 could have shipped in December"},
		{"rel_month_2025_09", "1.1.0", mustMonthDate(t, 2025, time.September), true,
			"a release dated 2025-09 could have shipped on the 30th"},
		{"rel_day_on_boundary", "1.2.0", mustExactDate(t, 2025, time.September, 5), true,
			"the boundary itself is inside the window"},
		{"rel_day_before", "1.3.0", mustExactDate(t, 2025, time.September, 4), false,
			"a day precisely one before the boundary is outside it"},
		{"rel_month_2025_08", "1.4.0", mustMonthDate(t, 2025, time.August), false,
			"August ends on the 31st, which is before the window opens"},
		{"rel_year_2024", "1.5.0", mustYearDate(t, 2024), false,
			"2024 ends on 31 December 2024, which is before the window opens"},
	}

	for _, tc := range cases {
		r := newRelease(t, tc.id, v.ID, e.ID, tc.version, "stable", observed)
		r.ReleaseDate = tc.date
		m := domain.ReleaseProductMapping{
			ID: "rmap_" + r.ID, ReleaseID: r.ID, ProductID: p.ID,
			Applicability: domain.Applicability{Channel: "stable"},
		}
		if err := releases.Insert(ctx, r, []domain.ReleaseProductMapping{m}); err != nil {
			t.Fatalf("Insert %s: %v", r.ID, err)
		}
	}

	windowed, _, err := releases.ListForProduct(ctx, p.ID, application.ReleaseListOptions{Limit: 50, Since: since})
	if err != nil {
		t.Fatalf("ListForProduct windowed: %v", err)
	}
	got := map[string]bool{}
	for _, r := range windowed {
		got[r.ID] = true
	}

	for _, tc := range cases {
		if got[tc.id] != tc.want {
			t.Errorf("%s (%s, precision %s): in window = %v, want %v -- %s",
				tc.id, tc.date, tc.date.Precision(), got[tc.id], tc.want, tc.why)
		}
		// The SQL CASE and domain.PartialDate.PeriodEnd are the same rule written in
		// two languages, so the query's verdict is checked against the domain's rather
		// than only against a hand-written expectation. A change to either that the
		// other does not follow fails here.
		wantByDomain := !tc.date.PeriodEnd().Before(since)
		if got[tc.id] != wantByDomain {
			t.Errorf("%s: the query says in-window = %v, domain.PeriodEnd (%s) says %v; the SQL and the domain rule have diverged",
				tc.id, got[tc.id], tc.date.PeriodEnd().Format("2006-01-02"), wantByDomain)
		}
	}
	if len(windowed) != 3 {
		t.Errorf("windowed ListForProduct = %d releases, want 3", len(windowed))
	}
}

// TestListForProductDefaultPageSizeIsTheDocumentedOne is the revert detector for the
// page bound this adapter used to restate as a literal. It clamped an absent limit to
// 50 while api.md §2 documents a default of 20 for the endpoint, so a caller who
// reached the repository without going through application.ListReleases got a page size
// nothing documents. The bounds now come from the application constants, which is where
// the contract is enforced for every caller.
func TestListForProductDefaultPageSizeIsTheDocumentedOne(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)

	// More rows than the default page holds, so the page size is observable at all.
	const seeded = application.DefaultReleasePageSize + 5
	observed := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < seeded; i++ {
		id := "rel_page_" + itoaTest(i)
		r := newRelease(t, id, v.ID, e.ID, "2.0."+itoaTest(i), "stable", observed.Add(time.Duration(i)*time.Minute))
		m := domain.ReleaseProductMapping{
			ID: "rmap_" + id, ReleaseID: r.ID, ProductID: p.ID,
			Applicability: domain.Applicability{Channel: "stable"},
		}
		if err := releases.Insert(ctx, r, []domain.ReleaseProductMapping{m}); err != nil {
			t.Fatalf("Insert %s: %v", id, err)
		}
	}

	page, next, err := releases.ListForProduct(ctx, p.ID, application.ReleaseListOptions{})
	if err != nil {
		t.Fatalf("ListForProduct: %v", err)
	}
	if len(page) != application.DefaultReleasePageSize {
		t.Errorf("an absent limit returned %d releases, want application.DefaultReleasePageSize (%d)",
			len(page), application.DefaultReleasePageSize)
	}
	if next == "" {
		t.Error("a full page returned no cursor, so the remaining releases are unreachable")
	}
}

// itoaTest keeps the seeding loop above readable without pulling strconv into a file
// that otherwise has no use for it.
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestClearLatestFlagHandlesNullChannel covers the case the partial unique index makes
// subtle: a mapping with no channel is indexed under the empty string, so clearing it
// requires the same COALESCE the index uses.
func TestClearLatestFlagHandlesNullChannel(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)

	observed := time.Now().UTC()
	r1 := newRelease(t, "rel_1", v.ID, e.ID, "1.0", "", observed)
	m1 := domain.ReleaseProductMapping{ID: "rmap_1", ReleaseID: r1.ID, ProductID: p.ID, IsLatestObserved: true}
	if err := releases.Insert(ctx, r1, []domain.ReleaseProductMapping{m1}); err != nil {
		t.Fatalf("Insert r1: %v", err)
	}

	if err := releases.ClearLatestFlag(ctx, p.ID, ""); err != nil {
		t.Fatalf("ClearLatestFlag: %v", err)
	}

	r2 := newRelease(t, "rel_2", v.ID, e.ID, "1.1", "", observed.Add(time.Minute))
	m2 := domain.ReleaseProductMapping{ID: "rmap_2", ReleaseID: r2.ID, ProductID: p.ID, IsLatestObserved: true}
	if err := releases.Insert(ctx, r2, []domain.ReleaseProductMapping{m2}); err != nil {
		t.Fatalf("Insert r2 after clearing a NULL-channel flag: %v", err)
	}

	latest, err := releases.LatestForProduct(ctx, p.ID, "")
	if err != nil {
		t.Fatalf("LatestForProduct: %v", err)
	}
	if latest.ID != r2.ID {
		t.Errorf("latest = %q, want rel_2", latest.ID)
	}
}

func TestTouchVerifiedIsTheOnlyReleaseUpdate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)

	observed := time.Now().UTC().Add(-24 * time.Hour)
	r := newRelease(t, "rel_1", v.ID, e.ID, "7.24.2", "stable", observed)
	m := domain.ReleaseProductMapping{ID: "rmap_1", ReleaseID: r.ID, ProductID: p.ID}
	if err := releases.Insert(ctx, r, []domain.ReleaseProductMapping{m}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := releases.TouchVerified(ctx, r.ID, at); err != nil {
		t.Fatalf("TouchVerified: %v", err)
	}

	got, err := releases.GetByID(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !got.LastVerifiedAt.Equal(at) {
		t.Errorf("LastVerifiedAt = %v, want %v", got.LastVerifiedAt, at)
	}
	// PostgreSQL stores timestamps to microsecond precision, so compare with a
	// tolerance rather than for exact equality.
	if got.FirstObservedAt.Sub(observed).Abs() > time.Millisecond {
		t.Errorf("FirstObservedAt moved: %v, want %v", got.FirstObservedAt, observed)
	}
	if got.Version.Raw() != "7.24.2" {
		t.Errorf("TouchVerified changed the version to %q", got.Version.Raw())
	}

	if err := releases.TouchVerified(ctx, "rel_missing", at); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("TouchVerified on a missing release = %v, want domain.ErrNotFound", err)
	}
}

func TestRefreshProductSummaryAndSearch(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	vendors := NewVendorRepo(db)
	products := NewProductRepo(db)
	releases := NewReleaseRepo(db)
	summaries := NewSummaryRepo(db)

	v := newVendor("ven_cisco", "cisco")
	v.Name = "Cisco"
	if err := vendors.Upsert(ctx, v); err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	p := newProduct("prd_c9300", v.ID, "catalyst-9300")
	p.Name = "Catalyst 9300"
	if err := products.Upsert(ctx, p); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	alias, err := domain.NewProductAlias("pal_1", p.ID, "C9300", domain.AliasModelNumber)
	if err != nil {
		t.Fatalf("build alias: %v", err)
	}
	if err := products.ReplaceAliases(ctx, p.ID, []domain.ProductAlias{alias}); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	e := seedEvidence(t, db, v.ID)

	observed := time.Now().UTC()
	r := newRelease(t, "rel_1", v.ID, e.ID, "17.12.4", "stable", observed)
	r.ReleaseDate = mustMonthDate(t, 2026, time.February)
	recommended := true
	r.Recommended = &recommended
	m := domain.ReleaseProductMapping{
		ID:               "rmap_1",
		ReleaseID:        r.ID,
		ProductID:        p.ID,
		Applicability:    domain.Applicability{Channel: "stable"},
		IsLatestObserved: true,
	}
	if err := releases.Insert(ctx, r, []domain.ReleaseProductMapping{m}); err != nil {
		t.Fatalf("Insert release: %v", err)
	}

	if err := releases.RefreshProductSummary(ctx, p.ID); err != nil {
		t.Fatalf("RefreshProductSummary: %v", err)
	}
	// Refreshing twice must not duplicate or fail.
	if err := releases.RefreshProductSummary(ctx, p.ID); err != nil {
		t.Fatalf("second RefreshProductSummary: %v", err)
	}

	sum, err := summaries.Get(ctx, "catalyst-9300")
	if err != nil {
		t.Fatalf("summary Get: %v", err)
	}
	if sum.VendorSlug != "cisco" || sum.ProductName != "Catalyst 9300" {
		t.Errorf("summary identity wrong: %+v", sum)
	}
	if sum.LatestReleaseID != r.ID || sum.LatestRawVersion != "17.12.4" ||
		sum.LatestReleaseType != string(domain.ReleaseTypeFirmware) || sum.LatestChannel != "stable" {
		t.Errorf("summary latest release wrong: %+v", sum)
	}
	if sum.LatestReleaseDate.Precision() != domain.PrecisionMonthOnly {
		t.Errorf("summary latest date precision = %q, want month_only", sum.LatestReleaseDate.Precision())
	}
	if sum.RecommendedReleaseID != r.ID {
		t.Errorf("RecommendedReleaseID = %q, want rel_1", sum.RecommendedReleaseID)
	}
	if sum.ReleaseCount != 1 {
		t.Errorf("ReleaseCount = %d, want 1", sum.ReleaseCount)
	}
	if len(sum.Aliases) != 1 || sum.Aliases[0] != "C9300" {
		t.Errorf("Aliases = %v, want [C9300]", sum.Aliases)
	}
	if sum.LastVerifiedAt.IsZero() {
		t.Error("LastVerifiedAt was not aggregated from the releases")
	}

	if _, err := summaries.Get(ctx, "no-such-product"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("summary Get on a missing product = %v, want domain.ErrNotFound", err)
	}
	if err := releases.RefreshProductSummary(ctx, "prd_missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("RefreshProductSummary on a missing product = %v, want domain.ErrNotFound", err)
	}

	t.Run("exact term", func(t *testing.T) {
		got, err := summaries.Search(ctx, "catalyst 9300", 10)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(got) != 1 || got[0].ProductID != p.ID {
			t.Errorf("Search returned %d rows, want the catalyst", len(got))
		}
	})

	t.Run("vendor name", func(t *testing.T) {
		got, err := summaries.Search(ctx, "cisco", 10)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("Search on the vendor name returned %d rows, want 1", len(got))
		}
	})

	t.Run("typo falls back to trigram similarity", func(t *testing.T) {
		got, err := summaries.Search(ctx, "Catalist 9300", 10)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("Search for a misspelling returned %d rows; the trigram fallback did not fire", len(got))
		}
	})

	t.Run("empty query returns nothing", func(t *testing.T) {
		got, err := summaries.Search(ctx, "   ", 10)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("Search on an empty query returned %d rows, want 0", len(got))
		}
	})

	t.Run("query and limit are capped", func(t *testing.T) {
		long := make([]byte, 5000)
		for i := range long {
			long[i] = 'a'
		}
		if _, err := summaries.Search(ctx, string(long), 100000); err != nil {
			t.Errorf("Search with an oversized query and limit: %v", err)
		}
	})

	byVendor, _, err := summaries.ListByVendor(ctx, "cisco", 10, "")
	if err != nil {
		t.Fatalf("summary ListByVendor: %v", err)
	}
	if len(byVendor) != 1 {
		t.Errorf("ListByVendor = %d rows, want 1", len(byVendor))
	}
}

// TestRefreshProductSummaryOfficialSources is the revert detector for scope item 2: a
// product's officialSources must be the sources that have actually contributed a
// currently-mapped, non-withdrawn release, never every source registered against it.
// Three sources are set up -- one that contributed, a second that also contributed
// (proving the list is not hardcoded to one row), and a third that is registered for
// the same product and vendor but has never produced a release -- and only the first
// two may appear.
func TestRefreshProductSummaryOfficialSources(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	sources := NewSourceRepo(db)
	evidenceRepo := NewEvidenceRepo(db)
	releases := NewReleaseRepo(db)

	// Source A: registered, contributes a release.
	srcA := newSource("src_a", v.ID, "official-changelog")
	srcA.URL = "https://mikrotik.example/changelog"
	srcA.Official = true
	if err := sources.Upsert(ctx, srcA); err != nil {
		t.Fatalf("seed source A: %v", err)
	}
	evA := newEvidence("ev_a", srcA.ID)
	evA.SourceURL = srcA.URL
	if err := evidenceRepo.Insert(ctx, evA); err != nil {
		t.Fatalf("seed evidence A: %v", err)
	}

	// Source B: registered, also contributes, and is not "official" -- proving the
	// per-source official flag on each entry is read, not assumed true for everyone.
	srcB := newSource("src_b", v.ID, "community-mirror")
	srcB.URL = "https://mirror.example/mikrotik"
	srcB.Official = false
	srcB.QualityClass = domain.QualityTrustedCommunity
	if err := sources.Upsert(ctx, srcB); err != nil {
		t.Fatalf("seed source B: %v", err)
	}
	evB := newEvidence("ev_b", srcB.ID)
	evB.SourceURL = srcB.URL
	evB.Official = false
	if err := evidenceRepo.Insert(ctx, evB); err != nil {
		t.Fatalf("seed evidence B: %v", err)
	}

	// Source C: registered against the same vendor, never contributes anything.
	srcC := newSource("src_c", v.ID, "registered-but-silent")
	if err := sources.Upsert(ctx, srcC); err != nil {
		t.Fatalf("seed source C: %v", err)
	}

	// Source D: contributes, but only to a withdrawn release -- must not appear either.
	srcD := newSource("src_d", v.ID, "withdrawn-only")
	if err := sources.Upsert(ctx, srcD); err != nil {
		t.Fatalf("seed source D: %v", err)
	}
	evD := newEvidence("ev_d", srcD.ID)
	if err := evidenceRepo.Insert(ctx, evD); err != nil {
		t.Fatalf("seed evidence D: %v", err)
	}

	observed := time.Now().UTC().Add(-time.Hour)
	rA := newRelease(t, "rel_a", v.ID, evA.ID, "7.24.2", "stable", observed)
	rB := newRelease(t, "rel_b", v.ID, evB.ID, "7.24.2", "beta", observed.Add(time.Minute))
	rD := newRelease(t, "rel_d", v.ID, evD.ID, "7.20.0", "stable", observed.Add(-time.Hour))
	rD.Withdrawn = true
	rD.WithdrawnAt = observed
	rD.WithdrawnReason = "test fixture"

	for _, ins := range []struct {
		r domain.Release
		m domain.ReleaseProductMapping
	}{
		{rA, domain.ReleaseProductMapping{ID: "rmap_a", ReleaseID: rA.ID, ProductID: p.ID, Applicability: domain.Applicability{Channel: "stable"}, IsLatestObserved: true}},
		{rB, domain.ReleaseProductMapping{ID: "rmap_b", ReleaseID: rB.ID, ProductID: p.ID, Applicability: domain.Applicability{Channel: "beta"}, IsLatestObserved: true}},
		{rD, domain.ReleaseProductMapping{ID: "rmap_d", ReleaseID: rD.ID, ProductID: p.ID, Applicability: domain.Applicability{Channel: "stable"}}},
	} {
		if err := releases.Insert(ctx, ins.r, []domain.ReleaseProductMapping{ins.m}); err != nil {
			t.Fatalf("Insert %s: %v", ins.r.ID, err)
		}
	}

	if err := releases.RefreshProductSummary(ctx, p.ID); err != nil {
		t.Fatalf("RefreshProductSummary: %v", err)
	}

	sum, err := NewSummaryRepo(db).Get(ctx, p.Slug)
	if err != nil {
		t.Fatalf("summary Get: %v", err)
	}

	if len(sum.OfficialSources) != 2 {
		t.Fatalf("OfficialSources = %d entries, want 2 (got %+v)", len(sum.OfficialSources), sum.OfficialSources)
	}
	bySlug := map[string]application.ProductSourceRef{}
	for _, s := range sum.OfficialSources {
		bySlug[s.Slug] = s
	}
	for _, slug := range []string{"registered-but-silent", "withdrawn-only"} {
		if _, ok := bySlug[slug]; ok {
			t.Errorf("OfficialSources includes %q, which never contributed a current, non-withdrawn release", slug)
		}
	}
	a, ok := bySlug["official-changelog"]
	if !ok {
		t.Fatal("OfficialSources is missing the contributing official source")
	}
	if a.URL != srcA.URL || a.Kind != domain.PublicKindHTML || !a.Official {
		t.Errorf("official-changelog entry = %+v, want URL %q, kind %q, official true", a, srcA.URL, domain.PublicKindHTML)
	}
	b, ok := bySlug["community-mirror"]
	if !ok {
		t.Fatal("OfficialSources is missing the contributing community source")
	}
	if b.URL != srcB.URL || b.Official {
		t.Errorf("community-mirror entry = %+v, want URL %q, official false", b, srcB.URL)
	}
}

// TestRefreshProductSummaryConflictDetail is the revert detector for scope item 3: the
// conflict_* columns must carry the real open conflict's channel, versions, source
// count and detection time when has_source_conflict is true, and clear together --
// exactly as has_source_conflict itself already does -- once the conflict resolves.
func TestRefreshProductSummaryConflictDetail(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	releases := NewReleaseRepo(db)
	conflicts := NewConflictRepo(db)
	summaries := NewSummaryRepo(db)

	observed := time.Now().UTC().Add(-time.Hour)
	r := newRelease(t, "rel_1", v.ID, e.ID, "7.24.2", "stable", observed)
	m := domain.ReleaseProductMapping{
		ID: "rmap_1", ReleaseID: r.ID, ProductID: p.ID,
		Applicability: domain.Applicability{Channel: "stable"}, IsLatestObserved: true,
	}
	if err := releases.Insert(ctx, r, []domain.ReleaseProductMapping{m}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := releases.RefreshProductSummary(ctx, p.ID); err != nil {
		t.Fatalf("RefreshProductSummary before any conflict: %v", err)
	}
	before, err := summaries.Get(ctx, p.Slug)
	if err != nil {
		t.Fatalf("summary Get: %v", err)
	}
	if before.HasSourceConflict || before.Conflict != nil {
		t.Fatalf("a product with no conflict reports one: %+v", before)
	}

	detected := time.Now().UTC().Add(-time.Minute)
	c := domain.SourceConflict{
		ProductID:     p.ID,
		Channel:       "stable",
		AuthorityRank: domain.QualityOfficialManufacturer.Authority(),
		Versions:      []string{"7.24.2", "7.25.0"},
		SourceIDs:     []string{"src_x", "src_y"},
		DetectedAt:    detected,
	}
	if _, _, err := conflicts.UpsertOpenConflict(ctx, c); err != nil {
		t.Fatalf("UpsertOpenConflict: %v", err)
	}

	if err := releases.RefreshProductSummary(ctx, p.ID); err != nil {
		t.Fatalf("RefreshProductSummary with an open conflict: %v", err)
	}
	open, err := summaries.Get(ctx, p.Slug)
	if err != nil {
		t.Fatalf("summary Get: %v", err)
	}
	if !open.HasSourceConflict {
		t.Fatal("HasSourceConflict = false with an open conflict present")
	}
	if open.Conflict == nil {
		t.Fatal("Conflict is nil with HasSourceConflict true")
	}
	if open.Conflict.Channel != "stable" ||
		len(open.Conflict.Versions) != 2 ||
		open.Conflict.SourceCount != 2 ||
		open.Conflict.DetectedAt.IsZero() {
		t.Errorf("Conflict detail = %+v, want channel stable, 2 versions, source count 2, a real detected_at", open.Conflict)
	}

	// Resolving the conflict must clear has_source_conflict and the detail together,
	// on the next refresh -- neither is "preserved rather than reset" the way
	// advisory_count deliberately is.
	if _, err := conflicts.CloseOpenConflict(ctx, p.ID, "stable", "sources reconciled", "reviewer_1", time.Now().UTC()); err != nil {
		t.Fatalf("CloseOpenConflict: %v", err)
	}
	if err := releases.RefreshProductSummary(ctx, p.ID); err != nil {
		t.Fatalf("RefreshProductSummary after resolving: %v", err)
	}
	closed, err := summaries.Get(ctx, p.Slug)
	if err != nil {
		t.Fatalf("summary Get: %v", err)
	}
	if closed.HasSourceConflict {
		t.Error("HasSourceConflict is still true after the conflict was resolved")
	}
	if closed.Conflict != nil {
		t.Errorf("Conflict detail survived resolution: %+v, want nil", closed.Conflict)
	}
}

// TestProductSummariesConflictConsistencyCheckRejectsPartialRows proves the database
// itself, not just RefreshProductSummary's own discipline, refuses a product_summaries
// row where the four conflict_* columns disagree about whether a conflict is open.
func TestProductSummariesConflictConsistencyCheckRejectsPartialRows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, p := seedCatalog(t, db)
	if err := NewReleaseRepo(db).RefreshProductSummary(ctx, p.ID); err != nil {
		t.Fatalf("RefreshProductSummary: %v", err)
	}

	_, err := pool(db).Exec(ctx,
		`UPDATE product_summaries SET conflict_channel = 'stable' WHERE product_id = $1`, p.ID)
	if !errors.Is(translate(err), domain.ErrValidation) {
		t.Fatalf("setting conflict_channel alone = %v, want domain.ErrValidation from the CHECK constraint", err)
	}

	_, err = pool(db).Exec(ctx,
		`UPDATE product_summaries
            SET conflict_channel = 'stable', conflict_versions = ARRAY['1.0', '2.0'],
                conflict_source_count = 2, conflict_detected_at = now()
          WHERE product_id = $1`, p.ID)
	if err != nil {
		t.Errorf("setting all four conflict_* columns together was rejected: %v", err)
	}
}

func TestUnitOfWorkRollsBackAndNests(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	vendors := NewVendorRepo(db)

	sentinel := errors.New("use case failed")
	err := db.Within(ctx, func(ctx context.Context) error {
		if err := vendors.Upsert(ctx, newVendor("ven_rolled_back", "rolled-back")); err != nil {
			return err
		}
		// A nested unit of work joins the outer transaction rather than committing
		// independently.
		if err := db.Within(ctx, func(ctx context.Context) error {
			return vendors.Upsert(ctx, newVendor("ven_nested", "nested"))
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Within returned %v, want the use case's own error unchanged", err)
	}

	for _, slug := range []string{"rolled-back", "nested"} {
		if _, err := vendors.GetBySlug(ctx, slug); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s survived a rolled-back transaction (%v)", slug, err)
		}
	}

	// The committing path.
	if err := db.Within(ctx, func(ctx context.Context) error {
		return vendors.Upsert(ctx, newVendor("ven_committed", "committed"))
	}); err != nil {
		t.Fatalf("Within: %v", err)
	}
	if _, err := vendors.GetBySlug(ctx, "committed"); err != nil {
		t.Errorf("committed vendor is missing: %v", err)
	}
}

func TestReviewAndUsageRepos(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	reviews := NewReviewRepo(db)
	usage := NewUsageRepo(db)

	item := applicationReviewItem(v.ID, p.ID)
	if err := reviews.Create(ctx, item); err != nil {
		t.Fatalf("review Create: %v", err)
	}
	open, _, err := reviews.List(ctx, application.ReviewQueueFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(open) != 1 || open[0].Payload["candidates"] != "4" {
		t.Fatalf("List = %+v", open)
	}
	got, err := reviews.GetByID(ctx, open[0].ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID != open[0].ID || got.Kind != open[0].Kind {
		t.Errorf("GetByID = %+v, want the item List returned", got)
	}
	if _, err := reviews.GetByID(ctx, "rev_missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetByID on a missing item = %v, want domain.ErrNotFound", err)
	}

	at := time.Now().UTC()
	if err := reviews.Resolve(ctx, open[0].ID, "matched to catalyst-9300-24t", "maintainer", at); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := reviews.Resolve(ctx, open[0].ID, "again", "maintainer", at); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("re-resolving = %v, want domain.ErrNotFound", err)
	}
	open, _, err = reviews.List(ctx, application.ReviewQueueFilter{})
	if err != nil {
		t.Fatalf("List after resolve: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("open queue = %d items after resolving the only one", len(open))
	}

	// API keys and metering.
	if _, err := pool(db).Exec(ctx,
		`INSERT INTO api_consumers (id, name, plan, monthly_quota) VALUES ('con_1', 'Acme', 'professional', 100000)`); err != nil {
		t.Fatalf("seed consumer: %v", err)
	}
	keyID, err := usage.Create(ctx, "con_1", "sha256-of-secret", "fsk_live", "CI")
	if err != nil {
		t.Fatalf("api key Create: %v", err)
	}
	consumer, resolvedKeyID, err := usage.ResolveByHash(ctx, "sha256-of-secret")
	if err != nil {
		t.Fatalf("ResolveByHash: %v", err)
	}
	if consumer.ID != "con_1" || consumer.Plan != "professional" || resolvedKeyID != keyID {
		t.Errorf("resolved consumer = %+v, key = %q", consumer, resolvedKeyID)
	}
	if err := usage.TouchLastUsed(ctx, keyID, time.Now().UTC()); err != nil {
		t.Fatalf("TouchLastUsed: %v", err)
	}

	periodStart := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	rec := usageRecord("use_1", "idem-1", keyID, at)
	if err := usage.Record(ctx, rec); err != nil {
		t.Fatalf("usage Record: %v", err)
	}
	// The same request retried must not be counted twice.
	if err := usage.Record(ctx, rec); err != nil {
		t.Fatalf("usage Record retry: %v", err)
	}
	consumed, err := usage.QuotaConsumed(ctx, "con_1", periodStart)
	if err != nil {
		t.Fatalf("QuotaConsumed: %v", err)
	}
	if consumed != 5 {
		t.Errorf("QuotaConsumed = %d, want 5: a retried record must not double-count", consumed)
	}

	// A consumer with no usage has consumed nothing, which is not an error.
	if got, err := usage.QuotaConsumed(ctx, "con_none", periodStart); err != nil || got != 0 {
		t.Errorf("QuotaConsumed for an unmetered consumer = (%d, %v), want (0, nil)", got, err)
	}

	if err := usage.Revoke(ctx, keyID, "rotated", at); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, _, err := usage.ResolveByHash(ctx, "sha256-of-secret"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a revoked key still resolves (%v)", err)
	}
	if err := usage.Revoke(ctx, keyID, "again", at); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("re-revoking = %v, want domain.ErrNotFound", err)
	}
}

func TestEvidenceRoundTripAndAIProvenance(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, _ := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)
	evidence := NewEvidenceRepo(db)

	got, err := evidence.GetByID(ctx, e.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.SourceURL != e.SourceURL || got.Excerpt != e.Excerpt ||
		got.DiscoveryMethod != domain.DiscoveryDeterministic || got.Confidence != 0.95 {
		t.Errorf("round trip lost data: %+v", got)
	}

	// An AI-assisted record with no model is refused by the domain before it reaches
	// the constraint that would also refuse it.
	bad := newEvidence("ev_ai", e.SourceID)
	bad.DiscoveryMethod = domain.DiscoveryAIAssisted
	if err := evidence.Insert(ctx, bad); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("ai_assisted evidence with no model = %v, want domain.ErrValidation", err)
	}

	bad.AIModelID = "claude-sonnet-4"
	bad.AIPromptVersion = "v3"
	if err := evidence.Insert(ctx, bad); err != nil {
		t.Fatalf("ai_assisted evidence with provenance: %v", err)
	}

	if _, err := evidence.GetByID(ctx, "ev_missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing evidence = %v, want domain.ErrNotFound", err)
	}
}
