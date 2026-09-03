package application_test

import (
	"context"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/application/apptest"
	"github.com/macimottin/firmscout/internal/domain"
)

// fixture assembles a complete in-memory world: one vendor, one product, one compliant
// source, and the fakes for every port. Every test in this file starts here, so the
// setup cost of testing the whole pipeline is one function call and no I/O.
type fixture struct {
	clock      *apptest.Clock
	ids        *apptest.IDGen
	sources    *apptest.Sources
	products   *apptest.Products
	candidates *apptest.Candidates
	releases   *apptest.Releases
	evidence   *apptest.EvidenceStore
	reviews    *apptest.Reviews
	queue      *apptest.Queue
	artifacts  *apptest.Artifacts
	events     *apptest.Events
	uow        *apptest.UnitOfWork
	fetcher    *apptest.Fetcher
	normalizer *apptest.Normalizer
	collector  *apptest.Collector

	source  domain.Source
	product domain.Product
}

const (
	testNow      = "2026-09-03T12:00:00Z"
	sourceID     = "src_mikrotik_changelogs"
	productID    = "prd_mikrotik_routeros"
	vendorID     = "ven_mikrotik"
	changedBody  = "<div class=\"changelog-header\">7.24.3</div>"
	originalBody = "<div class=\"changelog-header\">7.24.2</div>"
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	now, err := time.Parse(time.RFC3339, testNow)
	if err != nil {
		t.Fatalf("parse test time: %v", err)
	}

	f := &fixture{
		clock:      apptest.NewClock(now),
		ids:        apptest.NewIDGen(),
		sources:    apptest.NewSources(),
		products:   apptest.NewProducts(),
		candidates: apptest.NewCandidates(),
		releases:   apptest.NewReleases(),
		evidence:   apptest.NewEvidenceStore(),
		reviews:    &apptest.Reviews{},
		queue:      apptest.NewQueue(),
		artifacts:  apptest.NewArtifacts(),
		events:     &apptest.Events{},
		uow:        &apptest.UnitOfWork{},
		fetcher:    &apptest.Fetcher{},
		normalizer: &apptest.Normalizer{},
	}

	f.product = domain.Product{
		ID:                 productID,
		VendorID:           vendorID,
		Slug:               "mikrotik-routeros",
		Name:               "RouterOS",
		DefaultReleaseType: domain.ReleaseTypeEmbeddedOS,
		LifecycleStatus:    domain.LifecycleActive,
	}
	f.products.Add(f.product)
	f.products.AddAlias(productID, "RouterOS")

	f.source = domain.Source{
		ID:                    sourceID,
		VendorID:              vendorID,
		ProductID:             productID,
		Slug:                  "changelogs",
		SourceType:            domain.SourceTypeHTMLPage,
		URL:                   "https://mikrotik.com/download/changelogs",
		Official:              true,
		QualityClass:          domain.QualityOfficialManufacturer,
		RobotsPolicyStatus:    domain.RobotsAllowed,
		TermsReviewStatus:     domain.TermsApproved,
		AuthenticationType:    domain.AuthNone,
		Enabled:               true,
		CollectorID:           "mikrotik.changelogs",
		CheckFrequencySeconds: 21600,
		Health:                domain.SourceActive,
		Confidence:            0.9,
		NextCheckAt:           now.Add(-time.Minute),
		Normalize:             domain.NormalizeConfig{SectionSelector: "div.changelog-header"},
	}
	f.sources.Add(f.source)
	f.sources.SetProducts(sourceID, []domain.Product{f.product})

	return f
}

func (f *fixture) checkSource() *application.CheckSource {
	return application.NewCheckSource(application.CheckSourceDeps{
		Sources:   f.sources,
		Fetcher:   f.fetcher,
		Normalize: f.normalizer,
		Artifacts: f.artifacts,
		Queue:     f.queue,
		Events:    f.events,
		Clock:     f.clock,
		IDs:       f.ids,
		UserAgent: "FirmScoutTest/0.0",
	})
}

