package application

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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

// Search bounds, in the units api.md §2 states them in.
//
// The two length bounds are counted differently on purpose, and neither is counted the
// way len() counts a string. The minimum is "at least 2 characters", so it is a rune
// count: a single three-byte ideograph is one character and one catalogue sweep, and a
// byte count would admit it. The maximum is "a longer query is truncated to 200 bytes
// of UTF-8 on a character boundary", so it is a byte bound that must never split a
// rune -- the trigram index is given a string, and half a rune is invalid UTF-8, which
// is a 500 rather than a search.
const (
	// MinSearchQueryLength is a number of runes.
	MinSearchQueryLength = 2
	// MaxSearchQueryLength is a number of bytes, cut on a rune boundary.
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
	// Both bounds are applied here rather than trusted to a caller. They used to hold
	// only because the HTTP handler applied them first -- counting runes for the
	// minimum and cutting on a rune boundary for the maximum, neither of which this
	// use case did -- so a non-HTTP caller could search for a single character, and
	// one passing a long multi-byte string got a rune sliced in half and handed to the
	// trigram index. A use case whose documented input rules are enforced by one of
	// its callers has no rules.
	text := strings.TrimSpace(q.Text)
	if utf8.RuneCountInString(text) < MinSearchQueryLength {
		return SearchResults{}, fmt.Errorf("search query must be at least %d characters: %w",
			MinSearchQueryLength, domain.ErrValidation)
	}
	text = truncateOnRuneBoundary(text, MaxSearchQueryLength)

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
	// Conflict is the detail behind HasConflict -- copied straight from the summary,
	// never independently derived, so the two can never say something different about
	// the same product. See ProductConflictSummary.
	Conflict *ProductConflictSummary
}

// Execute returns the latest observed release for a product and channel.
//
// The slug is validated here, as GetProduct validates it, rather than being handed to
// the projection to fail as a lookup miss. A slug that cannot name anything is a
// malformed request, not a request for something absent, and the two carry different
// answers in api.md §6: invalid-parameter (400) and not-found (404). Leaving the check
// out let "Not_A_Slug" come back as 404 for any caller that did not happen to be the
// HTTP handler, which validates first -- the same shape of drift as the page-size
// clamps, where a use case's real contract was whatever its busiest caller enforced.
func (uc *GetLatestRelease) Execute(ctx context.Context, slug, channel string) (LatestReleaseResult, error) {
	slug = strings.TrimSpace(strings.ToLower(slug))
	if !domain.ValidSlug(slug) {
		return LatestReleaseResult{}, fmt.Errorf("invalid product slug %q: %w", slug, domain.ErrValidation)
	}
	summary, err := uc.deps.Summaries.Get(ctx, slug)
	if err != nil {
		return LatestReleaseResult{}, err
	}

	out := LatestReleaseResult{
		Summary:     summary,
		HasConflict: summary.HasSourceConflict,
		Conflict:    summary.Conflict,
	}

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

// ReleaseHistoryQuery is one product's history request.
type ReleaseHistoryQuery struct {
	Slug   string
	Limit  int
	Cursor string
	// Plan decides the history window. Empty means PlanAnonymous.
	Plan string
}

// ReleaseHistory is a page of a product's releases.
type ReleaseHistory struct {
	ProductID  string
	Releases   []domain.Release
	NextCursor string
	// Windowed reports that this page is bounded by the caller's plan rather than by
	// the catalogue, and Since is the boundary. A consumer that cannot tell a complete
	// history from a truncated one has been misled by omission.
	Windowed bool
	Since    time.Time
}

// Release history page bounds. These are the contract api.md §2 documents for
// GET /api/v1/products/{slug}/releases -- "limit (default 20, max 100)" -- and this is
// where it is enforced, for every caller and not only for an HTTP request.
//
// The default lived only in the HTTP handler until this file's clamp was corrected,
// which meant a non-HTTP caller asking for nothing got 50 and one asking for 150 got 50
// as well: two answers the documentation never promised, invisible from the API because
// the handler happened to clamp first. A use case that leaves its documented bounds to
// whichever caller reaches it is not the source of truth for them.
const (
	DefaultReleasePageSize = 20
	MaxReleasePageSize     = 100
)

// Execute returns a page of release history.
//
// The ordering is the repository's: first-observed descending, with the release id
// breaking ties, because that pair is also the keyset the cursor is built from and a
// page order that differs from the cursor order cannot paginate. It is never
// version-string ordering: a product whose vendor renumbered its scheme would otherwise
// have its history scrambled (ADR-0017).
//
// This comment used to claim the order was release date descending with unknown dates
// last. It never was, at any layer -- ListForProduct has always ordered by
// first_observed_at DESC, id DESC and nothing here reorders. The claim came from the
// sort the HTTP handler applies to the page after it is fetched, which is a different
// thing and is documented as such (api.md §2, "Sorting is page-local"): a use case that
// describes its caller's behaviour as its own is how a contract ends up with two
// answers.
//
// The window is applied by the repository (ReleaseListOptions.Since), not by filtering
// the page returned here: filtering afterwards would consume a cursor for rows the
// caller never sees, which would make a windowed consumer's pages short for no reason
// they could discover.
func (uc *ListReleases) Execute(ctx context.Context, q ReleaseHistoryQuery) (ReleaseHistory, error) {
	// Same reason as GetLatestRelease.Execute: a slug that cannot name anything is a
	// 400, not a 404, and the use case must say so for every caller rather than relying
	// on the one caller that happens to check first.
	slug := strings.TrimSpace(strings.ToLower(q.Slug))
	if !domain.ValidSlug(slug) {
		return ReleaseHistory{}, fmt.Errorf("invalid product slug %q: %w", slug, domain.ErrValidation)
	}
	summary, err := uc.deps.Summaries.Get(ctx, slug)
	if err != nil {
		return ReleaseHistory{}, err
	}
	// Out of range in each direction is answered differently on purpose: absent means
	// "give me the documented default", while too large means "give me as much as you
	// will allow" -- a caller asking for 150 wants a big page, and answering with 20
	// would be a smaller page than the one they would have got by asking for nothing.
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultReleasePageSize
	}
	if limit > MaxReleasePageSize {
		limit = MaxReleasePageSize
	}

	plan := q.Plan
	if plan == "" {
		plan = PlanAnonymous
	}
	since := HistoryWindow(plan, uc.deps.Clock.Now())

	releases, next, err := uc.deps.Releases.ListForProduct(ctx, summary.ProductID, ReleaseListOptions{
		Limit:  limit,
		Cursor: q.Cursor,
		Since:  since,
	})
	if err != nil {
		return ReleaseHistory{}, fmt.Errorf("list releases: %w", err)
	}
	return ReleaseHistory{
		ProductID:  summary.ProductID,
		Releases:   releases,
		NextCursor: next,
		Windowed:   HistoryWindowed(plan),
		Since:      since,
	}, nil
}

