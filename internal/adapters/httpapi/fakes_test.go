package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/application/apptest"
	"github.com/macimottin/firmscout/internal/domain"
)

// Hand-written in-memory fakes. There is no mocking framework and no database: these
// tests are about the HTTP contract -- status codes, media types, headers and the exact
// bytes of the JSON -- and every one of them runs in microseconds against httptest.
//
// Each fake is mutex-guarded because the after-response worker touches some of them
// from a second goroutine. The guards are what make that safe; they are also what a
// -race build would check, and this machine has no C compiler, so no run of this suite
// has been under the race detector. Do not read the guards as evidence that one was.

// ---------------------------------------------------------------------------
// SummaryRepository
// ---------------------------------------------------------------------------

type fakeSummaries struct {
	mu        sync.Mutex
	bySlug    map[string]application.ProductSummary
	search    []application.ProductSummary
	searchErr error
	getErr    error

	lastQuery string
	lastLimit int
}

func newFakeSummaries() *fakeSummaries {
	return &fakeSummaries{bySlug: map[string]application.ProductSummary{}}
}

func (f *fakeSummaries) put(s application.ProductSummary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bySlug[s.ProductSlug] = s
}

func (f *fakeSummaries) Get(_ context.Context, slug string) (application.ProductSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return application.ProductSummary{}, f.getErr
	}
	s, ok := f.bySlug[slug]
	if !ok {
		return application.ProductSummary{}, domain.ErrNotFound
	}
	return s, nil
}

func (f *fakeSummaries) Search(_ context.Context, query string, limit int) ([]application.ProductSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastQuery, f.lastLimit = query, limit
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if limit < len(f.search) {
		return f.search[:limit], nil
	}
	return f.search, nil
}

func (f *fakeSummaries) ListByVendor(context.Context, string, int, string) ([]application.ProductSummary, string, error) {
	return nil, "", nil
}

// ---------------------------------------------------------------------------
// VendorRepository
// ---------------------------------------------------------------------------

type fakeVendors struct {
	mu     sync.Mutex
	bySlug map[string]domain.Vendor
	byID   map[string]domain.Vendor
	list   []domain.Vendor
	next   string
	err    error
}

func newFakeVendors() *fakeVendors {
	return &fakeVendors{bySlug: map[string]domain.Vendor{}, byID: map[string]domain.Vendor{}}
}

func (f *fakeVendors) put(v domain.Vendor) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bySlug[v.Slug] = v
	if v.ID != "" {
		f.byID[v.ID] = v
	}
	f.list = append(f.list, v)
}

// GetByID resolves a vendor the way the review queue needs it to: a queue row carries
// review_items.vendor_id, and naming the vendor is what makes the row recognisable to a
// human. A vendor seeded without an id stays unresolvable, which is the ON DELETE SET
// NULL case the presenter has to survive.
func (f *fakeVendors) GetByID(_ context.Context, id string) (domain.Vendor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return domain.Vendor{}, f.err
	}
	v, ok := f.byID[id]
	if !ok {
		return domain.Vendor{}, domain.ErrNotFound
	}
	return v, nil
}

func (f *fakeVendors) GetBySlug(_ context.Context, slug string) (domain.Vendor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return domain.Vendor{}, f.err
	}
	v, ok := f.bySlug[slug]
	if !ok {
		return domain.Vendor{}, domain.ErrNotFound
	}
	return v, nil
}

func (f *fakeVendors) List(_ context.Context, limit int, _ string) ([]domain.Vendor, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, "", f.err
	}
	out := f.list
	if limit < len(out) {
		out = out[:limit]
	}
	return out, f.next, nil
}

func (f *fakeVendors) Upsert(context.Context, domain.Vendor) error { return nil }

// ---------------------------------------------------------------------------
// ReleaseRepository
// ---------------------------------------------------------------------------

type fakeReleases struct {
	mu        sync.Mutex
	byID      map[string]domain.Release
	byProduct map[string][]domain.Release
	latest    map[string]domain.Release
	next      string
	err       error

	// lastOpts records what the handler asked for. Windowing is a fact about the
	// query, not about the page that comes back (application.ReleaseListOptions), so
	// a test that only inspected the response could not tell a repository that was
	// handed a window from one that was handed none and returned few rows anyway.
	lastOpts application.ReleaseListOptions
}

func newFakeReleases() *fakeReleases {
	return &fakeReleases{
		byID:      map[string]domain.Release{},
		byProduct: map[string][]domain.Release{},
		latest:    map[string]domain.Release{},
	}
}

func (f *fakeReleases) put(productID string, r domain.Release) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[r.ID] = r
	f.byProduct[productID] = append(f.byProduct[productID], r)
}

