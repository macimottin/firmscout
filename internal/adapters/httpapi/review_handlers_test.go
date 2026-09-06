package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// The internal review surface's tests.
//
// Two properties dominate this file and neither is about a happy path: the routes must
// not exist unless both of ADR-0021's switches are on, and a decision must not be
// accepted without an asserted actor. Everything else here is the queue working.

const (
	testReviewItemID  = "rev_01J8Z6P4RSTUVWXYZABCDEF"
	testReviewItemID2 = "rev_01J8Z6P4RSTUVWXYZABCDEG"
	testActor         = "alex@firmscout.dev"
)

// seedReviewQueue puts one conflict-shaped item, its candidate, its evidence, its
// source, the conflict itself and two observations into the fakes behind a
// review-enabled harness, so a detail response has every optional part populated.
func seedReviewQueue(t *testing.T, h *harness) {
	t.Helper()
	f := h.reviewFakes
	if f == nil {
		t.Fatal("the harness was not built with enableReviewAPI")
	}
	now := h.clock.Now()
	created := now.Add(-36 * time.Hour)

	f.products.Add(domain.Product{ID: "prd_routeros", VendorID: "ven_mikrotik", Slug: "mikrotik-routeros", Name: "RouterOS"})
	h.vendors.put(domain.Vendor{ID: "ven_mikrotik", Slug: "mikrotik", Name: "MikroTik"})
	// The compliance fields are part of the fixture, not decoration: a source may only
	// be published from when robots and terms both permit it (ADR-0018), so a source
	// seeded without them is one an accept is required to refuse.
	f.sources.Add(domain.Source{
		ID: "src_changelog", VendorID: "ven_mikrotik", Slug: "mikrotik.routeros-changelog",
		URL: "https://mikrotik.com/download/changelogs", Official: true,
		QualityClass: domain.QualityOfficialManufacturer, Health: domain.SourceActive,
		Enabled:            true,
		RobotsPolicyStatus: domain.RobotsAllowed,
		TermsReviewStatus:  domain.TermsApproved,
	})

	sep, err := domain.NewExactDate(2026, time.September, 2)
	if err != nil {
		t.Fatalf("NewExactDate: %v", err)
	}
	if err := f.evidence.Insert(t.Context(), domain.Evidence{
		ID: "evd_1", SourceID: "src_changelog", RetrievedAt: created,
		Excerpt: "7.24.2 (2026-09-02) -- stable",
	}); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	if _, _, err := f.candidates.UpsertByDedupeKey(t.Context(), domain.CandidateRelease{
		ID: "cnd_1", SourceID: "src_changelog", EvidenceID: "evd_1", ProductID: "prd_routeros",
		ProductMatchHint: "RouterOS", Version: mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS, Applicability: domain.Applicability{Channel: "stable"},
		ReleaseDate: sep, Confidence: 0.9, DedupeKey: "dk_1",
		State: domain.CandidateHumanReviewRequired, DiscoveredAt: created,
	}); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	if err := f.candidates.RecordValidation(t.Context(), "cnd_1", []domain.GateResult{
		{Gate: domain.GateSourceEligible, Order: 1, Outcome: domain.GatePassed, Detail: "source is dispatchable", EvaluatedBy: "pipeline", EvaluatedAt: created},
		{Gate: domain.GateMultiSourceAgree, Order: 10, Outcome: domain.GateReviewRequired, Detail: "other active sources report a different version: 7.24.1", EvaluatedBy: "pipeline", EvaluatedAt: created},
	}); err != nil {
		t.Fatalf("seed gate results: %v", err)
	}

	f.conflicts.SetObservation(domain.SourceObservation{
		SourceID: "src_changelog", ProductID: "prd_routeros", Channel: "stable",
		RawVersion: "7.24.2", NormalizedVersion: "7.24.2", ReleaseDate: sep,
		ObservedAt: created, FirstObservedAt: created,
		QualityClass: domain.QualityOfficialManufacturer, Official: true, Eligible: true,
	})
	f.conflicts.SetObservation(domain.SourceObservation{
		SourceID: "src_portal", ProductID: "prd_routeros", Channel: "stable",
		RawVersion: "7.24.1", NormalizedVersion: "7.24.1", ReleaseDate: domain.UnknownDate,
		ObservedAt: created, FirstObservedAt: created,
		QualityClass: domain.QualityAuthorizedPortal, Official: false, Eligible: true,
	})
	if _, _, err := f.conflicts.UpsertOpenConflict(t.Context(), domain.SourceConflict{
		ID: "cfl_1", ProductID: "prd_routeros", Channel: "stable", State: domain.ConflictOpen,
		AuthorityRank: domain.QualityOfficialManufacturer.Authority(),
		Versions:      []string{"7.24.1", "7.24.2"},
		SourceIDs:     []string{"src_changelog", "src_portal"},
		DetectedAt:    created, LastSeenAt: created,
	}); err != nil {
		t.Fatalf("seed conflict: %v", err)
	}

	f.reviews.Add(application.ReviewItem{
		ID: testReviewItemID, Kind: "multi_source_conflict",
		SubjectType: "candidate_release", SubjectID: "cnd_1",
		VendorID: "ven_mikrotik", ProductID: "prd_routeros",
		Title: "Sources disagree about RouterOS", Detail: "7.24.1 vs 7.24.2",
		Payload: map[string]string{
			application.PayloadKeyConflictID:  "cfl_1",
			application.PayloadKeyFailingGate: string(domain.GateMultiSourceAgree),
		},
		PriorityScore: 310, SLAClass: string(domain.SLAHigh),
		State: application.ReviewStateOpen, CreatedAt: created, UpdatedAt: created,
	})
	f.reviews.Add(application.ReviewItem{
		ID: testReviewItemID2, Kind: "candidate_low_confidence",
		SubjectType: "candidate_release", SubjectID: "cnd_missing",
		Title:         "Low confidence extraction",
		PriorityScore: 140, SLAClass: string(domain.SLALow),
		State: application.ReviewStateOpen, CreatedAt: created.Add(time.Hour), UpdatedAt: created.Add(time.Hour),
	})
}

