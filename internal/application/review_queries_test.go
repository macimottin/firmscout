package application_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/application/apptest"
	"github.com/macimottin/firmscout/internal/domain"
)

// ---------------------------------------------------------------------------
// Fakes A4 needs that apptest does not already provide.
//
// Per the ownership table, A4 does not touch apptest/fakes.go: VendorRepository and
// SummaryRepository have no existing fake there, so the minimal ones this file needs
// are declared here rather than added to a file another owner is editing.
// ---------------------------------------------------------------------------

type fakeVendors struct {
	items map[string]domain.Vendor
}

func newFakeVendors() *fakeVendors { return &fakeVendors{items: map[string]domain.Vendor{}} }

func (f *fakeVendors) add(v domain.Vendor) { f.items[v.ID] = v }

func (f *fakeVendors) GetByID(ctx context.Context, id string) (domain.Vendor, error) {
	v, ok := f.items[id]
	if !ok {
		return domain.Vendor{}, fmt.Errorf("vendor %s: %w", id, domain.ErrNotFound)
	}
	return v, nil
}

func (f *fakeVendors) GetBySlug(ctx context.Context, slug string) (domain.Vendor, error) {
	for _, v := range f.items {
		if v.Slug == slug {
			return v, nil
		}
	}
	return domain.Vendor{}, domain.ErrNotFound
}

func (f *fakeVendors) List(ctx context.Context, limit int, cursor string) ([]domain.Vendor, string, error) {
	var out []domain.Vendor
	for _, v := range f.items {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, "", nil
}

func (f *fakeVendors) Upsert(ctx context.Context, v domain.Vendor) error {
	f.items[v.ID] = v
	return nil
}

// fakeSummaries is a minimal SummaryRepository backing TestListReleasesAnnouncesItsWindow.
type fakeSummaries struct {
	bySlug map[string]application.ProductSummary
}

func newFakeSummaries() *fakeSummaries {
	return &fakeSummaries{bySlug: map[string]application.ProductSummary{}}
}

func (f *fakeSummaries) add(s application.ProductSummary) { f.bySlug[s.ProductSlug] = s }

func (f *fakeSummaries) Get(ctx context.Context, productSlug string) (application.ProductSummary, error) {
	s, ok := f.bySlug[productSlug]
	if !ok {
		return application.ProductSummary{}, domain.ErrNotFound
	}
	return s, nil
}

func (f *fakeSummaries) Search(ctx context.Context, query string, limit int) ([]application.ProductSummary, error) {
	return nil, nil
}

func (f *fakeSummaries) ListByVendor(ctx context.Context, vendorSlug string, limit int, cursor string) ([]application.ProductSummary, string, error) {
	return nil, "", nil
}

// ---------------------------------------------------------------------------
// Test fixture
// ---------------------------------------------------------------------------

const reviewTestNow = "2026-09-05T12:00:00Z"

type reviewFixture struct {
	clock      *apptest.Clock
	vendors    *fakeVendors
	products   *apptest.Products
	candidates *apptest.Candidates
	evidence   *apptest.EvidenceStore
	sources    *apptest.Sources
	reviews    *apptest.Reviews
	conflicts  *apptest.Conflicts
	audit      *apptest.Audit

	vendor  domain.Vendor
	product domain.Product
	source  domain.Source
}

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	now, err := time.Parse(time.RFC3339, reviewTestNow)
	if err != nil {
		t.Fatalf("parse test time: %v", err)
	}

	f := &reviewFixture{
		clock:      apptest.NewClock(now),
		vendors:    newFakeVendors(),
		products:   apptest.NewProducts(),
		candidates: apptest.NewCandidates(),
		evidence:   apptest.NewEvidenceStore(),
		sources:    apptest.NewSources(),
		reviews:    &apptest.Reviews{},
		conflicts:  apptest.NewConflicts(),
		audit:      apptest.NewAudit(),
	}

	f.vendor = domain.Vendor{ID: "ven_mikrotik", Slug: "mikrotik", Name: "MikroTik"}
	f.vendors.add(f.vendor)

	f.product = domain.Product{
		ID:                 "prd_routeros",
		VendorID:           f.vendor.ID,
		Slug:               "routeros",
		Name:               "RouterOS",
		DefaultReleaseType: domain.ReleaseTypeFirmware,
		LifecycleStatus:    domain.LifecycleActive,
	}
	f.products.Add(f.product)

	f.source = domain.Source{
		ID:                "src_mikrotik_changelogs",
		VendorID:          f.vendor.ID,
		ProductID:         f.product.ID,
		Slug:              "changelogs",
		SourceType:        domain.SourceTypeHTMLPage,
		URL:               "https://mikrotik.com/download/changelogs",
		Official:          true,
		QualityClass:      domain.QualityOfficialManufacturer,
		Enabled:           true,
		Health:            domain.SourceActive,
		TermsReviewStatus: domain.TermsApproved,
	}
	f.sources.Add(f.source)

	return f
}

