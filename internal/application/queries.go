package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

// QueryDeps are the ports the read-side use cases need.
//
// Read paths deliberately do not go through the write-side repositories. They read the
// precomputed product_summaries projection, because a product page served by a
// six-way join at request time is the difference between a public site that is cheap
// to run and one that is not. See ADR-0013.
type QueryDeps struct {
	Summaries SummaryRepository
	Vendors   VendorRepository
	Products  ProductRepository
	Releases  ReleaseRepository
	Events    EventPublisher
	Clock     Clock
}

// SearchProducts answers the public search box.
//
// Search is the most expensive public endpoint and the most attractive one to abuse,
// so the input is bounded before it reaches the database: a minimum length that
// rejects single-character catalogue sweeps, a maximum that rejects pathological
// queries, and a hard result cap.
type SearchProducts struct {
	deps QueryDeps
}

// NewSearchProducts builds the use case.
func NewSearchProducts(d QueryDeps) *SearchProducts { return &SearchProducts{deps: d} }

// Search bounds.
const (
	MinSearchQueryLength = 2
	MaxSearchQueryLength = 200
	MaxSearchResults     = 50
	DefaultSearchResults = 20
)

// SearchQuery is the validated input.
type SearchQuery struct {
	Text  string
	Limit int
}

// SearchResults carries the matches and the query that produced them.
type SearchResults struct {
	Query   string
	Results []ProductSummary
	Count   int
}

// Execute runs a bounded product search and records the analytics events that drive
// the catalogue's own roadmap. A search returning nothing is the single most valuable
// signal FirmScout collects: it is either a product to catalogue, an alias to learn,
// or a spelling to tolerate.
func (uc *SearchProducts) Execute(ctx context.Context, q SearchQuery) (SearchResults, error) {
	text := strings.TrimSpace(q.Text)
	if len(text) < MinSearchQueryLength {
		return SearchResults{}, fmt.Errorf("search query must be at least %d characters: %w",
			MinSearchQueryLength, domain.ErrValidation)
	}
	if len(text) > MaxSearchQueryLength {
		text = text[:MaxSearchQueryLength]
	}

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultSearchResults
	}
	if limit > MaxSearchResults {
		limit = MaxSearchResults
	}

	results, err := uc.deps.Summaries.Search(ctx, text, limit)
	if err != nil {
		return SearchResults{}, fmt.Errorf("search: %w", err)
	}

	now := uc.deps.Clock.Now()
	ev := domain.NewEvent(domain.EventSearchExecuted, now, "search", "").
		With("result_count", itoa(len(results)))
	events := []domain.Event{ev}
	if len(results) == 0 {
		// Deliberately recorded with the query text: a zero-result search is a
		// product request, and truncation happens in the analytics privacy filter
		// rather than here.
		events = append(events, domain.NewEvent(domain.EventSearchReturnedNoResult, now, "search", "").
			With("query", text))
	}
	if err := uc.deps.Events.Publish(ctx, events...); err != nil {
		return SearchResults{}, fmt.Errorf("publish search events: %w", err)
	}

	return SearchResults{Query: text, Results: results, Count: len(results)}, nil
}

// GetProduct serves a product page from the precomputed summary.
type GetProduct struct {
	deps QueryDeps
}

// NewGetProduct builds the use case.
func NewGetProduct(d QueryDeps) *GetProduct { return &GetProduct{deps: d} }

// Execute returns the summary for a product slug.
func (uc *GetProduct) Execute(ctx context.Context, slug string) (ProductSummary, error) {
	slug = strings.TrimSpace(strings.ToLower(slug))
	if !domain.ValidSlug(slug) {
		return ProductSummary{}, fmt.Errorf("invalid product slug %q: %w", slug, domain.ErrValidation)
	}
	s, err := uc.deps.Summaries.Get(ctx, slug)
	if err != nil {
		return ProductSummary{}, err
	}
	if err := uc.deps.Events.Publish(ctx,
		domain.NewEvent(domain.EventProductViewed, uc.deps.Clock.Now(), "product", s.ProductID).
			WithProduct(s.VendorSlug, s.ProductSlug)); err != nil {
		return s, fmt.Errorf("publish event: %w", err)
	}
	return s, nil
}

