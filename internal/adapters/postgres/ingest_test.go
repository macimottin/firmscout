package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

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

	list, _, err := releases.ListForProduct(ctx, p.ID, 10, "")
	if err != nil {
		t.Fatalf("ListForProduct: %v", err)
	}
	if len(list) != 2 || list[0].ID != r2.ID {
		t.Errorf("ListForProduct = %d rows, newest first? got %+v", len(list), list)
	}
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
	open, err := reviews.ListOpen(ctx, 10)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(open) != 1 || open[0].Payload["candidates"] != "4" {
		t.Fatalf("ListOpen = %+v", open)
	}

	at := time.Now().UTC()
	if err := reviews.Resolve(ctx, open[0].ID, "matched to catalyst-9300-24t", "maintainer", at); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := reviews.Resolve(ctx, open[0].ID, "again", "maintainer", at); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("re-resolving = %v, want domain.ErrNotFound", err)
	}
	open, err = reviews.ListOpen(ctx, 10)
	if err != nil {
		t.Fatalf("ListOpen after resolve: %v", err)
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