func (f *reviewFixture) deps() application.ReviewQueryDeps {
	return application.ReviewQueryDeps{
		Reviews:    f.reviews,
		Candidates: f.candidates,
		Evidence:   f.evidence,
		Sources:    f.sources,
		Products:   f.products,
		Vendors:    f.vendors,
		Conflicts:  f.conflicts,
		Audit:      f.audit,
		Clock:      f.clock,
	}
}

// mustVersion is declared once, in pipeline_test.go, and reused here.

// ---------------------------------------------------------------------------
// ListReviewQueue
// ---------------------------------------------------------------------------

func TestListReviewQueueResolvesSlugsAndAge(t *testing.T) {
	f := newReviewFixture(t)
	createdAt := f.clock.Now().Add(-2 * time.Hour)
	f.reviews.Add(application.ReviewItem{
		ID:            "rvw_1",
		Kind:          "candidate_low_confidence",
		SubjectType:   "candidate_release",
		SubjectID:     "cand_1",
		VendorID:      f.vendor.ID,
		ProductID:     f.product.ID,
		Title:         "Low confidence candidate",
		PriorityScore: 150,
		SLAClass:      string(domain.SLAStandard),
		State:         application.ReviewStateOpen,
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
	})

	uc := application.NewListReviewQueue(f.deps())
	page, err := uc.Execute(context.Background(), application.ReviewQueueQuery{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(page.Entries))
	}
	entry := page.Entries[0]
	if entry.VendorSlug != f.vendor.Slug || entry.VendorName != f.vendor.Name {
		t.Errorf("vendor not resolved: got slug=%q name=%q", entry.VendorSlug, entry.VendorName)
	}
	if entry.ProductSlug != f.product.Slug || entry.ProductName != f.product.Name {
		t.Errorf("product not resolved: got slug=%q name=%q", entry.ProductSlug, entry.ProductName)
	}
	if entry.Age != 2*time.Hour {
		t.Errorf("age = %v, want 2h", entry.Age)
	}
}

func TestListReviewQueueRejectsUnknownFilterValues(t *testing.T) {
	f := newReviewFixture(t)
	uc := application.NewListReviewQueue(f.deps())

	cases := []application.ReviewQueueQuery{
		{States: []string{"no_such_state"}},
		{Kinds: []string{"no_such_kind"}},
		{SLAClasses: []string{"no_such_sla"}},
	}
	for _, q := range cases {
		_, err := uc.Execute(context.Background(), q)
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("query %+v: err = %v, want domain.ErrValidation", q, err)
		}
	}
}

