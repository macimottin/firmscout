package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

func TestSourceRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	sources := NewSourceRepo(db)

	s := newSource("src_changelog", v.ID, "changelog")
	s.ProductID = p.ID
	s.ExpectedContentType = "text/html"
	s.MinFrequencySeconds = 900
	if err := sources.Upsert(ctx, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := sources.GetBySlug(ctx, v.ID, "changelog")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.URL != s.URL || got.ProductID != p.ID || got.CollectorID != s.CollectorID ||
		got.ExpectedContentType != "text/html" || got.MinFrequencySeconds != 900 ||
		got.Health != domain.SourceActive || got.QualityClass != domain.QualityOfficialManufacturer {
		t.Errorf("round trip lost data: %+v", got)
	}

	// The normalisation config is what stops a 409 KB page reporting a change on every
	// check, so its JSONB round trip is worth asserting field by field.
	if got.Normalize.SectionSelector != "#changelog" {
		t.Errorf("SectionSelector = %q, want #changelog", got.Normalize.SectionSelector)
	}
	if len(got.Normalize.Strip) != 2 || got.Normalize.Strip[0] != "script" || got.Normalize.Strip[1] != ".ads" {
		t.Errorf("Strip = %v, want [script .ads]", got.Normalize.Strip)
	}

	if _, err := sources.GetBySlug(ctx, v.ID, "absent"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("missing source = %v, want domain.ErrNotFound", err)
	}
}

func TestUpdateCheckStatePreservesConfiguration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, _ := seedCatalog(t, db)
	sources := NewSourceRepo(db)

	s := newSource("src_changelog", v.ID, "changelog")
	if err := sources.Upsert(ctx, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	s.ETag = `W/"abc123"`
	s.NormalizedContentHash = "sha256:deadbeef"
	s.LastCheckedAt = now
	s.LastSuccessAt = now
	s.LastChangedAt = now
	s.NextCheckAt = now.Add(6 * time.Hour)
	s.ConsecutiveUnchanged = 3
	s.Health = domain.SourceDegraded
	// Deliberately corrupt fields UpdateCheckState must not write.
	s.TermsReviewStatus = domain.TermsProhibited
	s.URL = "https://attacker.example/"
	s.Enabled = false

	if err := sources.UpdateCheckState(ctx, s); err != nil {
		t.Fatalf("UpdateCheckState: %v", err)
	}

	got, err := sources.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ETag != s.ETag || got.NormalizedContentHash != s.NormalizedContentHash ||
		got.ConsecutiveUnchanged != 3 || got.Health != domain.SourceDegraded {
		t.Errorf("check state was not written: %+v", got)
	}
	if got.TermsReviewStatus != domain.TermsApproved {
		t.Errorf("terms status = %q; UpdateCheckState must not rewrite compliance", got.TermsReviewStatus)
	}
	if got.URL == "https://attacker.example/" || !got.Enabled {
		t.Error("UpdateCheckState rewrote registration fields it does not own")
	}
	if !got.NextCheckAt.Equal(s.NextCheckAt) {
		t.Errorf("NextCheckAt = %v, want %v", got.NextCheckAt, s.NextCheckAt)
	}

	if err := sources.UpdateCheckState(ctx, domain.Source{ID: "src_missing"}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("UpdateCheckState on a missing source = %v, want domain.ErrNotFound", err)
	}
}

// TestListDispatchableAppliesTheCompliancePredicate is the most important test in this
// package. Each excluded source differs from a dispatchable one in exactly one field,
// so a failure names the rule that broke.
func TestListDispatchableAppliesTheCompliancePredicate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, _ := seedCatalog(t, db)
	sources := NewSourceRepo(db)

	now := time.Now().UTC()

	dispatchable := newSource("src_ok", v.ID, "ok")

	robotsDisallowed := newSource("src_robots", v.ID, "robots")
	robotsDisallowed.RobotsPolicyStatus = domain.RobotsDisallowed

	robotsUnknown := newSource("src_robots_unknown", v.ID, "robots-unknown")
	robotsUnknown.RobotsPolicyStatus = domain.RobotsUnknown

	termsPending := newSource("src_terms", v.ID, "terms")
	termsPending.TermsReviewStatus = domain.TermsPending

	termsProhibited := newSource("src_terms_prohibited", v.ID, "terms-prohibited")
	termsProhibited.TermsReviewStatus = domain.TermsProhibited

	disabled := newSource("src_disabled", v.ID, "disabled")
	disabled.Enabled = false

	notDue := newSource("src_not_due", v.ID, "not-due")
	notDue.NextCheckAt = now.Add(time.Hour)

	broken := newSource("src_broken", v.ID, "broken")
	broken.Health = domain.SourceBroken

	rateLimited := newSource("src_rate_limited", v.ID, "rate-limited")
	rateLimited.RetryAfterUntil = now.Add(30 * time.Minute)

	degraded := newSource("src_degraded", v.ID, "degraded")
	degraded.Health = domain.SourceDegraded

	retryExpired := newSource("src_retry_expired", v.ID, "retry-expired")
	retryExpired.RetryAfterUntil = now.Add(-time.Minute)

	all := []domain.Source{
		dispatchable, robotsDisallowed, robotsUnknown, termsPending, termsProhibited,
		disabled, notDue, broken, rateLimited, degraded, retryExpired,
	}
	for _, s := range all {
		if err := sources.Upsert(ctx, s); err != nil {
			t.Fatalf("seed %s: %v", s.Slug, err)
		}
	}
	// retry_after_until is check state, not registration, so Upsert does not write it.
	// The two sources that need one get it through the path that owns it.
	for _, s := range []domain.Source{rateLimited, retryExpired} {
		if err := sources.UpdateCheckState(ctx, s); err != nil {
			t.Fatalf("set retry window on %s: %v", s.Slug, err)
		}
	}

	got, err := sources.ListDispatchable(ctx, now, 100)
	if err != nil {
		t.Fatalf("ListDispatchable: %v", err)
	}
	returned := map[string]bool{}
	for _, s := range got {
		returned[s.ID] = true
	}

	wantIncluded := []string{"src_ok", "src_degraded", "src_retry_expired"}
	for _, id := range wantIncluded {
		if !returned[id] {
			t.Errorf("ListDispatchable omitted %s, which is dispatchable", id)
		}
	}
	wantExcluded := map[string]string{
		"src_robots":           "robots.txt disallows the path",
		"src_robots_unknown":   "robots status is unknown",
		"src_terms":            "terms review is still pending",
		"src_terms_prohibited": "terms review prohibits collection",
		"src_disabled":         "the source is disabled",
		"src_not_due":          "the source is not due yet",
		"src_broken":           "health is broken",
		"src_rate_limited":     "a Retry-After window is still open",
	}
	for id, why := range wantExcluded {
		if returned[id] {
			t.Errorf("ListDispatchable returned %s even though %s", id, why)
		}
	}

	// The Go predicate and the SQL predicate must agree, source by source. This is the
	// check that catches the two drifting apart.
	for _, s := range all {
		stored, err := sources.GetByID(ctx, s.ID)
		if err != nil {
			t.Fatalf("GetByID %s: %v", s.ID, err)
		}
		if want := stored.Dispatchable(now); want != returned[s.ID] {
			t.Errorf("%s: SQL says dispatchable=%v, domain.Source.Dispatchable says %v",
				s.ID, returned[s.ID], want)
		}
	}

	// Ordering is by next_check_at, so the most overdue source is checked first.
	for i := 1; i < len(got); i++ {
		if got[i].NextCheckAt.Before(got[i-1].NextCheckAt) {
			t.Fatalf("results are not ordered by next_check_at: %v before %v",
				got[i-1].NextCheckAt, got[i].NextCheckAt)
		}
	}
}