func (f *fakeReleases) setLatest(productID, channel string, r domain.Release) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latest[productID+"|"+channel] = r
}

func (f *fakeReleases) GetByID(_ context.Context, id string) (domain.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return domain.Release{}, f.err
	}
	r, ok := f.byID[id]
	if !ok {
		return domain.Release{}, domain.ErrNotFound
	}
	return r, nil
}

func (f *fakeReleases) Insert(context.Context, domain.Release, []domain.ReleaseProductMapping) error {
	return nil
}

func (f *fakeReleases) FindDuplicate(context.Context, string, string, string) (string, error) {
	return "", nil
}

// ProductRefForRelease reverse-scans byProduct for the release id and returns the
// productID it was put() under as both Slug and Name -- this fake has no separate
// product registry to resolve a real name from, and a test that cares can put() under
// a realistic-looking id and assert on that.
func (f *fakeReleases) ProductRefForRelease(_ context.Context, id string) (application.ReleaseProductRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for productID, rels := range f.byProduct {
		for _, r := range rels {
			if r.ID == id {
				return application.ReleaseProductRef{Slug: productID, Name: productID}, nil
			}
		}
	}
	return application.ReleaseProductRef{}, domain.ErrNotFound
}

// LatestForProduct honours the port's "an empty channel means ANY channel" rule (see
// application.ReleaseRepository.LatestForProduct) rather than treating "" as a channel
// name of its own.
//
// The map is keyed productID|channel, so an exact hit wins when the caller named a
// channel. When the caller named none, this falls back to any channel registered for
// that product, folded with domain.LatestComparison so the fake picks the release the
// adapter's ORDER BY would -- release date, then first-observed time, never the version
// string. Without the fallback, every handler test that registered a channelled release
// and then asked GET /products/{slug}/latest with no ?channel got a 404 from the fake
// and had to be written around it, which is precisely how the endpoint's own documented
// default call went untested through the defect that broke it in production SQL.
func (f *fakeReleases) LatestForProduct(_ context.Context, productID, channel string) (domain.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return domain.Release{}, f.err
	}
	if r, ok := f.latest[productID+"|"+channel]; ok {
		if !r.Serveable() {
			return domain.Release{}, domain.ErrNotFound
		}
		return r, nil
	}
	if channel != "" {
		return domain.Release{}, domain.ErrNotFound
	}
	var best domain.Release
	found := false
	for key, r := range f.latest {
		if !strings.HasPrefix(key, productID+"|") || !r.Serveable() {
			continue
		}
		if !found || domain.LatestComparison(r, best) > 0 {
			best, found = r, true
		}
	}
	if !found {
		return domain.Release{}, domain.ErrNotFound
	}
	return best, nil
}

func (f *fakeReleases) ListForProduct(_ context.Context, productID string, opts application.ReleaseListOptions) ([]domain.Release, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastOpts = opts
	if f.err != nil {
		return nil, "", f.err
	}
	// Returned in insertion order on purpose. The handler is responsible for the
	// documented ordering, and a fake that pre-sorted would hide a handler that did
	// not order at all.
	//
	// The window, by contrast, is applied here, because the real adapter applies it in
	// SQL: a fake that ignored Since would let a reverted window still pass a test
	// that only counted rows.
	out := make([]domain.Release, 0, len(f.byProduct[productID]))
	for _, rel := range f.byProduct[productID] {
		if insideWindow(rel, opts.Since) {
			out = append(out, rel)
		}
	}
	if opts.Limit > 0 && opts.Limit < len(out) {
		out = out[:opts.Limit]
	}
	return out, f.next, nil
}

// insideWindow delegates to application.WithinHistoryWindow rather than restating the
// rule. It used to restate it, and drifted: it kept comparing the stored anchor after the
// adapter had moved to the end of the period, so every windowing test in this package
// asserted the bug rather than the fix, and reinstating the anchor comparison in SQL
// would have left the whole suite green. A double that reimplements the thing under test
// can only agree with a wrong implementation by luck.
func insideWindow(rel domain.Release, since time.Time) bool {
	return application.WithinHistoryWindow(rel.ReleaseDate, rel.FirstObservedAt, since)
}

func (f *fakeReleases) ClearLatestFlag(context.Context, string, string) error { return nil }

func (f *fakeReleases) TouchVerified(context.Context, string, time.Time) error { return nil }

func (f *fakeReleases) RefreshProductSummary(context.Context, string) error { return nil }

// ---------------------------------------------------------------------------
// APIKeyRepository
// ---------------------------------------------------------------------------

type fakeAPIKeys struct {
	mu       sync.Mutex
	byHash   map[string]application.APIConsumer
	keyIDs   map[string]string
	err      error
	touched  []string
	resolves int
}