// TestListReviewQueueEnforcesTheDocumentedPageBounds is the revert detector for the
// same class of drift that was found in ListReleases, one file over.
//
// The clamp read "if limit <= 0 || limit > MaxReviewPageSize { limit = DefaultReviewPageSize }",
// so a caller asking for 500 received 50 rather than the documented maximum of 200 --
// a smaller page than the maximum they were reaching for, and a number api.md §11 does
// not promise for that input. Unlike the release-history case the two constants
// themselves were right, so only the above-the-maximum direction was wrong; it was
// invisible through HTTP because parseLimit clamps to the maximum before the use case
// sees the value, which is exactly what lets a handler become the de-facto contract.
//
// The page size is observed rather than the clamp, because the clamp is not exported:
// the fixture seeds more items than the maximum and reads back how many came out.
func TestListReviewQueueEnforcesTheDocumentedPageBounds(t *testing.T) {
	tests := []struct {
		name string
		ask  int
		want int
	}{
		{"absent takes the documented default", 0, application.DefaultReviewPageSize},
		{"negative takes the documented default", -10, application.DefaultReviewPageSize},
		{"a request inside the bounds is honoured", 37, 37},
		{"the maximum itself is honoured", application.MaxReviewPageSize, application.MaxReviewPageSize},
		{"above the maximum clamps to the maximum", 500, application.MaxReviewPageSize},
	}

	seeded := application.MaxReviewPageSize + 25
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t)
			for i := 0; i < seeded; i++ {
				f.reviews.Add(application.ReviewItem{
					ID:            fmt.Sprintf("rvw_%03d", i),
					Kind:          "candidate_low_confidence",
					SubjectType:   "candidate_release",
					SubjectID:     fmt.Sprintf("cand_%03d", i),
					State:         application.ReviewStateOpen,
					PriorityScore: 100,
					CreatedAt:     f.clock.Now(),
					UpdatedAt:     f.clock.Now(),
				})
			}
			uc := application.NewListReviewQueue(f.deps())

			page, err := uc.Execute(context.Background(), application.ReviewQueueQuery{Limit: tc.ask})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(page.Entries) != tc.want {
				t.Errorf("Limit(%d) produced a page of %d, want %d (api.md §11: default %d, max %d)",
					tc.ask, len(page.Entries), tc.want,
					application.DefaultReviewPageSize, application.MaxReviewPageSize)
			}
		})
	}
}

// TestReviewQueuePageBoundsMatchTheDocumentation pins the constants themselves: a
// correct clamp can point at wrong numbers, and the numbers are the part api.md states.
func TestReviewQueuePageBoundsMatchTheDocumentation(t *testing.T) {
	if application.DefaultReviewPageSize != 50 {
		t.Errorf("DefaultReviewPageSize = %d, want 50; api.md §11 documents a default of 50",
			application.DefaultReviewPageSize)
	}
	if application.MaxReviewPageSize != 200 {
		t.Errorf("MaxReviewPageSize = %d, want 200; api.md §11 documents a maximum of 200",
			application.MaxReviewPageSize)
	}
}

// TestListReviewQueueFiltersByPriorityBand exercises the boundary where a page is
// exactly full, and that a filter on kind actually narrows the result rather than being
// ignored.
func TestListReviewQueueFiltersByPriorityBand(t *testing.T) {
	f := newReviewFixture(t)
	for i := 0; i < application.DefaultReviewPageSize; i++ {
		f.reviews.Add(application.ReviewItem{
			ID:            fmt.Sprintf("rvw_%d", i),
			Kind:          "candidate_low_confidence",
			SubjectType:   "candidate_release",
			SubjectID:     fmt.Sprintf("cand_%d", i),
			State:         application.ReviewStateOpen,
			PriorityScore: 100,
			CreatedAt:     f.clock.Now(),
			UpdatedAt:     f.clock.Now(),
		})
	}
	f.reviews.Add(application.ReviewItem{
		ID:            "rvw_conflict",
		Kind:          "multi_source_conflict",
		SubjectType:   "candidate_release",
		SubjectID:     "cand_conflict",
		State:         application.ReviewStateOpen,
		PriorityScore: 250,
		CreatedAt:     f.clock.Now(),
		UpdatedAt:     f.clock.Now(),
	})

	uc := application.NewListReviewQueue(f.deps())

	// A page exactly the default size, unfiltered, returns exactly that many items --
	// the boundary where a page is exactly full.
	page, err := uc.Execute(context.Background(), application.ReviewQueueQuery{Limit: application.DefaultReviewPageSize})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(page.Entries) != application.DefaultReviewPageSize {
		t.Fatalf("want exactly %d entries, got %d", application.DefaultReviewPageSize, len(page.Entries))
	}

	filtered, err := uc.Execute(context.Background(), application.ReviewQueueQuery{Kinds: []string{"multi_source_conflict"}})
	if err != nil {
		t.Fatalf("Execute filtered: %v", err)
	}
	if len(filtered.Entries) != 1 || filtered.Entries[0].Item.ID != "rvw_conflict" {
		t.Fatalf("kind filter did not narrow the queue: got %+v", filtered.Entries)
	}
}