func (f *fixture) ingestDeps() application.IngestDeps {
	return application.IngestDeps{
		Sources:    f.sources,
		Products:   f.products,
		Candidates: f.candidates,
		Releases:   f.releases,
		Evidence:   f.evidence,
		Reviews:    f.reviews,
		Artifacts:  f.artifacts,
		Registry:   &apptest.Registry{C: f.collector},
		Queue:      f.queue,
		Events:     f.events,
		UoW:        f.uow,
		Clock:      f.clock,
		IDs:        f.ids,
	}
}

func mustVersion(t *testing.T, s string) domain.VersionString {
	t.Helper()
	v, err := domain.NewVersionString(s)
	if err != nil {
		t.Fatalf("NewVersionString(%q): %v", s, err)
	}
	return v
}

func mustDate(t *testing.T, s string) domain.PartialDate {
	t.Helper()
	d, err := domain.ParsePartialDate(s)
	if err != nil {
		t.Fatalf("ParsePartialDate(%q): %v", s, err)
	}
	return d
}

// candidateFor builds the candidate a collector would return for a version.
func (f *fixture) candidateFor(t *testing.T, version, date string) domain.CandidateRelease {
	t.Helper()
	v := mustVersion(t, version)
	app := domain.Applicability{Channel: "stable"}
	return domain.CandidateRelease{
		ProductID:          productID,
		ProductMatchHint:   "RouterOS",
		ProductMatchStatus: domain.MatchUnique,
		Version:            v,
		ReleaseType:        domain.ReleaseTypeEmbeddedOS,
		Applicability:      app,
		ReleaseDate:        mustDate(t, date),
		Confidence:         0.95,
		DedupeKey:          domain.ComputeDedupeKey("RouterOS", v, app),
		State:              domain.CandidateDiscovered,
	}
}

// ---------------------------------------------------------------------------
// CheckSource
// ---------------------------------------------------------------------------

func TestCheckSourceUnchangedIsTheCheapPath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fetcher.Responses = []application.FetchResult{{
		Outcome:      domain.OutcomeUnchanged,
		ChangeSignal: domain.SignalETag,
		StatusCode:   304,
	}}

	res, err := f.checkSource().Execute(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != domain.OutcomeUnchanged {
		t.Errorf("outcome = %q, want unchanged", res.Outcome)
	}
	if res.ExtractionEnqueued {
		t.Error("extraction was enqueued for an unchanged source")
	}
	if f.normalizer.Calls != 0 {
		t.Errorf("normaliser ran %d times on a 304; the cheap path must do no parsing", f.normalizer.Calls)
	}
	if f.artifacts.StoredCount() != 0 {
		t.Error("an artifact was stored for an unchanged source")
	}
	if f.events.Count(domain.EventSourceUnchanged) != 1 {
		t.Error("no SourceUnchanged event published")
	}
}

func TestCheckSourceSendsConditionalHeaders(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	src := f.source
	src.ETag = `"abc123"`
	src.LastModifiedValue = "Wed, 02 Sep 2026 10:00:00 GMT"
	f.sources.Add(src)
	f.fetcher.Responses = []application.FetchResult{{Outcome: domain.OutcomeUnchanged, StatusCode: 304}}

	if _, err := f.checkSource().Execute(context.Background(), sourceID); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(f.fetcher.Requests) != 1 {
		t.Fatalf("made %d requests, want 1", len(f.fetcher.Requests))
	}
	req := f.fetcher.Requests[0]
	if req.ETag != `"abc123"` {
		t.Errorf("ETag not sent: %q", req.ETag)
	}
	if req.LastModified == "" {
		t.Error("Last-Modified not sent")
	}
	if !req.RespectRobots {
		t.Error("robots policy was not requested; collection must always respect it")
	}
}

