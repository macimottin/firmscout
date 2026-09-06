package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// Handlers are thin on purpose. Each one parses and validates input, calls exactly one
// use case, and hands the result to a presenter. There is no business decision in
// this file: what "latest" means, whether a release may be published, how a version is
// compared, how deep a plan's history goes -- all of that lives in internal/domain and
// internal/application, where it is testable without an HTTP request and reusable from
// the CLI and the worker.
//
// # Why every read handler goes through a use case
//
// It did not, and the cost was exactly what the rule exists to prevent. When the
// handlers called repositories directly, this adapter *was* the read-side application
// layer, and the two copies had already drifted: ListReleases.Execute defaulted the
// page to 50 and clamped an over-large limit down to 50, while this file defaulted to
// 20 and clamped to 100. Worse, the plan history window was added to both rather than
// to one, so a tier-entitlement rule existed twice with only one copy reachable. The
// import graph was legal throughout -- the handler imported application for
// HistoryWindow while keeping the orchestration itself -- which is why archtest could
// not see any of it.
//
// The two endpoints with no use case behind them, GET /vendors and GET /releases/{id},
// are single reads with no rule to duplicate; they stay direct and are named here so
// the omission reads as a decision rather than an oversight.

// Query parameter limits.
//
// The documented contract (api.md §2, openapi.yaml) is enforced here, because clamping
// a caller's parameter is parsing, which is this layer's job. Where the application
// declares the same bound it is used rather than restated, so the two cannot drift;
// the use cases clamp again on their own input, which is the backstop for a caller that
// is not an HTTP request.
const (
	searchQueryMinRunes = 2
	// searchQueryMaxBytes is a byte bound, not a rune bound, because
	// SearchProducts.Execute truncates by bytes: handing it a longer string would let
	// it cut a multi-byte rune in half and send invalid UTF-8 to the trigram index,
	// which is a 500 rather than a search.
	searchQueryMaxBytes = application.MaxSearchQueryLength
	searchLimitDefault  = application.DefaultSearchResults
	searchLimitMax      = application.MaxSearchResults

	vendorLimitDefault = 50
	vendorLimitMax     = 200

	releaseLimitDefault = application.DefaultReleasePageSize
	releaseLimitMax     = application.MaxReleasePageSize
)

// Sort keys accepted by the releases endpoint. There is no "version" key, and its
// absence is a decision, not a gap: a version string is an opaque token, so ordering
// by one silently produces a wrong answer for every vendor whose scheme is not a plain
// dotted-integer sequence. "Latest" is derived from dates, never from string
// comparison. See ADR-0017.
const (
	sortReleaseDate    = "releaseDate"
	sortFirstObserved  = "firstObservedAt"
	orderAscending     = "asc"
	orderDescending    = "desc"
	contentTypeJSONUTF = "application/json; charset=utf-8"
)

// releaseIDPattern is the shape openapi.yaml declares for a release id.
var releaseIDPattern = regexp.MustCompile(`^rel_[A-Za-z0-9]{20,32}$`)

// validChannels is the constrained vocabulary the channel filter matches against. An
// unrecognised value is a 400, never a silently empty result set: an empty page looks
// to a consumer like "this product has no long-term releases", which is a different
// and possibly false claim from "you spelled the filter wrong".
var validChannels = map[string]bool{
	"stable": true, "long_term": true, "testing": true, "development": true,
}

// ---------------------------------------------------------------------------
// search
// ---------------------------------------------------------------------------

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	// Cap before validating: a caller who pastes a whole changelog into q should get
	// a search for the first 200 bytes, not a rejection. The cap is also what bounds
	// the work the trigram index is asked to do.
	q := capSearchQuery(strings.TrimSpace(r.URL.Query().Get("q")))
	if utf8.RuneCountInString(q) < searchQueryMinRunes {
		WriteProblem(w, r, InvalidParameter(r,
			"The 'q' parameter must be at least 2 characters. A shorter query matches most of the catalogue and answers nothing."))
		return
	}

	limit, err := parseLimit(r, searchLimitDefault, searchLimitMax)
	if err != nil {
		WriteProblem(w, r, InvalidParameter(r, err.Error()))
		return
	}

	// The use case bounds the query, runs the search and records the analytics events
	// -- including the zero-result event, which is the most valuable signal FirmScout
	// collects because it is a product request in disguise.
	results, err := s.searchProducts.Execute(r.Context(), application.SearchQuery{Text: q, Limit: limit})
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, PresentSearchResults(results.Results, results.Query))
}