func TestListReviewQueueUnknownVendorSlugYieldsEmptyPage(t *testing.T) {
	f := newReviewFixture(t)
	f.reviews.Add(application.ReviewItem{
		ID:          "rvw_1",
		Kind:        "candidate_low_confidence",
		SubjectType: "candidate_release",
		SubjectID:   "cand_1",
		State:       application.ReviewStateOpen,
		CreatedAt:   f.clock.Now(),
	})

	uc := application.NewListReviewQueue(f.deps())
	page, err := uc.Execute(context.Background(), application.ReviewQueueQuery{VendorSlug: "no-such-vendor"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Fatalf("want an empty page for an unknown vendor slug, got %d entries", len(page.Entries))
	}
}

// ---------------------------------------------------------------------------
// GetReviewItem
// ---------------------------------------------------------------------------

func TestGetReviewItemNotFound(t *testing.T) {
	f := newReviewFixture(t)
	uc := application.NewGetReviewItem(f.deps())
	_, err := uc.Execute(context.Background(), "rvw_missing")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want domain.ErrNotFound", err)
	}
}

func TestGetReviewItemSurvivesMissingParts(t *testing.T) {
	f := newReviewFixture(t)
	f.reviews.Add(application.ReviewItem{
		ID:          "rvw_orphan",
		Kind:        "multi_source_conflict",
		SubjectType: "candidate_release",
		SubjectID:   "cand_gone",
		VendorID:    f.vendor.ID,
		ProductID:   f.product.ID,
		Payload:     map[string]string{application.PayloadKeyConflictID: "cnf_gone"},
		State:       application.ReviewStateOpen,
		CreatedAt:   f.clock.Now(),
	})
	// Deliberately: no candidate, no conflict recorded for cand_gone / cnf_gone.

	uc := application.NewGetReviewItem(f.deps())
	detail, err := uc.Execute(context.Background(), "rvw_orphan")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if detail.CandidateFound {
		t.Error("CandidateFound = true, want false")
	}
	if detail.EvidenceFound {
		t.Error("EvidenceFound = true, want false")
	}
	if detail.ConflictFound {
		t.Error("ConflictFound = true, want false")
	}
	if detail.Entry.Item.ID != "rvw_orphan" {
		t.Errorf("Entry.Item.ID = %q, want rvw_orphan", detail.Entry.Item.ID)
	}
}