// The MikroTik changelog page has no ETag and changes its furniture constantly. A
// changed body whose monitored section is identical must NOT trigger extraction.
func TestCheckSourceIgnoresChangesOutsideTheMonitoredSection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	src := f.source
	// The hash the source already holds is the hash of the section content.
	f.normalizer.Sections = map[string]string{
		"page-v1 " + originalBody: "SECTION-STABLE",
		"page-v2 " + originalBody: "SECTION-STABLE",
	}
	_, sectionHash, err := f.normalizer.Normalize("text/html", []byte("page-v1 "+originalBody), "div.changelog-header", nil)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	src.NormalizedContentHash = sectionHash
	f.sources.Add(src)

	f.fetcher.Responses = []application.FetchResult{{
		Outcome:     domain.OutcomeChanged,
		StatusCode:  200,
		ContentType: "text/html",
		Body:        []byte("page-v2 " + originalBody), // different page, same section
	}}

	res, err := f.checkSource().Execute(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != domain.OutcomeUnchanged {
		t.Fatalf("outcome = %q, want unchanged; a rotating banner must not look like a release", res.Outcome)
	}
	if res.ExtractionEnqueued {
		t.Error("extraction ran for a page whose monitored section did not change")
	}
}

func TestCheckSourceChangedEnqueuesExtraction(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fetcher.Responses = []application.FetchResult{{
		Outcome:     domain.OutcomeChanged,
		StatusCode:  200,
		ContentType: "text/html",
		ETag:        `"new"`,
		Body:        []byte(changedBody),
	}}

	res, err := f.checkSource().Execute(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != domain.OutcomeChanged {
		t.Fatalf("outcome = %q, want changed", res.Outcome)
	}
	if !res.ExtractionEnqueued {
		t.Fatal("extraction was not enqueued for a changed source")
	}
	if got := f.queue.JobsOfKind(application.JobExtractCandidates); len(got) != 1 {
		t.Fatalf("enqueued %d extraction jobs, want 1", len(got))
	}
	if f.artifacts.StoredCount() != 1 {
		t.Errorf("stored %d artifacts, want 1", f.artifacts.StoredCount())
	}
	stored, err := f.sources.GetByID(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("reload source: %v", err)
	}
	if stored.ETag != `"new"` {
		t.Errorf("ETag not persisted: %q", stored.ETag)
	}
	if stored.NextCheckAt.IsZero() || !stored.NextCheckAt.After(f.clock.Now()) {
		t.Error("next check time was not scheduled into the future")
	}
}

// The compliance gate is re-checked here, not only at dispatch, because a source's
// status can change between the scheduler's query and the check.
func TestCheckSourceRefusesWhenComplianceForbidsIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	src := f.source
	src.RobotsPolicyStatus = domain.RobotsDisallowed
	f.sources.Add(src)

	_, err := f.checkSource().Execute(context.Background(), sourceID)
	if err == nil {
		t.Fatal("a robots-disallowed source was checked")
	}
	if len(f.fetcher.Requests) != 0 {
		t.Error("an HTTP request was made against a disallowed source")
	}
}

func TestCheckSourceFailureDegradesThenBreaks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.fetcher.Responses = []application.FetchResult{{
		Outcome: domain.OutcomeUnavailable, StatusCode: 503,
	}}

	uc := f.checkSource()
	for i := 1; i <= 3; i++ {
		if _, err := uc.Execute(context.Background(), sourceID); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
		f.clock.Advance(time.Hour)
	}
	src, err := f.sources.GetByID(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if src.Health != domain.SourceBroken {
		t.Errorf("health = %q after three failures, want broken", src.Health)
	}
	if src.ConsecutiveFailures != 3 {
		t.Errorf("consecutive failures = %d, want 3", src.ConsecutiveFailures)
	}
	if f.events.Count(domain.EventSourceBroken) == 0 {
		t.Error("no SourceBroken event published")
	}
}