// capSearchQuery truncates q to the longest prefix SearchProducts.Execute will accept
// without truncating it further, cutting only on a rune boundary. See searchQueryMaxBytes.
func capSearchQuery(q string) string {
	if len(q) <= searchQueryMaxBytes {
		return q
	}
	cut := searchQueryMaxBytes
	for cut > 0 && !utf8.RuneStart(q[cut]) {
		cut--
	}
	return q[:cut]
}

// ---------------------------------------------------------------------------
// vendors
// ---------------------------------------------------------------------------

func (s *Server) handleListVendors(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r, vendorLimitDefault, vendorLimitMax)
	if err != nil {
		WriteProblem(w, r, InvalidParameter(r, err.Error()))
		return
	}
	cursor := r.URL.Query().Get("cursor")

	vendors, next, err := s.vendors.List(r.Context(), limit, cursor)
	if err != nil {
		s.writeRepoError(w, r, err, "cursor")
		return
	}
	s.writeJSON(w, r, http.StatusOK, PresentVendors(vendors, next))
}

func (s *Server) handleGetVendor(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !domain.ValidSlug(slug) {
		WriteProblem(w, r, InvalidParameter(r, "The vendor slug must be lowercase kebab-case."))
		return
	}
	// Through the use case rather than straight to the repository, for the same reason
	// handleGetProduct is: GetVendor publishes domain.EventVendorViewed, and this is the
	// only endpoint that can publish it. While this handler called VendorRepository
	// directly the use case had no callers at all, so the one signal FirmScout has about
	// which vendors readers open was never emitted -- a hole no test could see, because
	// a use case nobody calls still compiles and still passes its own unit tests.
	vendor, err := s.getVendor.Execute(r.Context(), slug)
	if errors.Is(err, domain.ErrNotFound) {
		WriteProblem(w, r, NotFound(r, "No vendor with that slug was found."))
		return
	}
	if err != nil {
		WriteProblem(w, r, Internal(r, err))
		return
	}
	s.writeJSON(w, r, http.StatusOK, PresentVendor(vendor))
}

// ---------------------------------------------------------------------------
// products
// ---------------------------------------------------------------------------

func (s *Server) handleGetProduct(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !domain.ValidSlug(slug) {
		WriteProblem(w, r, InvalidParameter(r, "The product slug must be lowercase kebab-case."))
		return
	}
	// The use case reads the summary and records the product view. The event is not
	// emitted here as well: two publishers of one fact is how a funnel ends up
	// double-counting whichever path is busier.
	summary, err := s.getProduct.Execute(r.Context(), slug)
	if errors.Is(err, domain.ErrNotFound) {
		WriteProblem(w, r, NotFound(r, "No product with that slug was found."))
		return
	}
	if err != nil {
		WriteProblem(w, r, Internal(r, err))
		return
	}
	s.writeJSON(w, r, http.StatusOK, PresentProduct(summary))
}

