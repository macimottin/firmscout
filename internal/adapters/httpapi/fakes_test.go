package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// Hand-written in-memory fakes. There is no mocking framework and no database: these
// tests are about the HTTP contract -- status codes, media types, headers and the exact
// bytes of the JSON -- and every one of them runs in microseconds against httptest.
//
// Each fake is mutex-guarded because the after-response worker touches some of them
// from a second goroutine, and the suite runs under -race.

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
	list   []domain.Vendor
	next   string
	err    error
}

func newFakeVendors() *fakeVendors { return &fakeVendors{bySlug: map[string]domain.Vendor{}} }

func (f *fakeVendors) put(v domain.Vendor) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bySlug[v.Slug] = v
	f.list = append(f.list, v)
}

func (f *fakeVendors) GetByID(context.Context, string) (domain.Vendor, error) {
	return domain.Vendor{}, domain.ErrNotFound
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

func (f *fakeReleases) LatestForProduct(_ context.Context, productID, channel string) (domain.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return domain.Release{}, f.err
	}
	r, ok := f.latest[productID+"|"+channel]
	if !ok {
		return domain.Release{}, domain.ErrNotFound
	}
	return r, nil
}

func (f *fakeReleases) ListForProduct(_ context.Context, productID string, limit int, _ string) ([]domain.Release, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, "", f.err
	}
	// Returned in insertion order on purpose. The handler is responsible for the
	// documented ordering, and a fake that pre-sorted would hide a handler that did
	// not order at all.
	out := append([]domain.Release(nil), f.byProduct[productID]...)
	if limit < len(out) {
		out = out[:limit]
	}
	return out, f.next, nil
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