// ---------------------------------------------------------------------------
// Extraction
// ---------------------------------------------------------------------------

func TestExtractCreatesCandidatesWithEvidence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.collector = &apptest.Collector{
		CollectorID: "mikrotik.changelogs",
		Ver:         "1",
		VendorSlug:  "mikrotik",
		Candidates: []domain.CandidateRelease{
			f.candidateFor(t, "7.24.3", "2026-09-02"),
			f.candidateFor(t, "7.24.2", "2026-08-15"),
		},
	}
	artID, _, err := f.artifacts.Put(context.Background(), "hash1", "text/html", []byte(changedBody))
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	res, err := application.NewExtractCandidates(f.ingestDeps()).Execute(context.Background(), sourceID, artID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.NewCandidates != 2 {
		t.Errorf("new candidates = %d, want 2", res.NewCandidates)
	}
	if f.evidence.Count() != 2 {
		t.Errorf("evidence rows = %d, want 2; every candidate must carry provenance", f.evidence.Count())
	}
	if got := len(f.queue.JobsOfKind(application.JobValidateCandidate)); got != 2 {
		t.Errorf("enqueued %d validation jobs, want 2", got)
	}
}

// Re-running extraction on the same artifact must not duplicate candidates, which is
// what makes the queue's at-least-once delivery acceptable.
func TestExtractIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.collector = &apptest.Collector{
		CollectorID: "mikrotik.changelogs", Ver: "1",
		Candidates: []domain.CandidateRelease{f.candidateFor(t, "7.24.3", "2026-09-02")},
	}
	artID, _, _ := f.artifacts.Put(context.Background(), "hash1", "text/html", []byte(changedBody))
	uc := application.NewExtractCandidates(f.ingestDeps())

	first, err := uc.Execute(context.Background(), sourceID, artID)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := uc.Execute(context.Background(), sourceID, artID)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if first.NewCandidates != 1 {
		t.Errorf("first run created %d candidates, want 1", first.NewCandidates)
	}
	if second.NewCandidates != 0 {
		t.Errorf("second run created %d candidates, want 0", second.NewCandidates)
	}
	if second.Duplicates != 1 {
		t.Errorf("second run reported %d duplicates, want 1", second.Duplicates)
	}
}

// ---------------------------------------------------------------------------
// Full pipeline
// ---------------------------------------------------------------------------