func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	summary, ok := s.lookupProduct(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()

	limit, err := parseLimit(r, releaseLimitDefault, releaseLimitMax)
	if err != nil {
		WriteProblem(w, r, InvalidParameter(r, err.Error()))
		return
	}
	channel := strings.TrimSpace(query.Get("channel"))
	if channel != "" && !validChannels[channel] {
		WriteProblem(w, r, InvalidParameter(r, "Unrecognised 'channel' value. Valid values are stable, long_term, testing and development."))
		return
	}
	releaseType := strings.TrimSpace(query.Get("releaseType"))
	if releaseType != "" && !domain.ValidReleaseType(domain.ReleaseType(releaseType)) {
		WriteProblem(w, r, InvalidParameter(r, "Unrecognised 'releaseType' value."))
		return
	}
	sortKey := query.Get("sort")
	if sortKey == "" {
		sortKey = sortReleaseDate
	}
	if sortKey != sortReleaseDate && sortKey != sortFirstObserved {
		// Named explicitly so a caller who tried sort=version reads why, rather than
		// concluding the parameter is merely unsupported yet.
		WriteProblem(w, r, InvalidParameter(r,
			"Unrecognised 'sort' value. Only releaseDate and firstObservedAt are sortable; version strings are opaque tokens with no defined ordering and are deliberately not a sort key."))
		return
	}
	order := query.Get("order")
	if order == "" {
		order = orderDescending
	}
	if order != orderAscending && order != orderDescending {
		WriteProblem(w, r, InvalidParameter(r, "Unrecognised 'order' value. Use asc or desc."))
		return
	}

	// History depth is a tier boundary (api.md §2). The window is the use case's
	// decision, not this handler's: it reads the caller's plan, applies the boundary
	// through the query rather than by filtering the page the query returned, and
	// reports both the boundary and whether one was applied at all.
	plan := CallerFromContext(r.Context()).Plan()
	page, err := s.listReleases.Execute(r.Context(), application.ReleaseHistoryQuery{
		Slug:   summary.ProductSlug,
		Limit:  limit,
		Cursor: query.Get("cursor"),
		Plan:   plan,
	})
	if err != nil {
		s.writeRepoError(w, r, err, "cursor")
		return
	}

	releases := filterReleases(page.Releases, channel, releaseType)
	orderReleases(releases, sortKey, order)

	ref := &ProductRef{Slug: summary.ProductSlug, Name: summary.ProductName}
	window := PresentHistoryWindow(plan, page.Since, latestOutsideWindow(summary, page.Since))
	s.writeJSON(w, r, http.StatusOK, PresentReleases(releases, ref, page.NextCursor, window))
}

// handleGetLatest answers "what version should I be on?".
//
// # Why this endpoint is not windowed
//
// A caller's plan bounds how much *history* they receive (api.md §2), and the newest
// observed release is not history: it is the current answer, and it is the answer the
// public website -- an anonymous caller of this same API -- exists to show. Windowing
// it would make FirmScout report "this product has no published release" for a product
// that plainly has one, whenever the vendor last shipped more than twelve months ago,
// which is the normal state of the discontinued hardware this catalogue tracks. That is
// a false statement in a product whose only asset is being right, and api.md §1 already
// puts the paywall on endpoints, volume and history depth -- never on the current fact.
//
// The consequence is that an anonymous caller can see a latest release here and an empty
// page from /releases. That is not left to be discovered: the history response's window
// member says so out loud (see latestOutsideWindow).
func (s *Server) handleGetLatest(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !domain.ValidSlug(slug) {
		WriteProblem(w, r, InvalidParameter(r, "The product slug must be lowercase kebab-case."))
		return
	}
	channel := strings.TrimSpace(r.URL.Query().Get("channel"))
	if channel != "" && !validChannels[channel] {
		WriteProblem(w, r, InvalidParameter(r, "Unrecognised 'channel' value. Valid values are stable, long_term, testing and development."))
		return
	}

	result, err := s.getLatest.Execute(r.Context(), slug, channel)
	if err != nil {
		// Both failures are ErrNotFound and they are different answers, so they are
		// told apart by what the use case had already resolved when it gave up: a
		// summary means the product exists and this channel has no release.
		switch {
		case errors.Is(err, domain.ErrNotFound) && result.Summary.ProductSlug == "":
			WriteProblem(w, r, NotFound(r, "No product with that slug was found."))
		// "on that channel" is only honest when the caller named one. An omitted
		// ?channel means ANY channel (see application.ReleaseRepository.LatestForProduct),
		// so blaming a channel the caller never mentioned sends them looking for a
		// filter to remove -- which is exactly the wrong diagnosis, and exactly what
		// this endpoint said while it was answering 404 to its own default call.
		case errors.Is(err, domain.ErrNotFound) && channel == "":
			WriteProblem(w, r, NotFound(r, "This product has no published release."))
		case errors.Is(err, domain.ErrNotFound):
			WriteProblem(w, r, NotFound(r, "This product has no published release on that channel."))
		// The use case now rejects a malformed slug itself. This handler validates the
		// slug before calling, so the branch is unreachable through HTTP today -- it is
		// here so that it stays unreachable for the right reason. Without it, a
		// validation error would fall into default and be answered 500, which would turn
		// the removal of the pre-check into an availability bug instead of a 400.
		case errors.Is(err, domain.ErrValidation):
			WriteProblem(w, r, InvalidParameter(r, "The product slug must be lowercase kebab-case.").WithCause(err))
		default:
			WriteProblem(w, r, Internal(r, err))
		}
		return
	}

	s.writeJSON(w, r, http.StatusOK, PresentLatestRelease(result))
}