// postReview issues a decision request against the full chain.
func (h *harness) postReview(target, actor, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if actor != "" {
		r.Header.Set(HeaderReviewActor, actor)
	}
	r.Header.Set(headerContentTypeKey, "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The default build has no internal surface at all. Not a 403, not a 401: the paths do
// not exist, so a probe cannot even confirm the feature is compiled in.
//
// This is the revert detector for ADR-0021's default-off rule. If either switch stops
// being required, one of these paths starts answering something other than the
// catch-all's 404 problem document.
func TestReviewRoutesAbsentByDefault(t *testing.T) {
	t.Parallel()

	targets := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/internal/review/items"},
		{http.MethodGet, "/internal/review/items/" + testReviewItemID},
		{http.MethodPost, "/internal/review/items/" + testReviewItemID + "/accept"},
		{http.MethodPost, "/internal/review/items/" + testReviewItemID + "/reject"},
	}

	// Both switches off.
	plain := newHarness(t)
	// The dependencies present, the flag off. This half is the one an accidental
	// wiring produces, and it must still register nothing.
	wired := newHarness(t, wireReviewAPI)

	for _, h := range map[string]*harness{"no dependencies and no flag": plain, "dependencies wired, flag off": wired} {
		for _, target := range targets {
			w := h.do(target.method, target.path, func(r *http.Request) {
				r.Header.Set(HeaderReviewActor, testActor)
			})
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s: status = %d, want 404 -- the internal surface must not exist by default",
					target.method, target.path, w.Code)
			}
			if ct := w.Header().Get("Content-Type"); ct != ProblemContentType {
				t.Errorf("%s %s: Content-Type = %q, want %q", target.method, target.path, ct, ProblemContentType)
			}
			var p Problem
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatalf("%s %s: unmarshal problem: %v", target.method, target.path, err)
			}
			if p.Type != TypeNotFound {
				t.Errorf("%s %s: type = %q, want %q", target.method, target.path, p.Type, TypeNotFound)
			}
			// The catch-all's detail, not a route-specific one: a probe learns nothing
			// about whether the surface exists elsewhere.
			if !strings.Contains(p.Detail, "No endpoint matches") {
				t.Errorf("%s %s: detail = %q, want the catch-all's", target.method, target.path, p.Detail)
			}
		}
	}
}