func TestGetReviewItemReturnsFailingGates(t *testing.T) {
	f := newReviewFixture(t)

	cand := domain.CandidateRelease{
		ID:         "cand_1",
		SourceID:   f.source.ID,
		ProductID:  f.product.ID,
		Version:    mustVersion(t, "7.24.3"),
		Confidence: 0.6,
		DedupeKey:  "dedupe-1",
		State:      domain.CandidateHumanReviewRequired,
		EvidenceID: "evd_1",
	}
	if _, _, err := f.candidates.UpsertByDedupeKey(context.Background(), cand); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}

	gates := []domain.GateResult{
		{Gate: domain.GateProductIdentity, Order: 1, Outcome: domain.GatePassed, Detail: "resolved"},
		{
			Gate:    domain.GateMultiSourceAgree,
			Order:   domain.GateIndex(domain.GateMultiSourceAgree),
			Outcome: domain.GateReviewRequired,
			Detail:  "other active sources report a different version: 7.24.2",
		},
	}
	if err := f.candidates.RecordValidation(context.Background(), cand.ID, gates); err != nil {
		t.Fatalf("record validation: %v", err)
	}

	ev := domain.Evidence{
		ID:              "evd_1",
		SourceID:        f.source.ID,
		SourceURL:       f.source.URL,
		Official:        true,
		Excerpt:         "RouterOS 7.24.3 has been released.",
		RawValue:        "7.24.3",
		DiscoveryMethod: domain.DiscoveryDeterministic,
	}
	if err := f.evidence.Insert(context.Background(), ev); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	f.reviews.Add(application.ReviewItem{
		ID:          "rvw_1",
		Kind:        "multi_source_conflict",
		SubjectType: "candidate_release",
		SubjectID:   cand.ID,
		VendorID:    f.vendor.ID,
		ProductID:   f.product.ID,
		Payload:     map[string]string{application.PayloadKeyFailingGate: string(domain.GateMultiSourceAgree)},
		State:       application.ReviewStateOpen,
		CreatedAt:   f.clock.Now(),
	})

	uc := application.NewGetReviewItem(f.deps())
	detail, err := uc.Execute(context.Background(), "rvw_1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !detail.CandidateFound {
		t.Fatal("CandidateFound = false, want true")
	}
	if !detail.EvidenceFound || detail.Evidence.Excerpt != ev.Excerpt {
		t.Errorf("evidence not carried through: found=%v excerpt=%q", detail.EvidenceFound, detail.Evidence.Excerpt)
	}
	if len(ev.Excerpt) > domain.MaxExcerptLength {
		t.Fatalf("test fixture excerpt exceeds MaxExcerptLength, fix the test")
	}
	if !detail.SourceFound || detail.Source.ID != f.source.ID {
		t.Errorf("source not carried through: found=%v id=%q", detail.SourceFound, detail.Source.ID)
	}

	found := false
	for _, g := range detail.Gates {
		if g.Gate == domain.GateMultiSourceAgree && g.Outcome == domain.GateReviewRequired {
			found = true
		}
	}
	if !found {
		t.Errorf("Gates does not contain the failing multi_source_agreement gate: %+v", detail.Gates)
	}
}

func TestGetReviewItemLoadsConflictAndAudit(t *testing.T) {
	f := newReviewFixture(t)

	now := f.clock.Now()
	obsA := domain.SourceObservation{
		SourceID: f.source.ID, ProductID: f.product.ID,
		RawVersion: "7.24.3", NormalizedVersion: "7.24.3",
		ObservedAt: now, FirstObservedAt: now,
		QualityClass: domain.QualityOfficialManufacturer, Official: true, Eligible: true,
	}
	obsB := domain.SourceObservation{
		SourceID: "src_reseller", ProductID: f.product.ID,
		RawVersion: "7.24.2", NormalizedVersion: "7.24.2",
		ObservedAt: now, FirstObservedAt: now,
		QualityClass: domain.QualityOfficialManufacturer, Official: true, Eligible: true,
	}
	f.conflicts.SetObservation(obsA)
	f.conflicts.SetObservation(obsB)

	conflict := domain.SourceConflict{
		ID: "cnf_1", ProductID: f.product.ID, State: domain.ConflictOpen,
		AuthorityRank: domain.QualityOfficialManufacturer.Authority(),
		Versions:      []string{"7.24.2", "7.24.3"},
		SourceIDs:     []string{f.source.ID, "src_reseller"},
		DetectedAt:    now, LastSeenAt: now,
	}
	if _, _, err := f.conflicts.UpsertOpenConflict(context.Background(), conflict); err != nil {
		t.Fatalf("seed conflict: %v", err)
	}

	if err := f.audit.Record(context.Background(), application.AuditEvent{
		ID: "aud_1", ActorType: application.ActorTypeHuman, ActorID: "reviewer@example.com",
		Action: application.AuditActionConflictResolved, SubjectType: "review_item", SubjectID: "rvw_1",
		OccurredAt: now,
	}); err != nil {
		t.Fatalf("seed audit: %v", err)
	}

	f.reviews.Add(application.ReviewItem{
		ID:          "rvw_1",
		Kind:        "multi_source_conflict",
		SubjectType: "candidate_release",
		SubjectID:   "cand_missing",
		ProductID:   f.product.ID,
		Payload:     map[string]string{application.PayloadKeyConflictID: "cnf_1"},
		State:       application.ReviewStateOpen,
		CreatedAt:   now,
	})

	uc := application.NewGetReviewItem(f.deps())
	detail, err := uc.Execute(context.Background(), "rvw_1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !detail.ConflictFound || detail.Conflict.ID != "cnf_1" {
		t.Fatalf("conflict not loaded: found=%v id=%q", detail.ConflictFound, detail.Conflict.ID)
	}
	if len(detail.ConflictObservations) != 2 {
		t.Fatalf("want 2 conflict observations, got %d", len(detail.ConflictObservations))
	}
	if len(detail.Audit) != 1 || detail.Audit[0].ID != "aud_1" {
		t.Fatalf("audit trail not loaded: %+v", detail.Audit)
	}
}