// lookupProduct resolves the {slug} path value to a product summary, writing the
// problem document itself when it cannot. ok is false when a response has been sent.
//
// The release history endpoint still needs this even though it goes through a use case,
// because ReleaseHistory carries the product id and not the product's name, and every
// release in the response is rendered with a {slug, name} reference. The report for this
// phase asks for the summary to be returned from ListReleases so the second read goes
// away; until then the second read is one indexed row against a projection, and inventing
// the name here or dropping it from the response are both worse.
func (s *Server) lookupProduct(w http.ResponseWriter, r *http.Request) (application.ProductSummary, bool) {
	slug := r.PathValue("slug")
	if !domain.ValidSlug(slug) {
		WriteProblem(w, r, InvalidParameter(r, "The product slug must be lowercase kebab-case."))
		return application.ProductSummary{}, false
	}
	summary, err := s.summaries.Get(r.Context(), slug)
	if errors.Is(err, domain.ErrNotFound) {
		WriteProblem(w, r, NotFound(r, "No product with that slug was found."))
		return application.ProductSummary{}, false
	}
	if err != nil {
		WriteProblem(w, r, Internal(r, err))
		return application.ProductSummary{}, false
	}
	return summary, true
}

// latestOutsideWindow reports that the product's newest observed release cannot appear
// in this windowed page at all, which is what makes an empty history page and a
// populated /latest response readable as one consistent story instead of two endpoints
// contradicting each other.
//
// It claims "outside" only when the vendor's own date puts it outside at *every*
// precision that date could denote: a release the vendor dated "2025" may have shipped
// in December, so it is not outside a window opening in September 2025, and saying it
// was would assert a day the vendor never published (ADR-0017).
//
// The flag and the page must agree, and the only way to guarantee that is to ask the
// same question the query asks, in the same units. The query's predicate is a date
// comparison in UTC -- the stored precision's period end against the boundary reduced to
// a UTC calendar day (postgres.ListForProduct) -- so this reduces the boundary the same
// way rather than comparing against the raw instant. Comparing PeriodEnd, which is a UTC
// midnight, against an instant carrying a time of day would put this flag and the page
// on different sides of the boundary for a release dated on the boundary day itself: the
// query would return the release while the response asserted it could not appear.
//
// An undated release is never claimed either: the window falls back to first-observed
// time for those (application.ReleaseListOptions.Since), which the summary does not
// carry, and a guess is worth less than an absent flag.
func latestOutsideWindow(summary application.ProductSummary, since time.Time) bool {
	if since.IsZero() || summary.LatestReleaseID == "" || !summary.LatestReleaseDate.Known() {
		return false
	}
	return summary.LatestReleaseDate.PeriodEnd().Before(application.UTCDayOf(since))
}

// ---------------------------------------------------------------------------
// releases
// ---------------------------------------------------------------------------

func (s *Server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !releaseIDPattern.MatchString(id) {
		WriteProblem(w, r, InvalidParameter(r, "The release id must look like rel_<identifier>."))
		return
	}
	release, err := s.releases.GetByID(r.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		WriteProblem(w, r, NotFound(r, "No release with that id was found."))
		return
	}
	if err != nil {
		WriteProblem(w, r, Internal(r, err))
		return
	}
	// Unlike a product's own history or latest release, this request path carries no
	// product context to build a ProductRef from directly -- it has to be resolved
	// from the release id. Only domain.ErrNotFound renders the release with product
	// omitted (PresentRelease's documented "could not resolve" case): Insert's own
	// invariant ("a release with no mapping is unreachable") means that specific case
	// should not occur in practice, but a mapping genuinely deleted out from under a
	// release is at least an honest "we don't know" rather than a fabricated answer.
	//
	// Any OTHER error -- a pool exhausted, a context deadline, an ordinary transient
	// failure on this now-mandatory second round trip -- must not be treated the same
	// way. Omitting product because a database call happened to fail would serve a 200
	// whose missing key means "no mapping exists" when it actually means "we could not
	// look", reproducing exactly the crash class this endpoint exists to have closed.
	// Fail loudly instead, the same way the GetByID lookup three lines above already
	// does. (openapi.yaml no longer lists product as required on Release, because this
	// very branch and /latest both legitimately omit it -- which changes nothing here:
	// an honest omission is a documented state, and a swallowed error is not.)
	var product *ProductRef
	switch ref, perr := s.releases.ProductRefForRelease(r.Context(), id); {
	case perr == nil:
		product = &ProductRef{Slug: ref.Slug, Name: ref.Name}
	case errors.Is(perr, domain.ErrNotFound):
		// product stays nil: an honest "could not resolve", not a fabricated ref.
	default:
		WriteProblem(w, r, Internal(r, perr))
		return
	}
	s.writeJSON(w, r, http.StatusOK, PresentRelease(release, product))
}