// GetLatestRelease answers "what version should I be on?" for one product.
type GetLatestRelease struct {
	deps QueryDeps
}

// NewGetLatestRelease builds the use case.
func NewGetLatestRelease(d QueryDeps) *GetLatestRelease { return &GetLatestRelease{deps: d} }

// LatestReleaseResult carries the answer plus the honesty the product depends on.
type LatestReleaseResult struct {
	Summary ProductSummary
	Release domain.Release
	// Recommended is the vendor's own designation, or nil when the vendor never
	// made one. FirmScout never substitutes "the newest release" for "the
	// recommended release": the newest version is not automatically the safest.
	Recommended *domain.Release
	// HasConflict reports that active sources disagree about this product, which the
	// caller must surface rather than hide.
	HasConflict bool
}

// Execute returns the latest observed release for a product and channel.
func (uc *GetLatestRelease) Execute(ctx context.Context, slug, channel string) (LatestReleaseResult, error) {
	summary, err := uc.deps.Summaries.Get(ctx, strings.TrimSpace(strings.ToLower(slug)))
	if err != nil {
		return LatestReleaseResult{}, err
	}

	out := LatestReleaseResult{Summary: summary, HasConflict: summary.HasSourceConflict}

	rel, err := uc.deps.Releases.LatestForProduct(ctx, summary.ProductID, channel)
	switch {
	case err == nil:
		out.Release = rel
	case isNotFound(err):
		return out, fmt.Errorf("no releases recorded for %s: %w", slug, domain.ErrNotFound)
	default:
		return out, fmt.Errorf("latest release: %w", err)
	}

	if summary.RecommendedReleaseID != "" {
		rec, err := uc.deps.Releases.GetByID(ctx, summary.RecommendedReleaseID)
		if err == nil {
			out.Recommended = &rec
		} else if !isNotFound(err) {
			return out, fmt.Errorf("recommended release: %w", err)
		}
	}

	return out, nil
}

// ListReleases returns a product's version history.
type ListReleases struct {
	deps QueryDeps
}

// NewListReleases builds the use case.
func NewListReleases(d QueryDeps) *ListReleases { return &ListReleases{deps: d} }

// ReleaseHistory is a page of a product's releases.
type ReleaseHistory struct {
	ProductID  string
	Releases   []domain.Release
	NextCursor string
}

// MaxReleasePageSize bounds a history page.
const MaxReleasePageSize = 100

// Execute returns a page of release history.
//
// The ordering is release date descending with unknown dates last, then first-observed
// descending. It is never version-string ordering: a product whose vendor renumbered
// its scheme would otherwise have its history scrambled.
func (uc *ListReleases) Execute(ctx context.Context, slug string, limit int, cursor string) (ReleaseHistory, error) {
	summary, err := uc.deps.Summaries.Get(ctx, strings.TrimSpace(strings.ToLower(slug)))
	if err != nil {
		return ReleaseHistory{}, err
	}
	if limit <= 0 || limit > MaxReleasePageSize {
		limit = 50
	}
	releases, next, err := uc.deps.Releases.ListForProduct(ctx, summary.ProductID, limit, cursor)
	if err != nil {
		return ReleaseHistory{}, fmt.Errorf("list releases: %w", err)
	}
	return ReleaseHistory{ProductID: summary.ProductID, Releases: releases, NextCursor: next}, nil
}

// GetVendor serves a vendor page.
type GetVendor struct {
	deps QueryDeps
}

// NewGetVendor builds the use case.
func NewGetVendor(d QueryDeps) *GetVendor { return &GetVendor{deps: d} }

// VendorPage is a vendor with its products.
type VendorPage struct {
	Vendor     domain.Vendor
	Products   []ProductSummary
	NextCursor string
}