// ---------------------------------------------------------------------------
// ListReleases windowing (application/queries.go, edited by A4)
// ---------------------------------------------------------------------------

func TestListReleasesAnnouncesItsWindow(t *testing.T) {
	f := newReviewFixture(t)
	summaries := newFakeSummaries()
	summaries.add(application.ProductSummary{ProductID: f.product.ID, ProductSlug: f.product.Slug})
	releases := apptest.NewReleases()

	deps := application.QueryDeps{
		Summaries: summaries,
		Vendors:   f.vendors,
		Products:  f.products,
		Releases:  releases,
		Events:    &apptest.Events{},
		Clock:     f.clock,
	}
	uc := application.NewListReleases(deps)

	windowed, err := uc.Execute(context.Background(), application.ReleaseHistoryQuery{Slug: f.product.Slug, Plan: application.PlanFree})
	if err != nil {
		t.Fatalf("Execute (free): %v", err)
	}
	if !windowed.Windowed {
		t.Error("free plan: Windowed = false, want true")
	}
	wantSince := application.HistoryWindow(application.PlanFree, f.clock.Now())
	if !windowed.Since.Equal(wantSince) {
		t.Errorf("free plan: Since = %v, want %v", windowed.Since, wantSince)
	}

	unwindowed, err := uc.Execute(context.Background(), application.ReleaseHistoryQuery{Slug: f.product.Slug, Plan: application.PlanEnterprise})
	if err != nil {
		t.Fatalf("Execute (enterprise): %v", err)
	}
	if unwindowed.Windowed {
		t.Error("enterprise plan: Windowed = true, want false")
	}
	if !unwindowed.Since.IsZero() {
		t.Errorf("enterprise plan: Since = %v, want zero", unwindowed.Since)
	}
}

// TestQueueAgeIsNeverNegative. created_at is written from one clock and read against
// another, and those are the same clock only until the queue is served from a replica,
// the reader runs on another host, or the host's clock is corrected backwards. A
// negative age is never a true statement about how long somebody has been waiting, and
// it reads as a bug in whatever renders it rather than as the skew it is.
func TestQueueAgeIsNeverNegative(t *testing.T) {
	f := newReviewFixture(t)
	// An item whose created_at is three minutes in the future, which is exactly what a
	// backwards clock correction between the write and the read produces.
	createdAt := f.clock.Now().Add(3 * time.Minute)
	f.reviews.Add(application.ReviewItem{
		ID:            "rvw_future",
		Kind:          "candidate_low_confidence",
		SubjectType:   "candidate_release",
		SubjectID:     "cand_1",
		VendorID:      f.vendor.ID,
		ProductID:     f.product.ID,
		Title:         "Written by a clock that was later corrected",
		PriorityScore: 150,
		SLAClass:      string(domain.SLAStandard),
		State:         application.ReviewStateOpen,
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
	})

	page, err := application.NewListReviewQueue(f.deps()).Execute(
		context.Background(), application.ReviewQueueQuery{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(page.Entries))
	}
	if age := page.Entries[0].Age; age != 0 {
		t.Errorf("age = %v, want 0; a queue must never report that an item has waited a negative time", age)
	}
}
