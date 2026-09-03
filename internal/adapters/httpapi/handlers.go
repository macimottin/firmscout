package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// Handlers are thin on purpose. Each one parses and validates input, calls exactly one
// read port, and hands the result to a presenter. There is no business decision in
// this file: what "latest" means, whether a release may be published, how a version is
// compared -- all of that lives in internal/domain and internal/application, where it
// is testable without an HTTP request and reusable from the CLI and the worker.

// Query parameter limits, from docs/architecture/api.md §2 and docs/api/openapi.yaml.
const (
	searchQueryMaxRunes = 200
	searchQueryMinRunes = 2
	searchLimitDefault  = 20
	searchLimitMax      = 50

	vendorLimitDefault = 50
	vendorLimitMax     = 200

	releaseLimitDefault = 20
	releaseLimitMax     = 100
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
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	// Cap before validating: a caller who pastes a whole changelog into q should get
	// a search for the first 200 characters, not a rejection. The cap is also what
	// bounds the work the trigram index is asked to do.
	if utf8.RuneCountInString(q) > searchQueryMaxRunes {
		q = string([]rune(q)[:searchQueryMaxRunes])
	}
	if utf8.RuneCountInString(q) < searchQueryMinRunes {
		WriteProblem(w, r, InvalidRequest(r,
			"The 'q' parameter must be at least 2 characters. A shorter query matches most of the catalogue and answers nothing."))
		return
	}

	limit, err := parseLimit(r, searchLimitDefault, searchLimitMax)
	if err != nil {
		WriteProblem(w, r, InvalidRequest(r, err.Error()))
		return
	}

	results, err := s.summaries.Search(ctx, q, limit)
	if err != nil {
		WriteProblem(w, r, Internal(r, err))
		return
	}

	// Analytics: what people search for, and -- more useful -- what they search for
	// and do not find, which is the backlog of products worth adding.
	now := s.now()
	events := []domain.Event{
		domain.NewEvent(domain.EventSearchExecuted, now, "search", "").
			With("query", q).
			With("result_count", strconv.Itoa(len(results))).
			With("limit", strconv.Itoa(limit)),
	}
	if len(results) == 0 {
		events = append(events, domain.NewEvent(domain.EventSearchReturnedNoResult, now, "search", "").
			With("query", q))
	}
	s.publish(events...)

	s.writeJSON(w, r, http.StatusOK, PresentSearchResults(results, q))
}

// ---------------------------------------------------------------------------
// vendors
// ---------------------------------------------------------------------------

func (s *Server) handleListVendors(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r, vendorLimitDefault, vendorLimitMax)
	if err != nil {
		WriteProblem(w, r, InvalidRequest(r, err.Error()))
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
		WriteProblem(w, r, InvalidRequest(r, "The vendor slug must be lowercase kebab-case."))
		return
	}
	vendor, err := s.vendors.GetBySlug(r.Context(), slug)
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
	summary, ok := s.lookupProduct(w, r)
	if !ok {
		return
	}
	s.publish(domain.NewEvent(domain.EventProductViewed, s.now(), "product", summary.ProductID).
		WithProduct(summary.VendorSlug, summary.ProductSlug))
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
		WriteProblem(w, r, InvalidRequest(r, err.Error()))
		return
	}
	channel := strings.TrimSpace(query.Get("channel"))
	if channel != "" && !validChannels[channel] {
		WriteProblem(w, r, InvalidRequest(r, "Unrecognised 'channel' value. Valid values are stable, long_term, testing and development."))
		return
	}
	releaseType := strings.TrimSpace(query.Get("releaseType"))
	if releaseType != "" && !domain.ValidReleaseType(domain.ReleaseType(releaseType)) {
		WriteProblem(w, r, InvalidRequest(r, "Unrecognised 'releaseType' value."))
		return
	}
	sortKey := query.Get("sort")
	if sortKey == "" {
		sortKey = sortReleaseDate
	}
	if sortKey != sortReleaseDate && sortKey != sortFirstObserved {
		// Named explicitly so a caller who tried sort=version reads why, rather than
		// concluding the parameter is merely unsupported yet.
		WriteProblem(w, r, InvalidRequest(r,
			"Unrecognised 'sort' value. Only releaseDate and firstObservedAt are sortable; version strings are opaque tokens with no defined ordering and are deliberately not a sort key."))
		return
	}
	order := query.Get("order")
	if order == "" {
		order = orderDescending
	}
	if order != orderAscending && order != orderDescending {
		WriteProblem(w, r, InvalidRequest(r, "Unrecognised 'order' value. Use asc or desc."))
		return
	}

	releases, next, err := s.releases.ListForProduct(r.Context(), summary.ProductID, limit, query.Get("cursor"))
	if err != nil {
		s.writeRepoError(w, r, err, "cursor")
		return
	}

	releases = filterReleases(releases, channel, releaseType)
	orderReleases(releases, sortKey, order)

	ref := &ProductRef{Slug: summary.ProductSlug, Name: summary.ProductName}
	s.writeJSON(w, r, http.StatusOK, PresentReleases(releases, ref, next))
}