func TestReviewQueueListsItems(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)

	w := h.do(http.MethodGet, "/internal/review/items")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got ReviewQueueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body %s", err, w.Body.String())
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, want 2: %s", len(got.Items), w.Body.String())
	}
	// Highest priority first, which is the order review_items_queue_idx serves.
	first := got.Items[0]
	if first.ID != testReviewItemID {
		t.Errorf("first item = %q, want the higher-priority %q", first.ID, testReviewItemID)
	}
	if first.SLAClass != string(domain.SLAHigh) || first.PriorityScore != 310 {
		t.Errorf("priority not rendered: %+v", first)
	}
	if first.Subject.Type != "candidate_release" || first.Subject.ID != "cnd_1" {
		t.Errorf("subject = %+v", first.Subject)
	}
	if first.Vendor == nil || first.Vendor.Slug != "mikrotik" {
		t.Errorf("vendor not resolved: %+v", first.Vendor)
	}
	if first.Product == nil || first.Product.Slug != "mikrotik-routeros" {
		t.Errorf("product not resolved: %+v", first.Product)
	}
	if first.AgeSeconds != int64(36*time.Hour/time.Second) {
		t.Errorf("ageSeconds = %d, want %d", first.AgeSeconds, int64(36*time.Hour/time.Second))
	}
	// An item queued against no vendor renders no vendor member, rather than a
	// {slug:"",name:""} that offers a link resolving to nothing.
	if got.Items[1].Vendor != nil {
		t.Errorf("an item with no vendor rendered one: %+v", got.Items[1].Vendor)
	}
}

// A mistyped filter must be named, not answered with a queue that looks caught up.
func TestReviewQueueRejectsUnknownFilterValues(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)

	for _, target := range []string{
		"/internal/review/items?state=opne",
		"/internal/review/items?kind=multi_source_conflicts",
		"/internal/review/items?sla=urgnet",
		"/internal/review/items?vendor=Not_A_Slug",
		"/internal/review/items?product=Not_A_Slug",
		"/internal/review/items?limit=nope",
	} {
		w := h.do(http.MethodGet, target)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, w.Code)
			continue
		}
		var p Problem
		_ = json.Unmarshal(w.Body.Bytes(), &p)
		if p.Type != TypeInvalidParameter {
			t.Errorf("%s: type = %q, want %q", target, p.Type, TypeInvalidParameter)
		}
	}

	// A well-formed slug naming nothing is different: narrowing to a scope that does
	// not exist is a legitimate way to get zero results.
	w := h.do(http.MethodGet, "/internal/review/items?vendor=nobody")
	if w.Code != http.StatusOK {
		t.Fatalf("unknown vendor slug: status = %d, want 200", w.Code)
	}
	var page ReviewQueueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("items = %d, want an empty page", len(page.Items))
	}
}

func TestReviewQueueFiltersAreForwarded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)

	w := h.do(http.MethodGet, "/internal/review/items?kind=multi_source_conflict&sla=high&state=open")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	var got ReviewQueueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].ID != testReviewItemID {
		t.Fatalf("filters were not applied: %s", w.Body.String())
	}
}