// filterReleases applies the exact-match filters to the page the query returned.
//
// # The consequence, which the contract now states rather than hides
//
// The page is fetched and its next cursor minted before this runs, so a filtered query
// can return an empty page together with a non-null nextCursor -- there were rows, and
// none of them matched. A client that stops on an empty page therefore concludes there
// are no matching releases when there may be many, so openapi.yaml and api.md §2 say
// explicitly that a filtered page may be empty while nextCursor is non-null and that a
// client must page until nextCursor is null. Until this phase they said the opposite by
// implication, having promised that "the window is applied by the query, not by
// filtering a returned page, so cursors stay meaningful" -- true of the window, and a
// consumer would read it as a property of the endpoint.
//
// The real fix is to push channel and releaseType into the query beside Since, which
// needs two fields on application.ReleaseListOptions and a WHERE clause in a repository
// this owner does not hold; it is requested in this phase's report. Documenting the
// behaviour is the honest interim, because the alternative -- leaving a claim in the
// contract that the code does not honour -- is the failure mode, not the filter.
func filterReleases(rs []domain.Release, channel, releaseType string) []domain.Release {
	if channel == "" && releaseType == "" {
		return rs
	}
	out := make([]domain.Release, 0, len(rs))
	for _, rel := range rs {
		if channel != "" && rel.Channel != channel {
			continue
		}
		if releaseType != "" && string(rel.ReleaseType) != releaseType {
			continue
		}
		out = append(out, rel)
	}
	return out
}

// orderReleases sorts a page of releases.
//
// # It sorts the page, not the history
//
// The query orders by first_observed_at DESC, id DESC -- the pair the cursor is built
// from, because a page order that differs from the cursor order cannot paginate -- and
// this runs afterwards, over the rows already fetched. So `sort` and `order` reorder
// each page internally and do not change which rows land on which page: asking for
// `order=asc` returns the newest page first, ascending within itself. That is stated in
// api.md §2 and in openapi.yaml rather than left for a consumer to discover, and the
// real fix is a query that orders by the requested key with a cursor built from it,
// which is a repository change and not a handler one.
//
// # Why this is not a sort by version
//
// The obvious implementation -- sort by version string descending -- is wrong for
// most of the vendors FirmScout tracks. "9.99.99" sorts above "1.0.0" under every
// string and semver comparison, and yet if 9.99.99 shipped in January and 1.0.0
// shipped in September then September's release is the later one. Vendors renumber,
// fork stable and long-term streams with unrelated numbering, and use schemes like
// 3.003.0015.001 that no general comparator orders correctly. Version strings are
// opaque tokens (ADR-0017); recency is a fact about dates.
//
// So the ordering is: releases with a known date first, ordered by that date; releases
// with no date at all last, because an undated observation is weaker evidence of
// recency than a dated one and putting it at the top would misrepresent it as newest.
// First-observed time breaks ties, and the id breaks the remaining ties so the order
// is stable across identical pages.
func orderReleases(rs []domain.Release, sortKey, order string) {
	desc := order != orderAscending
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if sortKey == sortReleaseDate {
			aKnown, bKnown := a.ReleaseDate.Known(), b.ReleaseDate.Known()
			if aKnown != bKnown {
				// Nulls last in both sort directions: their position is a
				// statement about evidence quality, not about ordering.
				return aKnown
			}
			if aKnown && bKnown {
				switch {
				case a.ReleaseDate.Before(b.ReleaseDate):
					return !desc
				case b.ReleaseDate.Before(a.ReleaseDate):
					return desc
				}
			}
		}
		switch {
		case a.FirstObservedAt.Before(b.FirstObservedAt):
			return !desc
		case b.FirstObservedAt.Before(a.FirstObservedAt):
			return desc
		}
		if desc {
			return a.ID > b.ID
		}
		return a.ID < b.ID
	})
}