func newFakeAPIKeys() *fakeAPIKeys {
	return &fakeAPIKeys{byHash: map[string]application.APIConsumer{}, keyIDs: map[string]string{}}
}

func (f *fakeAPIKeys) add(hash, keyID string, c application.APIConsumer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byHash[hash] = c
	f.keyIDs[hash] = keyID
}

func (f *fakeAPIKeys) ResolveByHash(_ context.Context, hash string) (application.APIConsumer, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if f.err != nil {
		return application.APIConsumer{}, "", f.err
	}
	c, ok := f.byHash[hash]
	if !ok {
		return application.APIConsumer{}, "", domain.ErrNotFound
	}
	return c, f.keyIDs[hash], nil
}

func (f *fakeAPIKeys) Create(context.Context, string, string, string, string) (string, error) {
	return "", nil
}

func (f *fakeAPIKeys) Revoke(context.Context, string, string, time.Time) error { return nil }

func (f *fakeAPIKeys) TouchLastUsed(_ context.Context, keyID string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, keyID)
	return nil
}

// ---------------------------------------------------------------------------
// UsageRecorder
// ---------------------------------------------------------------------------

type fakeUsage struct {
	mu       sync.Mutex
	records  []application.UsageRecord
	consumed int64
	err      error
}

func (f *fakeUsage) Record(_ context.Context, r application.UsageRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, r)
	return nil
}

func (f *fakeUsage) QuotaConsumed(context.Context, string, time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.consumed, f.err
}

func (f *fakeUsage) recorded() []application.UsageRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]application.UsageRecord(nil), f.records...)
}

// ---------------------------------------------------------------------------
// EventPublisher
// ---------------------------------------------------------------------------

type fakeEvents struct {
	mu     sync.Mutex
	events []domain.Event
}

func (f *fakeEvents) Publish(_ context.Context, events ...domain.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, events...)
	return nil
}

func (f *fakeEvents) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.events))
	for _, e := range f.events {
		out = append(out, string(e.Name))
	}
	return out
}