// The detail is what a reviewer decides from. Every part that exists must be there,
// because the alternative is opening a database client.
func TestReviewDetailRendersGatesAndConflict(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)

	w := h.do(http.MethodGet, "/internal/review/items/"+testReviewItemID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got ReviewItemDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body %s", err, w.Body.String())
	}

	if got.Item.ID != testReviewItemID {
		t.Errorf("item id = %q", got.Item.ID)
	}
	if got.Candidate == nil {
		t.Fatal("the candidate under review is missing")
	}
	if got.Candidate.RawVersion != "7.24.2" || got.Candidate.Channel != "stable" {
		t.Errorf("candidate = %+v", got.Candidate)
	}
	if got.Candidate.ReleaseDate != "2026-09-02" || got.Candidate.ReleaseDatePrecision != "exact_day" {
		t.Errorf("candidate date rendered as %q/%q", got.Candidate.ReleaseDate, got.Candidate.ReleaseDatePrecision)
	}

	// The failing gate is the whole reason the item is in the queue. A queue that
	// shows an item needs review but not which check said so is one nobody can act on.
	if len(got.Gates) != 2 {
		t.Fatalf("gates = %d, want 2: %+v", len(got.Gates), got.Gates)
	}
	if got.Gates[1].Gate != string(domain.GateMultiSourceAgree) || got.Gates[1].Outcome != string(domain.GateReviewRequired) {
		t.Errorf("the failing gate is not rendered: %+v", got.Gates[1])
	}
	if got.Gates[1].Order != 10 {
		t.Errorf("gate order = %d, want 10", got.Gates[1].Order)
	}

	if got.Evidence == nil || !strings.Contains(got.Evidence.Excerpt, "7.24.2") {
		t.Errorf("evidence = %+v", got.Evidence)
	}
	if got.Source == nil || got.Source.QualityClass != string(domain.QualityOfficialManufacturer) {
		t.Errorf("source = %+v", got.Source)
	}

	if got.Conflict == nil {
		t.Fatal("the conflict this item belongs to is missing")
	}
	if got.Conflict.ID != "cfl_1" || got.Conflict.State != string(domain.ConflictOpen) {
		t.Errorf("conflict = %+v", got.Conflict)
	}
	if len(got.Conflict.Versions) != 2 {
		t.Errorf("conflict versions = %v, want both participants", got.Conflict.Versions)
	}
	if len(got.Conflict.Observations) != 2 {
		t.Fatalf("observations = %d, want one per source: %+v", len(got.Conflict.Observations), got.Conflict.Observations)
	}
	// The observation whose source published no date renders no date and says so,
	// rather than borrowing a day from anywhere.
	var undated bool
	for _, o := range got.Conflict.Observations {
		if o.RawVersion == "7.24.1" {
			undated = o.ReleaseDate == "" && o.ReleaseDatePrecision == "unknown"
			if !o.Eligible {
				t.Errorf("the disagreeing source should be eligible: %+v", o)
			}
		}
	}
	if !undated {
		t.Error("an observation with no vendor date must render no releaseDate and precision unknown")
	}
}

// A part that is missing is not an error: an item whose candidate was pruned is still a
// decision somebody has to close out.
func TestReviewDetailSurvivesMissingParts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)

	w := h.do(http.MethodGet, "/internal/review/items/"+testReviewItemID2)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	// The optional parts are explicit nulls, not absent keys: a client must be able to
	// tell "pruned" from "this client does not read that field".
	body := w.Body.String()
	for _, key := range []string{`"candidate":null`, `"evidence":null`, `"source":null`, `"conflict":null`} {
		if !strings.Contains(body, key) {
			t.Errorf("missing part not rendered as an explicit null (%s): %s", key, body)
		}
	}
}