// ---------------------------------------------------------------------------
// system endpoints
// ---------------------------------------------------------------------------

// handleHealthz is liveness. It answers 200 unconditionally and touches nothing.
//
// A liveness probe that checks the database is a liveness probe that restarts every
// API instance during a database incident, turning a degraded read path into a total
// outage and a crash loop. Whether this process should be killed and whether it can
// currently serve useful traffic are different questions; this endpoint answers only
// the first. Readiness answers the second.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(HeaderCacheControl, "no-store")
	s.writeJSON(w, r, http.StatusOK, HealthStatus{Status: StatusOK})
}

// handleReadyz reports whether this instance should receive traffic. It runs the
// injected gate, which in production pings the database, and answers 503 with a
// problem document when the gate fails so a load balancer takes the instance out of
// rotation without the process being restarted.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(HeaderCacheControl, "no-store")
	if s.ready == nil {
		s.writeJSON(w, r, http.StatusOK, HealthStatus{Status: StatusOK})
		return
	}
	if err := s.ready(r.Context()); err != nil {
		WriteProblem(w, r, ServiceUnavailable(r,
			"A dependency required to serve requests is unavailable.", err))
		return
	}
	s.writeJSON(w, r, http.StatusOK, HealthStatus{Status: StatusOK})
}

// handleNotFound answers an unrouted path with a problem document rather than
// net/http's plain-text 404, so a consumer's error handling sees the same media type
// for every failure.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	WriteProblem(w, r, NotFound(r, "No endpoint matches this path and method."))
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// parseLimit reads the limit parameter, clamping it to max rather than rejecting an
// over-large value. A caller asking for more than the page maximum wants as much as
// they can get, and clamping tells them so through the returned page size; a
// non-numeric or non-positive value, by contrast, is a mistake worth naming.
func parseLimit(r *http.Request, def, max int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("limit must be a positive integer")
	}
	if n < 1 {
		return 0, errors.New("limit must be at least 1")
	}
	if n > max {
		n = max
	}
	return n, nil
}

// writeQueryError maps a read use case's failure to a problem document.
//
// It is the fallback for a use case whose failures the handler cannot describe more
// precisely than the catalogue in api.md §6 does. A handler that can say something
// better -- "no product with that slug", "no release on that channel" -- says it
// itself, because a caller reading "the requested resource does not exist" on a URL
// with two resources in it learns nothing.
func (s *Server) writeQueryError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		WriteProblem(w, r, NotFound(r, "The requested resource does not exist."))
	case errors.Is(err, domain.ErrValidation):
		WriteProblem(w, r, InvalidParameter(r, "A parameter is not one this endpoint accepts.").WithCause(err))
	default:
		WriteProblem(w, r, Internal(r, err))
	}
}

// writeRepoError maps a repository failure to a problem document. A malformed cursor
// is the caller's mistake, so it is a 400 naming the parameter rather than a 500.
func (s *Server) writeRepoError(w http.ResponseWriter, r *http.Request, err error, param string) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		WriteProblem(w, r, NotFound(r, "The requested resource does not exist."))
	case errors.Is(err, domain.ErrValidation):
		WriteProblem(w, r, InvalidParameter(r, "The '"+param+"' parameter is not a cursor this API issued.").WithCause(err))
	default:
		WriteProblem(w, r, Internal(r, err))
	}
}

// writeJSON serialises v.
//
// The body is marshalled before the status line is written so that a marshalling
// failure can still become a 500 problem document; writing the header first and then
// discovering the encoder failed leaves a truncated 200 on the wire, which a consumer
// cannot distinguish from a network fault.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		WriteProblem(w, r, Internal(r, err))
		return
	}
	h := w.Header()
	h.Set(headerContentTypeKey, contentTypeJSONUTF)
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