// withName returns every event published under one name, so a test can assert on what an
// event carries and not only that it happened.
func (f *fakeEvents) withName(name domain.EventName) []domain.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.Event
	for _, e := range f.events {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Clock and IDs
// ---------------------------------------------------------------------------

type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type seqIDs struct {
	mu sync.Mutex
	n  int
}

func (g *seqIDs) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return prefix + "_" + time.Duration(g.n).String()
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type harness struct {
	*Server
	summaries *fakeSummaries
	vendors   *fakeVendors
	releases  *fakeReleases
	apiKeys   *fakeAPIKeys
	usage     *fakeUsage
	events    *fakeEvents
	clock     *fixedClock

	// reviewFakes is non-nil only for a harness built with wireReviewAPI or
	// enableReviewAPI. It is how a test seeds the queue the internal surface serves.
	reviewFakes *reviewFixtures
}

// newHarness builds a server over fakes with rate limiting disabled by default, so a
// test that is not about throttling never trips over it.
func newHarness(t *testing.T, customise ...func(*Deps, *harness)) *harness {
	t.Helper()
	h := &harness{
		summaries: newFakeSummaries(),
		vendors:   newFakeVendors(),
		releases:  newFakeReleases(),
		apiKeys:   newFakeAPIKeys(),
		usage:     &fakeUsage{},
		events:    &fakeEvents{},
		clock:     &fixedClock{t: time.Date(2026, 9, 3, 18, 30, 0, 0, time.UTC)},
	}
	d := Deps{
		Summaries:  h.summaries,
		Vendors:    h.vendors,
		Releases:   h.releases,
		APIKeys:    h.apiKeys,
		Usage:      h.usage,
		Events:     h.events,
		Clock:      h.clock,
		IDs:        &seqIDs{},
		Logger:     discardLogger(),
		RateLimits: RateLimitConfig{Disabled: true},
	}
	for _, fn := range customise {
		fn(&d, h)
	}
	srv, err := NewServer(d)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	h.Server = srv
	t.Cleanup(func() { _ = srv.Close() })
	return h
}

// do issues a request against the full middleware chain.
func (h *harness) do(method, target string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for _, fn := range mutate {
		fn(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// discardLogger silences the access log. A test that wants to assert on log output
// builds its own logger over a buffer instead.
func discardLogger() *slog.Logger { return slog.New(discardHandler{}) }

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }

// ---------------------------------------------------------------------------
// the internal review surface
// ---------------------------------------------------------------------------

// reviewFixtures holds the in-memory application fakes the review use cases run over.
//
// The three review dependencies on Deps are concrete use-case types, not interfaces, so
// there is nothing to substitute at the HTTP boundary: a test either builds the real
// ListReviewQueue, GetReviewItem and DecideReviewItem or it tests nothing. That is the
// better bargain anyway -- an accept has to actually publish through PublishRelease and
// actually write the audit row for these tests to mean what they claim -- and
// internal/application/apptest already provides every port they need.
type reviewFixtures struct {
	reviews    *apptest.Reviews
	candidates *apptest.Candidates
	evidence   *apptest.EvidenceStore
	sources    *apptest.Sources
	products   *apptest.Products
	conflicts  *apptest.Conflicts
	audit      *apptest.Audit
	releases   *apptest.Releases
	events     *apptest.Events
	ids        *apptest.IDGen
}

func newReviewFixtures() *reviewFixtures {
	return &reviewFixtures{
		reviews:    &apptest.Reviews{},
		candidates: apptest.NewCandidates(),
		evidence:   apptest.NewEvidenceStore(),
		sources:    apptest.NewSources(),
		products:   apptest.NewProducts(),
		conflicts:  apptest.NewConflicts(),
		audit:      apptest.NewAudit(),
		releases:   apptest.NewReleases(),
		events:     &apptest.Events{},
		ids:        apptest.NewIDGen(),
	}
}

func (f *reviewFixtures) queryDeps(vendors application.VendorRepository, clock application.Clock) application.ReviewQueryDeps {
	return application.ReviewQueryDeps{
		Reviews:    f.reviews,
		Candidates: f.candidates,
		Evidence:   f.evidence,
		Sources:    f.sources,
		Products:   f.products,
		Vendors:    vendors,
		Conflicts:  f.conflicts,
		Audit:      f.audit,
		Clock:      clock,
	}
}

func (f *reviewFixtures) decideDeps(clock application.Clock) application.ReviewDeps {
	ingest := application.IngestDeps{
		Sources:    f.sources,
		Products:   f.products,
		Candidates: f.candidates,
		Releases:   f.releases,
		Evidence:   f.evidence,
		Reviews:    f.reviews,
		Conflicts:  f.conflicts,
		Audit:      f.audit,
		Events:     f.events,
		UoW:        &apptest.UnitOfWork{},
		Clock:      clock,
		IDs:        f.ids,
	}
	return application.ReviewDeps{
		Reviews:    f.reviews,
		Candidates: f.candidates,
		Conflicts:  f.conflicts,
		Audit:      f.audit,
		Publisher:  application.NewPublishRelease(ingest),
		Events:     f.events,
		UoW:        &apptest.UnitOfWork{},
		Clock:      clock,
		IDs:        f.ids,
	}
}

// cursorRejectingReviews refuses any cursor, the way the PostgreSQL adapter refuses one
// it did not mint. It exists because the in-memory queue fake does not paginate -- a fake
// that reimplemented keyset cursors would be testing the fake -- and the error mapping
// for a bad cursor still has to be exercised.
type cursorRejectingReviews struct {
	application.ReviewRepository
}

func (r cursorRejectingReviews) List(_ context.Context, f application.ReviewQueueFilter) ([]application.ReviewItem, string, error) {
	if f.Cursor != "" {
		return nil, "", fmt.Errorf("review.List.cursor: %w", domain.ErrValidation)
	}
	return nil, "", nil
}

// enableReviewAPIRejectingCursors is enableReviewAPI with a queue whose repository
// refuses cursors.
func enableReviewAPIRejectingCursors(d *Deps, h *harness) {
	enableReviewAPI(d, h)
	deps := h.reviewFakes.queryDeps(h.vendors, h.clock)
	deps.Reviews = cursorRejectingReviews{h.reviewFakes.reviews}
	d.ReviewQueue = application.NewListReviewQueue(deps)
}

// wireReviewAPI attaches the three review use cases to Deps without touching
// ReviewAPIEnabled. It is separate from enableReviewAPI so a test can prove that the
// dependencies alone are not enough -- which is half of ADR-0021's double switch.
func wireReviewAPI(d *Deps, h *harness) {
	f := newReviewFixtures()
	h.reviewFakes = f
	d.ReviewQueue = application.NewListReviewQueue(f.queryDeps(h.vendors, h.clock))
	d.ReviewItems = application.NewGetReviewItem(f.queryDeps(h.vendors, h.clock))
	d.Review = application.NewDecideReviewItem(f.decideDeps(h.clock))
}

// enableReviewAPI turns both of ADR-0021's switches on: the three dependencies and the
// flag. Only a harness built with this customiser has any /internal route at all.
func enableReviewAPI(d *Deps, h *harness) {
	wireReviewAPI(d, h)
	d.ReviewAPIEnabled = true
}