func TestFullPipelinePublishesARelease(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.collector = &apptest.Collector{
		CollectorID: "mikrotik.changelogs", Ver: "1",
		Candidates: []domain.CandidateRelease{f.candidateFor(t, "7.24.3", "2026-09-02")},
	}
	f.fetcher.Responses = []application.FetchResult{{
		Outcome: domain.OutcomeChanged, StatusCode: 200,
		ContentType: "text/html", Body: []byte(changedBody),
	}}
	ctx := context.Background()

	// 1. Check.
	checkRes, err := f.checkSource().Execute(ctx, sourceID)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !checkRes.ExtractionEnqueued {
		t.Fatal("check did not enqueue extraction")
	}

	// 2. Extract.
	deps := f.ingestDeps()
	extractRes, err := application.NewExtractCandidates(deps).Execute(ctx, sourceID, checkRes.ArtifactID)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(extractRes.CandidateIDs) != 1 {
		t.Fatalf("extracted %d candidates, want 1", len(extractRes.CandidateIDs))
	}
	candidateID := extractRes.CandidateIDs[0]

	// 3. Validate.
	valRes, err := application.NewValidateCandidate(deps).Execute(ctx, candidateID)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if valRes.Decision != domain.GatePassed {
		t.Fatalf("validation decision = %q (%s), want passed", valRes.Decision, valRes.Reason)
	}
	if len(f.candidates.Validations[candidateID]) != len(domain.GateOrder()) {
		t.Errorf("recorded %d gate results, want %d",
			len(f.candidates.Validations[candidateID]), len(domain.GateOrder()))
	}

	// 4. Publish.
	pubRes, err := application.NewPublishRelease(deps).Execute(ctx, candidateID)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !pubRes.Published {
		t.Fatalf("release not published: %s", pubRes.Reason)
	}
	if f.releases.Count() != 1 {
		t.Fatalf("stored %d releases, want 1", f.releases.Count())
	}

	rel, err := f.releases.GetByID(ctx, pubRes.ReleaseID)
	if err != nil {
		t.Fatalf("load release: %v", err)
	}
	if rel.Version.Raw() != "7.24.3" {
		t.Errorf("version = %q, want 7.24.3", rel.Version.Raw())
	}
	if rel.ReleaseType != domain.ReleaseTypeEmbeddedOS {
		t.Errorf("release type = %q; RouterOS is an embedded OS, not firmware", rel.ReleaseType)
	}
	if rel.ReleaseDate.String() != "2026-09-02" {
		t.Errorf("release date = %q, want 2026-09-02", rel.ReleaseDate.String())
	}
	if rel.EvidenceID == "" {
		t.Error("published release carries no evidence")
	}
	if rel.Recommended != nil {
		t.Error("recommended was set without the vendor saying so")
	}

	// The whole publication must be one transaction.
	if f.uow.Transactions != 1 {
		t.Errorf("publication opened %d transactions, want 1", f.uow.Transactions)
	}
	if len(f.releases.SummaryRefresh) != 1 {
		t.Errorf("product summary refreshed %d times, want 1", len(f.releases.SummaryRefresh))
	}
	// The first release for a product is the latest observed.
	mappings := f.releases.Mappings()
	if len(mappings) != 1 || !mappings[0].IsLatestObserved {
		t.Error("first release for a product was not flagged latest observed")
	}
	if f.events.Count(domain.EventReleasePublished) != 1 {
		t.Error("no ReleasePublished event")
	}
}

func TestPublishIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.collector = &apptest.Collector{
		CollectorID: "c", Ver: "1",
		Candidates: []domain.CandidateRelease{f.candidateFor(t, "7.24.3", "2026-09-02")},
	}
	ctx := context.Background()
	deps := f.ingestDeps()
	artID, _, _ := f.artifacts.Put(ctx, "h", "text/html", []byte(changedBody))
	ex, err := application.NewExtractCandidates(deps).Execute(ctx, sourceID, artID)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	id := ex.CandidateIDs[0]
	if _, err := application.NewValidateCandidate(deps).Execute(ctx, id); err != nil {
		t.Fatalf("validate: %v", err)
	}

	pub := application.NewPublishRelease(deps)
	first, err := pub.Execute(ctx, id)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	second, err := pub.Execute(ctx, id)
	if err != nil {
		t.Fatalf("second publish returned an error instead of being a no-op: %v", err)
	}
	if second.Published {
		t.Error("the same candidate was published twice")
	}
	if second.ReleaseID != first.ReleaseID {
		t.Errorf("second publish reported a different release: %q vs %q", second.ReleaseID, first.ReleaseID)
	}
	if f.releases.Count() != 1 {
		t.Errorf("stored %d releases after a duplicate publish, want 1", f.releases.Count())
	}
}