// Execute returns a vendor and a page of its products.
func (uc *GetVendor) Execute(ctx context.Context, slug string, limit int, cursor string) (VendorPage, error) {
	slug = strings.TrimSpace(strings.ToLower(slug))
	if !domain.ValidSlug(slug) {
		return VendorPage{}, fmt.Errorf("invalid vendor slug %q: %w", slug, domain.ErrValidation)
	}
	v, err := uc.deps.Vendors.GetBySlug(ctx, slug)
	if err != nil {
		return VendorPage{}, err
	}
	if limit <= 0 || limit > MaxReleasePageSize {
		limit = 50
	}
	products, next, err := uc.deps.Summaries.ListByVendor(ctx, slug, limit, cursor)
	if err != nil {
		return VendorPage{}, fmt.Errorf("list vendor products: %w", err)
	}
	if err := uc.deps.Events.Publish(ctx,
		domain.NewEvent(domain.EventVendorViewed, uc.deps.Clock.Now(), "vendor", v.ID).
			With("vendor_slug", slug)); err != nil {
		return VendorPage{}, fmt.Errorf("publish event: %w", err)
	}
	return VendorPage{Vendor: v, Products: products, NextCursor: next}, nil
}

// RecordAPIUsage meters one API request.
//
// Metering runs after the response is written, so a customer's latency never pays for
// the meter, and every record carries an idempotency key so a retried request cannot
// be billed twice.
type RecordAPIUsage struct {
	recorder UsageRecorder
	clock    Clock
	ids      IDGenerator
}

// NewRecordAPIUsage builds the use case.
func NewRecordAPIUsage(r UsageRecorder, c Clock, ids IDGenerator) *RecordAPIUsage {
	return &RecordAPIUsage{recorder: r, clock: c, ids: ids}
}

// Execute records one request's usage.
func (uc *RecordAPIUsage) Execute(ctx context.Context, r UsageRecord) error {
	if r.ID == "" {
		r.ID = uc.ids.NewID("usg")
	}
	if r.IdempotencyKey == "" {
		return fmt.Errorf("usage record needs an idempotency key: %w", domain.ErrValidation)
	}
	if r.OccurredAt.IsZero() {
		r.OccurredAt = uc.clock.Now()
	}
	if r.QuotaWeight <= 0 {
		r.QuotaWeight = 1
	}
	return uc.recorder.Record(ctx, r)
}

// EvaluateAPIEntitlement decides whether a consumer may make a request.
type EvaluateAPIEntitlement struct {
	usage UsageRecorder
	clock Clock
}

// NewEvaluateAPIEntitlement builds the use case.
func NewEvaluateAPIEntitlement(u UsageRecorder, c Clock) *EvaluateAPIEntitlement {
	return &EvaluateAPIEntitlement{usage: u, clock: c}
}

// Entitlement is the verdict for one request.
type Entitlement struct {
	Allowed        bool
	Reason         string
	QuotaConsumed  int64
	QuotaLimit     int64
	QuotaRemaining int64
	RetryAfter     time.Duration
}

// Execute evaluates a consumer's monthly quota.
//
// Enforcement reads the aggregated counter rather than scanning raw usage rows, which
// means it can lag by one aggregation window and allow slight overage at a quota
// boundary. That is an accepted trade: the alternative costs latency on every request
// to prevent a rounding error on a few.
func (uc *EvaluateAPIEntitlement) Execute(ctx context.Context, c APIConsumer, weight int) (Entitlement, error) {
	if c.Status != "active" {
		return Entitlement{Allowed: false, Reason: "account is not active"}, nil
	}
	if c.MonthlyQuota <= 0 {
		return Entitlement{Allowed: true, Reason: "no quota configured"}, nil
	}

	now := uc.clock.Now()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	consumed, err := uc.usage.QuotaConsumed(ctx, c.ID, periodStart)
	if err != nil {
		return Entitlement{}, fmt.Errorf("read quota: %w", err)
	}

	if weight <= 0 {
		weight = 1
	}
	remaining := c.MonthlyQuota - consumed
	if remaining < int64(weight) {
		// Retry-After points at the start of the next month, which is when the quota
		// actually resets. Telling a client to retry sooner would be a lie.
		nextPeriod := periodStart.AddDate(0, 1, 0)
		return Entitlement{
			Allowed:        false,
			Reason:         "monthly quota exhausted",
			QuotaConsumed:  consumed,
			QuotaLimit:     c.MonthlyQuota,
			QuotaRemaining: 0,
			RetryAfter:     nextPeriod.Sub(now),
		}, nil
	}

	return Entitlement{
		Allowed:        true,
		QuotaConsumed:  consumed,
		QuotaLimit:     c.MonthlyQuota,
		QuotaRemaining: remaining,
	}, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