func TestReviewItemIDIsValidatedBeforeAnyLookup(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)

	w := h.do(http.MethodGet, "/internal/review/items/not-an-id")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed id: status = %d, want 400", w.Code)
	}
	var p Problem
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.Type != TypeInvalidParameter {
		t.Errorf("type = %q, want %q", p.Type, TypeInvalidParameter)
	}

	w = h.do(http.MethodGet, "/internal/review/items/rev_ZZZZZZZZZZZZZZZZZZZZ")
	if w.Code != http.StatusNotFound {
		t.Fatalf("well-formed unknown id: status = %d, want 404", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.Type != TypeNotFound {
		t.Errorf("type = %q, want %q", p.Type, TypeNotFound)
	}
}

// The actor header is the whole of the identity story, so a decision without one is
// refused before the use case is reached -- an unattributed publication is worse than
// no publication.
func TestAcceptRequiresActorHeader(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)

	for _, actor := range []string{"", "   "} {
		w := h.postReview("/internal/review/items/"+testReviewItemID+"/accept", actor, `{"reason":"verified against the vendor page"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("actor %q: status = %d, want 400; body %s", actor, w.Code, w.Body.String())
		}
		var p Problem
		_ = json.Unmarshal(w.Body.Bytes(), &p)
		// 400, not 401: a 401 obliges a WWW-Authenticate challenge and there is no
		// scheme to name. The header is not a credential.
		if p.Type != TypeInvalidParameter {
			t.Errorf("actor %q: type = %q, want %q", actor, p.Type, TypeInvalidParameter)
		}
		if !strings.Contains(p.Detail, HeaderReviewActor) {
			t.Errorf("the detail must name the header: %q", p.Detail)
		}
		if !strings.Contains(p.Detail, "not a credential") {
			t.Errorf("the detail must say the header is not a credential: %q", p.Detail)
		}
	}

	// Nothing was decided, nothing was published, nothing was written to the audit
	// trail. The use case was never reached.
	if items := h.reviewFakes.reviews.Items; items[0].State != application.ReviewStateOpen {
		t.Errorf("the item was decided without an actor: state = %q", items[0].State)
	}
	if events := h.reviewFakes.audit.Events(); len(events) != 0 {
		t.Errorf("an audit row was written without an actor: %+v", events)
	}
	if n := h.reviewFakes.releases.Count(); n != 0 {
		t.Errorf("a release was published without an actor: %d", n)
	}
}

// A name that is recorded verbatim in an audit row and printed by whatever reads it
// later must not carry a control character.
func TestAcceptRejectsAnUnusableActorName(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)

	for _, actor := range []string{
		"alex\a approved",
		strings.Repeat("x", maxReviewActorLength+1),
	} {
		w := h.postReview("/internal/review/items/"+testReviewItemID+"/accept", actor, `{"reason":"looks right"}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("actor %q: status = %d, want 400", actor, w.Code)
		}
	}
}

func TestReviewDecisionBodyIsValidated(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"empty body":       ``,
		"not an object":    `"just a string"`,
		"blank reason":     `{"reason":"   "}`,
		"missing reason":   `{}`,
		"unknown field":    `{"resaon":"typo"}`,
		"two objects":      `{"reason":"one"}{"reason":"two"}`,
		"oversized reason": `{"reason":"` + strings.Repeat("x", maxReviewDecisionBody) + `"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, enableReviewAPI)
			seedReviewQueue(t, h)

			w := h.postReview("/internal/review/items/"+testReviewItemID+"/accept", testActor, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
			}
			var p Problem
			_ = json.Unmarshal(w.Body.Bytes(), &p)
			// validation-failed, not invalid-parameter: the fault is in the body, and
			// telling a caller to check their query string would send them nowhere.
			if p.Type != TypeValidationFailed {
				t.Errorf("type = %q, want %q", p.Type, TypeValidationFailed)
			}
			if h.reviewFakes.reviews.Items[0].State != application.ReviewStateOpen {
				t.Error("the item was decided from an invalid body")
			}
		})
	}
}

func TestAcceptPublishesAndRecordsAnUnverifiedActor(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)

	w := h.postReview("/internal/review/items/"+testReviewItemID+"/accept", testActor,
		`{"reason":"confirmed 7.24.2 on the vendor changelog"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got ReviewDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body %s", err, w.Body.String())
	}
	if got.ItemID != testReviewItemID || got.Decision != application.ReviewDecisionAccepted {
		t.Errorf("decision = %+v", got)
	}
	if got.Actor != testActor {
		t.Errorf("actor = %q, want the asserted name echoed verbatim", got.Actor)
	}
	// The response must say the name is unverified. A client rendering an audit trail
	// must not be able to present an asserted name as a verified one.
	if got.ActorAuthenticated {
		t.Error("actorAuthenticated is true; the platform verified nothing (ADR-0021)")
	}
	if !strings.Contains(w.Body.String(), `"actorAuthenticated":false`) {
		t.Errorf("actorAuthenticated must be present, not omitted when false: %s", w.Body.String())
	}
	if !got.Published || got.ReleaseID == "" {
		t.Errorf("accepting must publish the candidate: %+v", got)
	}
	if got.ConflictID != "cfl_1" {
		t.Errorf("conflictId = %q, want the conflict the item covers", got.ConflictID)
	}

	// And the same facts landed in the audit trail, unverified.
	events := h.reviewFakes.audit.Events()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1: %+v", len(events), events)
	}
	e := events[0]
	if e.ActorID != testActor || e.ActorAuthenticated {
		t.Errorf("audit row = %+v, want the asserted actor recorded unverified", e)
	}
	if e.Action != application.AuditActionReviewAccepted {
		t.Errorf("audit action = %q", e.Action)
	}
	if !strings.Contains(e.Reason, "7.24.2") {
		t.Errorf("the reason was not recorded: %q", e.Reason)
	}
	if e.RequestID == "" {
		t.Error("the audit row must carry the request id that produced it")
	}
}

func TestRejectMovesTheCandidateToRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)

	w := h.postReview("/internal/review/items/"+testReviewItemID+"/reject", testActor,
		`{"reason":"the portal misread a release note"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got ReviewDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Decision != application.ReviewDecisionRejected || got.Published {
		t.Errorf("decision = %+v, want a rejection that published nothing", got)
	}
	cand, err := h.reviewFakes.candidates.GetByID(t.Context(), "cnd_1")
	if err != nil {
		t.Fatalf("load candidate: %v", err)
	}
	if cand.State != domain.CandidateRejected {
		t.Errorf("candidate state = %q, want rejected", cand.State)
	}
	if n := h.reviewFakes.releases.Count(); n != 0 {
		t.Errorf("a rejection published %d releases", n)
	}
}

// Two reviewers racing produce one decision and one visible failure, and the failure
// says what actually happened rather than blaming the payload.
func TestAcceptReturnsConflictForDecidedItem(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)

	first := h.postReview("/internal/review/items/"+testReviewItemID+"/accept", testActor, `{"reason":"first"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first decision: status = %d; body %s", first.Code, first.Body.String())
	}

	second := h.postReview("/internal/review/items/"+testReviewItemID+"/reject", "sam@firmscout.dev", `{"reason":"second"}`)
	if second.Code != http.StatusConflict {
		t.Fatalf("second decision: status = %d, want 409; body %s", second.Code, second.Body.String())
	}
	var p Problem
	if err := json.Unmarshal(second.Body.Bytes(), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ct := second.Header().Get("Content-Type"); ct != ProblemContentType {
		t.Errorf("Content-Type = %q, want %q", ct, ProblemContentType)
	}
	if p.Type != TypeConflict {
		t.Errorf("type = %q, want %q", p.Type, TypeConflict)
	}
	if p.Status != http.StatusConflict {
		t.Errorf("status member = %d, want 409", p.Status)
	}
	// Exactly one decision was recorded.
	if events := h.reviewFakes.audit.Events(); len(events) != 1 {
		t.Errorf("audit events = %d, want 1 -- the losing decision must write nothing", len(events))
	}
}

// A moderation queue must not be stored by anything between here and the reviewer.
func TestReviewResponsesAreNoStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)

	responses := []*httptest.ResponseRecorder{
		h.do(http.MethodGet, "/internal/review/items"),
		h.do(http.MethodGet, "/internal/review/items/"+testReviewItemID),
		h.postReview("/internal/review/items/"+testReviewItemID+"/accept", testActor, `{"reason":"ok"}`),
	}
	for i, w := range responses {
		if cc := w.Header().Get(HeaderCacheControl); cc != "no-store" {
			t.Errorf("response %d: Cache-Control = %q, want no-store", i, cc)
		}
		if etag := w.Header().Get(HeaderETag); etag != "" {
			t.Errorf("response %d: an ETag was issued for the review surface: %q", i, etag)
		}
		// And no Vary, because nothing here is cacheable for a Vary to partition.
		if v := w.Header().Get(HeaderVary); v != "" {
			t.Errorf("response %d: Vary = %q on an uncacheable response", i, v)
		}
	}
}