func (s *Server) handleGetLatest(w http.ResponseWriter, r *http.Request) {
	summary, ok := s.lookupProduct(w, r)
	if !ok {
		return
	}
	channel := strings.TrimSpace(r.URL.Query().Get("channel"))
	if channel != "" && !validChannels[channel] {
		WriteProblem(w, r, InvalidRequest(r, "Unrecognised 'channel' value. Valid values are stable, long_term, testing and development."))
		return
	}

	release, err := s.releases.LatestForProduct(r.Context(), summary.ProductID, channel)
	if errors.Is(err, domain.ErrNotFound) {
		WriteProblem(w, r, NotFound(r, "This product has no published release on that channel."))
		return
	}
	if err != nil {
		WriteProblem(w, r, Internal(r, err))
		return
	}

	s.writeJSON(w, r, http.StatusOK, LatestReleaseResponse{
		Vendor:        VendorRef{Slug: summary.VendorSlug, Name: summary.VendorName},
		Product:       ProductRef{Slug: summary.ProductSlug, Name: summary.ProductName},
		LatestRelease: PresentRelease(release, nil),
	})
}

// lookupProduct resolves the {slug} path value to a product summary, writing the
// problem document itself when it cannot. ok is false when a response has been sent.
func (s *Server) lookupProduct(w http.ResponseWriter, r *http.Request) (application.ProductSummary, bool) {
	slug := r.PathValue("slug")
	if !domain.ValidSlug(slug) {
		WriteProblem(w, r, InvalidRequest(r, "The product slug must be lowercase kebab-case."))
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

// ---------------------------------------------------------------------------
// releases
// ---------------------------------------------------------------------------

func (s *Server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !releaseIDPattern.MatchString(id) {
		WriteProblem(w, r, InvalidRequest(r, "The release id must look like rel_<identifier>."))
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
	s.writeJSON(w, r, http.StatusOK, PresentRelease(release, nil))
}

// filterReleases applies the exact-match filters. It is a filter over the page the
// repository returned; pushing the predicate into the query is the persistence
// adapter's job, and doing it here as well keeps the endpoint's contract true
// regardless of which adapter is underneath.
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

// writeRepoError maps a repository failure to a problem document. A malformed cursor
// is the caller's mistake, so it is a 400 naming the parameter rather than a 500.
func (s *Server) writeRepoError(w http.ResponseWriter, r *http.Request, err error, param string) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		WriteProblem(w, r, NotFound(r, "The requested resource does not exist."))
	case errors.Is(err, domain.ErrValidation):
		WriteProblem(w, r, InvalidRequest(r, "The '"+param+"' parameter is not a cursor this API issued.").WithCause(err))
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