// A duplicate candidate is not an error. The right response is to confirm the existing
// release is still current rather than publish a second identical row.
func TestDuplicateCandidateRefreshesVerificationInsteadOfPublishing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.collector = &apptest.Collector{
		CollectorID: "c", Ver: "1",
		Candidates: []domain.CandidateRelease{f.candidateFor(t, "7.24.3", "2026-09-02")},
	}
	ctx := context.Background()
	deps := f.ingestDeps()

	// Publish once.
	art1, _, _ := f.artifacts.Put(ctx, "h1", "text/html", []byte(changedBody))
	ex1, _ := application.NewExtractCandidates(deps).Execute(ctx, sourceID, art1)
	if _, err := application.NewValidateCandidate(deps).Execute(ctx, ex1.CandidateIDs[0]); err != nil {
		t.Fatalf("validate: %v", err)
	}
	pub, err := application.NewPublishRelease(deps).Execute(ctx, ex1.CandidateIDs[0])
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// A second source reports the same version. Use a distinct source so the
	// candidate is not deduplicated before validation.
	other := f.source
	other.ID = "src_other"
	other.Slug = "other"
	f.sources.Add(other)
	f.clock.Advance(24 * time.Hour)

	art2, _, _ := f.artifacts.Put(ctx, "h2", "text/html", []byte(changedBody))
	ex2, err := application.NewExtractCandidates(deps).Execute(ctx, "src_other", art2)
	if err != nil {
		t.Fatalf("second extract: %v", err)
	}
	val, err := application.NewValidateCandidate(deps).Execute(ctx, ex2.CandidateIDs[0])
	if err != nil {
		t.Fatalf("second validate: %v", err)
	}
	if val.Decision != domain.GateRejected {
		t.Errorf("duplicate decision = %q, want rejected", val.Decision)
	}
	if _, ok := f.releases.Verified[pub.ReleaseID]; !ok {
		t.Error("the existing release's verification timestamp was not refreshed")
	}
	if f.releases.Count() != 1 {
		t.Errorf("stored %d releases, want 1", f.releases.Count())
	}
}

func TestLowConfidenceRoutesToReviewNotPublication(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	c := f.candidateFor(t, "7.24.3", "2026-09-02")
	c.Confidence = 0.30
	f.collector = &apptest.Collector{CollectorID: "c", Ver: "1", Candidates: []domain.CandidateRelease{c}}

	ctx := context.Background()
	deps := f.ingestDeps()
	artID, _, _ := f.artifacts.Put(ctx, "h", "text/html", []byte(changedBody))
	ex, _ := application.NewExtractCandidates(deps).Execute(ctx, sourceID, artID)

	val, err := application.NewValidateCandidate(deps).Execute(ctx, ex.CandidateIDs[0])
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if val.Decision != domain.GateReviewRequired {
		t.Fatalf("decision = %q, want review_required", val.Decision)
	}
	if val.ReviewItemID == "" {
		t.Error("no review item created")
	}
	if len(f.reviews.Items) != 1 {
		t.Fatalf("review queue has %d items, want 1", len(f.reviews.Items))
	}
	if f.reviews.Items[0].Detail == "" {
		t.Error("review item carries no explanation")
	}
	if len(f.queue.JobsOfKind(application.JobPublishRelease)) != 0 {
		t.Error("publication was enqueued for a candidate needing review")
	}
	if f.releases.Count() != 0 {
		t.Error("a release was published without review")
	}
}

func TestImplausibleVersionTransitionRoutesToReview(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	// Publish 7.24.3 first.
	f.collector = &apptest.Collector{CollectorID: "c", Ver: "1",
		Candidates: []domain.CandidateRelease{f.candidateFor(t, "7.24.3", "2026-09-02")}}
	deps := f.ingestDeps()
	art1, _, _ := f.artifacts.Put(ctx, "h1", "text/html", []byte("a"))
	ex1, _ := application.NewExtractCandidates(deps).Execute(ctx, sourceID, art1)
	if _, err := application.NewValidateCandidate(deps).Execute(ctx, ex1.CandidateIDs[0]); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err := application.NewPublishRelease(deps).Execute(ctx, ex1.CandidateIDs[0]); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Now a wildly different version appears.
	f.collector = &apptest.Collector{CollectorID: "c", Ver: "1",
		Candidates: []domain.CandidateRelease{f.candidateFor(t, "1.0.0", "2026-09-03")}}
	deps = f.ingestDeps()
	art2, _, _ := f.artifacts.Put(ctx, "h2", "text/html", []byte("b"))
	ex2, err := application.NewExtractCandidates(deps).Execute(ctx, sourceID, art2)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	val, err := application.NewValidateCandidate(deps).Execute(ctx, ex2.CandidateIDs[0])
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if val.Decision != domain.GateReviewRequired {
		t.Fatalf("decision = %q, want review_required for a 7.24.3 to 1.0.0 transition", val.Decision)
	}
}