// GetVendor serves one vendor's detail.
//
// It is the counterpart of GetProduct: resolve the slug, read the record, record that
// somebody looked at it. The view event is the reason this use case exists rather than
// the handler simply calling VendorRepository.GetBySlug -- VendorViewed is the only
// signal FirmScout has about which vendors the catalogue's readers care about, and it
// is published in exactly one place so that a funnel cannot double-count whichever path
// is busier.
//
// It used to return a page of the vendor's products as well, bounded by a limit taken
// from MaxReleasePageSize. No endpoint asks for that: api.md §2 and openapi.yaml both
// declare GET /api/v1/vendors/{slug} as a `Vendor`, with the slug as its only
// parameter. Keeping the listing meant every vendor request paid for a product query
// nothing rendered, and borrowing a release-history bound for a product list was a
// third page-size rule with no documentation behind it. SummaryRepository.ListByVendor
// remains available for the vendor-page endpoint that would need it; the use case will
// grow the method when that endpoint is documented, rather than carrying an
// unreachable answer in the meantime.
type GetVendor struct {
	deps QueryDeps
}

// NewGetVendor builds the use case.
func NewGetVendor(d QueryDeps) *GetVendor { return &GetVendor{deps: d} }

// Execute returns the vendor with the given slug.
func (uc *GetVendor) Execute(ctx context.Context, slug string) (domain.Vendor, error) {
	slug = strings.TrimSpace(strings.ToLower(slug))
	if !domain.ValidSlug(slug) {
		return domain.Vendor{}, fmt.Errorf("invalid vendor slug %q: %w", slug, domain.ErrValidation)
	}
	v, err := uc.deps.Vendors.GetBySlug(ctx, slug)
	if err != nil {
		return domain.Vendor{}, err
	}
	// The vendor is returned alongside a publish failure, as GetProduct does: the read
	// succeeded, and losing an analytics event is not a reason to fail a page.
	if err := uc.deps.Events.Publish(ctx,
		domain.NewEvent(domain.EventVendorViewed, uc.deps.Clock.Now(), "vendor", v.ID).
			With("vendor_slug", slug)); err != nil {
		return v, fmt.Errorf("publish event: %w", err)
	}
	return v, nil
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

// truncateOnRuneBoundary cuts s to at most maxBytes bytes, moving the cut backwards
// until it falls between runes. Slicing a Go string at a byte offset is free to land
// inside a multi-byte rune, and the two or three bytes that survive are not valid
// UTF-8; PostgreSQL rejects them, so the result of over-long input would be an error
// rather than a shorter search.
func truncateOnRuneBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
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