func TestProductsForSourceCoversDirectAndFamilyScopes(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	vendors := NewVendorRepo(db)
	products := NewProductRepo(db)
	sources := NewSourceRepo(db)

	v := newVendor("ven_mikrotik", "mikrotik")
	if err := vendors.Upsert(ctx, v); err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	fam := domain.ProductFamily{ID: "fam_crs", VendorID: v.ID, Slug: "crs", Name: "CRS"}
	if err := products.UpsertFamily(ctx, fam); err != nil {
		t.Fatalf("seed family: %v", err)
	}
	for _, slug := range []string{"crs310", "crs326"} {
		p := newProduct("prd_"+slug, v.ID, slug)
		p.ProductFamilyID = fam.ID
		if err := products.Upsert(ctx, p); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
	if err := products.Upsert(ctx, newProduct("prd_hex", v.ID, "hex")); err != nil {
		t.Fatalf("seed hex: %v", err)
	}

	// A family-scoped source covers every product in the family.
	familySource := newSource("src_family", v.ID, "family")
	familySource.ProductFamilyID = fam.ID
	if err := sources.Upsert(ctx, familySource); err != nil {
		t.Fatalf("seed family source: %v", err)
	}

	got, err := sources.ProductsForSource(ctx, familySource.ID)
	if err != nil {
		t.Fatalf("ProductsForSource: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("family source covers %d products, want 2", len(got))
	}

	// An explicit source_products link adds a product outside the family: one
	// catalogue page, several devices.
	if _, err := pool(db).Exec(ctx,
		`INSERT INTO source_products (source_id, product_id) VALUES ($1, 'prd_hex')`,
		familySource.ID); err != nil {
		t.Fatalf("link source_products: %v", err)
	}
	got, err = sources.ProductsForSource(ctx, familySource.ID)
	if err != nil {
		t.Fatalf("ProductsForSource: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("after the explicit link the source covers %d products, want 3", len(got))
	}
}

func TestRecordCheckStoresHistory(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, _ := seedCatalog(t, db)
	sources := NewSourceRepo(db)

	s := newSource("src_changelog", v.ID, "changelog")
	if err := sources.Upsert(ctx, s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	started := time.Now().UTC().Add(-2 * time.Second)
	check := application.SourceCheck{
		SourceID:              s.ID,
		StartedAt:             started,
		FinishedAt:            started.Add(1500 * time.Millisecond),
		Outcome:               domain.OutcomeUnchanged,
		ChangeSignal:          domain.SignalETag,
		HTTPStatus:            304,
		ResponseETag:          `W/"abc"`,
		NormalizedContentHash: "sha256:cafe",
		BytesFetched:          0,
		TraceID:               "trace-1",
		RequestID:             "req-1",
	}
	if err := sources.RecordCheck(ctx, check); err != nil {
		t.Fatalf("RecordCheck: %v", err)
	}

	var (
		outcome  string
		signal   string
		duration int
	)
	if err := pool(db).QueryRow(ctx,
		`SELECT outcome, change_signal, duration_ms FROM source_checks WHERE source_id = $1`,
		s.ID).Scan(&outcome, &signal, &duration); err != nil {
		t.Fatalf("read back check: %v", err)
	}
	if outcome != string(domain.OutcomeUnchanged) || signal != string(domain.SignalETag) {
		t.Errorf("stored outcome/signal = %q/%q", outcome, signal)
	}
	if duration != 1500 {
		t.Errorf("duration_ms = %d, want 1500", duration)
	}
}