// The internal surface is unmetered. An internal decision must not consume a
// customer's monthly allowance, and there is no consumer to attribute it to anyway.
func TestReviewRoutesAreNotMetered(t *testing.T) {
	t.Parallel()
	h := newHarness(t, enableReviewAPI)
	seedReviewQueue(t, h)
	h.apiKeys.add(hashKey("k"), "key_1", consumerFixture())

	w := h.do(http.MethodGet, "/internal/review/items", func(r *http.Request) {
		r.Header.Set(HeaderAuthorization, "Bearer k")
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if records := h.usage.recorded(); len(records) != 0 {
		t.Errorf("the review surface was metered: %+v", records)
	}
	if q := w.Header().Get(HeaderQuotaLimit); q != "" {
		t.Errorf("X-Quota-Limit = %q on an unmetered route", q)
	}
}

// An unauthenticated endpoint that publishes releases is worth bounding even when it is
// only meant to be reachable from a private network. RateLimit is the one middleware
// the internal chain keeps, and this is what says so.
func TestReviewRoutesAreRateLimited(t *testing.T) {
	t.Parallel()
	const burst = 2
	h := newHarness(t, enableReviewAPI, func(d *Deps, _ *harness) {
		d.RateLimits = RateLimitConfig{
			Anonymous: map[EndpointClass]Policy{
				ClassList:   {Burst: burst, PerMinute: burst},
				ClassDetail: {Burst: burst, PerMinute: burst},
			},
		}
	})
	seedReviewQueue(t, h)

	var last int
	for i := 0; i < burst+1; i++ {
		w := h.do(http.MethodGet, "/internal/review/items")
		last = w.Code
		if i < burst && w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200; body %s", i, w.Code, w.Body.String())
		}
		if i == burst {
			var p Problem
			_ = json.Unmarshal(w.Body.Bytes(), &p)
			if p.Type != TypeRateLimited {
				t.Errorf("type = %q, want %q", p.Type, TypeRateLimited)
			}
			if w.Header().Get(HeaderRetryAfter) == "" {
				t.Error("a 429 must say when to retry")
			}
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("the burst was never exhausted: last status = %d", last)
	}
}

// The revert detector for the queue's error mapping.
//
// domain.ErrValidation reaches this handler from two places -- an unrecognised state,
// kind or SLA class, and a cursor the API did not issue -- and they need different
// sentences. Mapping both to "check 'state', 'kind' and 'sla'" sent a reviewer whose
// cursor had been truncated to fix three parameters that were correct, with no way to
// page past it. The public endpoints have always got this right via writeRepoError.
func TestQueueValidationErrorsNameTheParameterTheCallerCanFix(t *testing.T) {
	t.Parallel()

	t.Run("a cursor this API did not issue", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, enableReviewAPIRejectingCursors)

		w := h.do(http.MethodGet, "/internal/review/items?cursor=not-a-cursor")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
		}
		var p Problem
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.Type != TypeInvalidParameter {
			t.Errorf("type = %q, want %q", p.Type, TypeInvalidParameter)
		}
		if !strings.Contains(p.Detail, "cursor") {
			t.Errorf("detail = %q, want it to name the cursor", p.Detail)
		}
		if strings.Contains(p.Detail, "'state'") {
			t.Errorf("detail = %q sends the caller to fix filters they did not send", p.Detail)
		}
	})

	t.Run("an unrecognised filter value", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, enableReviewAPI)
		seedReviewQueue(t, h)

		w := h.do(http.MethodGet, "/internal/review/items?state=nonsense")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
		}
		var p Problem
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !strings.Contains(p.Detail, "'state'") {
			t.Errorf("detail = %q, want it to name the filters", p.Detail)
		}
		if strings.Contains(p.Detail, "cursor") {
			t.Errorf("detail = %q mentions a cursor the caller did not send", p.Detail)
		}
	})

	t.Run("both, which is genuinely ambiguous", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, enableReviewAPIRejectingCursors)

		w := h.do(http.MethodGet, "/internal/review/items?sla=high&cursor=not-a-cursor")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
		}
		var p Problem
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// Both are named, because either could be the mistake and guessing would send
		// the caller to the wrong one half the time.
		if !strings.Contains(p.Detail, "cursor") || !strings.Contains(p.Detail, "'sla'") {
			t.Errorf("detail = %q, want both candidates named", p.Detail)
		}
	})
}