// A month-precision date must survive the whole pipeline without gaining a day.
func TestMonthPrecisionSurvivesPublication(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.collector = &apptest.Collector{CollectorID: "c", Ver: "1",
		Candidates: []domain.CandidateRelease{f.candidateFor(t, "7.24.3", "2026-02")}}

	ctx := context.Background()
	deps := f.ingestDeps()
	artID, _, _ := f.artifacts.Put(ctx, "h", "text/html", []byte(changedBody))
	ex, _ := application.NewExtractCandidates(deps).Execute(ctx, sourceID, artID)
	if _, err := application.NewValidateCandidate(deps).Execute(ctx, ex.CandidateIDs[0]); err != nil {
		t.Fatalf("validate: %v", err)
	}
	pub, err := application.NewPublishRelease(deps).Execute(ctx, ex.CandidateIDs[0])
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	rel, err := f.releases.GetByID(ctx, pub.ReleaseID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rel.ReleaseDate.Precision() != domain.PrecisionMonthOnly {
		t.Errorf("precision = %q, want month_only", rel.ReleaseDate.Precision())
	}
	if rel.ReleaseDate.String() != "2026-02" {
		t.Errorf("date rendered as %q, want 2026-02", rel.ReleaseDate.String())
	}
	if _, ok := rel.ReleaseDate.ExactDay(); ok {
		t.Fatal("a day was invented during publication")
	}
}

// A newer release takes the latest-observed flag; the older one keeps its row.
func TestLatestObservedFlagMovesWithoutRewritingHistory(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	publish := func(version, date string, hash string) string {
		t.Helper()
		f.collector = &apptest.Collector{CollectorID: "c", Ver: "1",
			Candidates: []domain.CandidateRelease{f.candidateFor(t, version, date)}}
		deps := f.ingestDeps()
		art, _, _ := f.artifacts.Put(ctx, hash, "text/html", []byte(hash))
		ex, err := application.NewExtractCandidates(deps).Execute(ctx, sourceID, art)
		if err != nil {
			t.Fatalf("extract %s: %v", version, err)
		}
		if _, err := application.NewValidateCandidate(deps).Execute(ctx, ex.CandidateIDs[0]); err != nil {
			t.Fatalf("validate %s: %v", version, err)
		}
		res, err := application.NewPublishRelease(deps).Execute(ctx, ex.CandidateIDs[0])
		if err != nil {
			t.Fatalf("publish %s: %v", version, err)
		}
		return res.ReleaseID
	}

	oldID := publish("7.24.2", "2026-08-15", "h1")
	f.clock.Advance(24 * time.Hour)
	newID := publish("7.24.3", "2026-09-02", "h2")

	if f.releases.Count() != 2 {
		t.Fatalf("stored %d releases, want 2; history must not be overwritten", f.releases.Count())
	}
	if _, err := f.releases.GetByID(ctx, oldID); err != nil {
		t.Errorf("the older release disappeared: %v", err)
	}
	latest, err := f.releases.LatestForProduct(ctx, productID, "stable")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.ID != newID {
		t.Errorf("latest = %q, want the newer release %q", latest.ID, newID)
	}
	var flagged int
	for _, m := range f.releases.Mappings() {
		if m.IsLatestObserved {
			flagged++
		}
	}
	if flagged != 1 {
		t.Errorf("%d mappings claim to be latest, want exactly 1", flagged)
	}
}
