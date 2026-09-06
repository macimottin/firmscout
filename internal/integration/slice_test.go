package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/macimottin/firmscout/internal/adapters/artifact"
	"github.com/macimottin/firmscout/internal/adapters/collectors"
	"github.com/macimottin/firmscout/internal/adapters/fetch"
	"github.com/macimottin/firmscout/internal/adapters/httpapi"
	"github.com/macimottin/firmscout/internal/adapters/normalize"
	"github.com/macimottin/firmscout/internal/adapters/postgres"
	"github.com/macimottin/firmscout/internal/adapters/registry"
	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
	"github.com/macimottin/firmscout/internal/platform"
)

const repoRoot = "../.."

// world is the whole system, assembled against a real database.
type world struct {
	db        *postgres.DB
	ids       application.IDGenerator
	clock     application.Clock
	vendors   *postgres.VendorRepo
	cats      *postgres.CategoryRepo
	prods     *postgres.ProductRepo
	sources   *postgres.SourceRepo
	rels      *postgres.ReleaseRepo
	sums      *postgres.SummaryRepo
	reviews   *postgres.ReviewRepo
	conflicts *postgres.ConflictRepo
	audit     *postgres.AuditRepo
	ingest    application.IngestDeps
	check     application.CheckSourceDeps
	// reviewRead and reviewWrite are the reviewer's two halves, assembled here for the
	// same reason the ingest struct is: a Phase 2 assertion that built its own deps
	// would be testing a graph no binary assembles.
	reviewRead  application.ReviewQueryDeps
	reviewWrite application.ReviewDeps
}

func newWorld(t *testing.T) *world {
	t.Helper()

	url := sliceDatabaseURL(t)
	ctx := context.Background()
	ids := platform.NewIDGenerator()
	db, err := postgres.Open(ctx, postgres.Config{URL: url, MaxConns: 4, ApplicationName: "firmscout-slice-test", IDs: ids})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(db.Close)

	if err := postgres.NewMigrator(db).Up(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	truncateAll(t, db)

	blobs, err := artifact.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("artifact blob store: %v", err)
	}
	// Metadata in PostgreSQL, bytes on disk. The foreign key from source_checks
	// requires the metadata row to exist, so a store that only wrote bytes would fail
	// the very first check.
	store := postgres.NewArtifactStore(db, blobs)

	// Two production defaults have to be relaxed for a local test server, and both
	// relaxations are the point rather than an inconvenience: the guard refuses
	// private addresses (an httptest server listens on loopback) and refuses
	// destination ports outside 80 and 443 (httptest picks a random high port).
	// Neither can be disabled by configuration in a running deployment, only by
	// constructing the guard differently, which is why this code lives in a test.
	guard := fetch.NewGuard(fetch.AllowPrivateNetworks(), fetch.WithoutPortRestriction())

	reg, err := collectors.NewRegistryFromDir(os.DirFS(repoRoot), "collectors/config")
	if err != nil {
		t.Fatalf("load collector configs: %v", err)
	}

	clock := platform.SystemClock{}
	w := &world{
		db:        db,
		ids:       ids,
		clock:     clock,
		vendors:   postgres.NewVendorRepo(db),
		cats:      postgres.NewCategoryRepo(db),
		prods:     postgres.NewProductRepo(db),
		sources:   postgres.NewSourceRepo(db),
		rels:      postgres.NewReleaseRepo(db),
		sums:      postgres.NewSummaryRepo(db),
		reviews:   postgres.NewReviewRepo(db),
		conflicts: postgres.NewConflictRepo(db),
		audit:     postgres.NewAuditRepo(db),
	}
	events := platform.NewEventPublisher(platform.NewLogger(platform.Config{
		Service: "slice-test", LogLevel: "error", LogFormat: "json",
	}))

	candidates := postgres.NewCandidateRepo(db)
	evidence := postgres.NewEvidenceRepo(db)

	// Conflicts and Audit are wired here exactly as internal/platform wires them. A nil
	// Conflicts port is tolerated by ValidateCandidate and turns gate 10 into something
	// that can only pass, so an end-to-end test that left them out would prove the
	// pipeline publishes contested versions rather than that it refuses to.
	w.ingest = application.IngestDeps{
		Sources: w.sources, Products: w.prods,
		Candidates: candidates,
		Releases:   w.rels,
		Evidence:   evidence,
		Reviews:    w.reviews,
		Conflicts:  w.conflicts,
		Audit:      w.audit,
		Artifacts:  store, Registry: reg,
		Queue:  postgres.NewQueue(db),
		Events: events, UoW: db, Clock: clock, IDs: ids,
	}
	w.reviewRead = application.ReviewQueryDeps{
		Reviews: w.reviews, Candidates: candidates, Evidence: evidence,
		Sources: w.sources, Products: w.prods, Vendors: w.vendors,
		Conflicts: w.conflicts, Audit: w.audit, Clock: clock,
	}
	w.reviewWrite = application.ReviewDeps{
		Reviews: w.reviews, Candidates: candidates,
		Conflicts: w.conflicts, Audit: w.audit,
		Publisher: application.NewPublishRelease(w.ingest),
		Events:    events, UoW: db, Clock: clock, IDs: ids,
	}
	w.check = application.CheckSourceDeps{
		Sources: w.sources, Fetcher: fetch.New(guard), Normalize: normalize.New(),
		Artifacts: store, Queue: postgres.NewQueue(db), Events: events,
		Clock: clock, IDs: ids, Policy: domain.DefaultSchedulingPolicy(),
		UserAgent: platform.DefaultUserAgent, MaxBytes: 8 << 20, Timeout: 20 * time.Second,
	}
	return w
}

// sliceDatabaseURL returns a connection string for a database this package owns.
//
// The end-to-end test truncates everything between runs, and Go runs test packages
// concurrently, so sharing a database with the PostgreSQL adapter's own integration
// tests produces deadlocks rather than failures anyone can read. A dedicated database
// costs one CREATE and removes the whole class of problem.
func sliceDatabaseURL(t *testing.T) string {
	t.Helper()

	base := os.Getenv("FIRMSCOUT_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set FIRMSCOUT_TEST_DATABASE_URL to run the end-to-end slice test")
	}

	u, err := neturl.Parse(base)
	if err != nil {
		t.Fatalf("parse FIRMSCOUT_TEST_DATABASE_URL: %v", err)
	}
	target := strings.TrimPrefix(u.Path, "/") + "_slice"

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to create the slice database: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	var exists bool
	if err := admin.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, target).Scan(&exists); err != nil {
		t.Fatalf("check for the slice database: %v", err)
	}
	if !exists {
		// The database name comes from the connection string this test was given,
		// not from user input, and pgx cannot parameterise a CREATE DATABASE.
		if _, err := admin.Exec(ctx, `CREATE DATABASE "`+target+`"`); err != nil {
			t.Fatalf("create the slice database %q: %v", target, err)
		}
	}

	out := *u
	out.Path = "/" + target
	sliceURL := out.String()

	conn, err := pgx.Connect(ctx, sliceURL)
	if err != nil {
		t.Fatalf("connect to the slice database: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_trgm`); err != nil {
		t.Fatalf("enable pg_trgm: %v", err)
	}
	return sliceURL
}

func truncateAll(t *testing.T, db *postgres.DB) {
	t.Helper()
	ctx := context.Background()
	rows, err := db.Pool().Query(ctx, `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'schema_migrations' AND tablename <> 'release_types'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, `"`+n+`"`)
	}
	rows.Close()
	if len(names) == 0 {
		return
	}
	if _, err := db.Pool().Exec(ctx, "TRUNCATE "+strings.Join(names, ", ")+" RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// TestVerticalSlice runs the first vertical slice end to end against a real database:
// registry sync, a scheduled check against a recorded fixture, change detection,
// deterministic extraction, validation, publication, and finally the public API's
// rendering of the result.
//
// The one thing it does not touch is a manufacturer's website.
func TestVerticalSlice(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// --- 1. Registry sync: the committed dataset becomes database rows. ----------
	// WithSummaries is wired here for the same reason apps/cli wires it: without a
	// refresher the sync upserts products that the public API cannot serve, and this
	// test asserts on that API. A slice test that built the graph differently from the
	// binary would prove something no deployment runs.
	sync := application.NewSyncRegistry(
		registry.New(os.DirFS(repoRoot), "dataset"),
		w.vendors, w.cats, w.prods, w.sources, w.ids, w.clock).
		WithSummaries(w.rels)
	report, err := sync.Execute(ctx)
	if err != nil {
		t.Fatalf("registry sync: %v", err)
	}
	if report.VendorsUpserted == 0 || report.ProductsUpserted == 0 || report.SourcesUpserted == 0 {
		t.Fatalf("sync produced nothing: %+v", report)
	}
	// Every committed source ships uncollectable. That is ADR-0018 working.
	if report.SourcesLeftDisabled != report.SourcesUpserted {
		t.Errorf("%d of %d sources were collectable straight from the registry; "+
			"a source must not be collectable before a maintainer reviews its terms",
			report.SourcesUpserted-report.SourcesLeftDisabled, report.SourcesUpserted)
	}

	product, err := w.prods.GetBySlug(ctx, "mikrotik-routeros")
	if err != nil {
		t.Fatalf("product not synced: %v", err)
	}

	// --- 2. A local server replays the recorded changelog fixture. ---------------
	fixture, err := os.ReadFile(repoRoot + "/testdata/fixtures/mikrotik/changelogs.fixture.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var requests, robotsRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// The fetcher consults robots.txt before every fetch, so the test server has
		// to answer it. 404 is the honest reply for a host that publishes none, and
		// it means "no restrictions" -- which is also what mikrotik.com's own
		// robots.txt says in substance (measured 2026-09-03: an empty Disallow).
		if r.URL.Path == "/robots.txt" {
			robotsRequests++
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		requests++
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Deliberately no ETag and cache-control: private, exactly as the real page
		// behaves. Change detection therefore has to rely on the section hash.
		rw.Header().Set("Cache-Control", "private")
		_, _ = rw.Write(fixture)
	}))
	defer srv.Close()

	// --- 3. Point the synced source at it and grant the compliance it needs. -----
	src, err := w.sources.GetBySlug(ctx, product.VendorID, "changelogs")
	if err != nil {
		t.Fatalf("source not synced: %v", err)
	}
	src.URL = srv.URL + "/download/changelogs"
	src.Enabled = true
	src.TermsReviewStatus = domain.TermsApproved
	src.RobotsPolicyStatus = domain.RobotsNotApplicable
	src.Health = domain.SourceActive
	src.NextCheckAt = time.Now().Add(-time.Minute)
	if err := w.sources.Upsert(ctx, src); err != nil {
		t.Fatalf("enable source: %v", err)
	}

	// It must now appear in the scheduler's dispatch query.
	due, err := w.sources.ListDispatchable(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("list dispatchable: %v", err)
	}
	if len(due) != 1 || due[0].ID != src.ID {
		t.Fatalf("the enabled source is not dispatchable: %+v", due)
	}

	// --- 4. Check, extract, validate, publish. -----------------------------------
	checkRes, err := application.NewCheckSource(w.check).Execute(ctx, src.ID)
	if err != nil {
		t.Fatalf("check source: %v", err)
	}
	if checkRes.Outcome != domain.OutcomeChanged {
		t.Fatalf("first check outcome = %q, want changed", checkRes.Outcome)
	}
	if !checkRes.ExtractionEnqueued {
		t.Fatal("extraction was not enqueued")
	}

	ex, err := application.NewExtractCandidates(w.ingest).Execute(ctx, src.ID, checkRes.ArtifactID)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if ex.NewCandidates == 0 {
		t.Fatalf("no candidates extracted from the fixture (%d seen)", ex.Extracted)
	}

	published := 0
	for _, cid := range ex.CandidateIDs {
		val, err := application.NewValidateCandidate(w.ingest).Execute(ctx, cid)
		if err != nil {
			t.Fatalf("validate %s: %v", cid, err)
		}
		if val.Decision != domain.GatePassed {
			t.Logf("candidate %s routed to %s: %s", cid, val.Decision, val.Reason)
			continue
		}
		pub, err := application.NewPublishRelease(w.ingest).Execute(ctx, cid)
		if err != nil {
			t.Fatalf("publish %s: %v", cid, err)
		}
		if pub.Published {
			published++
		}
	}
	if published == 0 {
		t.Fatal("the pipeline published nothing from a fixture containing real releases")
	}

	// --- 5. A second check must find nothing changed. ----------------------------
	src2, err := w.sources.GetByID(ctx, src.ID)
	if err != nil {
		t.Fatalf("reload source: %v", err)
	}
	src2.NextCheckAt = time.Now().Add(-time.Minute)
	if err := w.sources.UpdateCheckState(ctx, src2); err != nil {
		t.Fatalf("reset next check: %v", err)
	}
	second, err := application.NewCheckSource(w.check).Execute(ctx, src.ID)
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if second.Outcome != domain.OutcomeUnchanged {
		t.Errorf("re-checking identical content reported %q; the section hash is not stable", second.Outcome)
	}
	if second.ExtractionEnqueued {
		t.Error("extraction ran again for unchanged content")
	}
	if requests < 2 {
		t.Errorf("the fixture server saw %d requests, expected at least 2", requests)
	}
	if robotsRequests == 0 {
		t.Error("the fetcher never consulted robots.txt; collection must always check it")
	}

	// --- 6. The read model the public site reads. --------------------------------
	summary, err := w.sums.Get(ctx, "mikrotik-routeros")
	if err != nil {
		t.Fatalf("product summary: %v", err)
	}
	if summary.LatestRawVersion == "" {
		t.Fatal("the product summary carries no latest version after publication")
	}
	if summary.LatestReleaseType != string(domain.ReleaseTypeEmbeddedOS) {
		t.Errorf("release type = %q, want embedded_os; RouterOS is not firmware", summary.LatestReleaseType)
	}

	// Search must find the product by an alias, not only by its name. This is the
	// path that depends on aliases_text reaching the generated tsvector column.
	hits, err := w.sums.Search(ctx, "RouterOS", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Error("searching for the product's own name returned nothing")
	}
	aliasHits, err := w.sums.Search(ctx, "ROS", 10)
	if err != nil {
		t.Fatalf("alias search: %v", err)
	}
	if len(aliasHits) == 0 {
		t.Error("searching by a registered alias returned nothing; aliases are not reaching the search index")
	}

	// --- 7. The public API renders it, at the right date precision. --------------
	api, err := httpapi.NewServer(httpapi.Deps{
		Summaries: w.sums,
		Vendors:   w.vendors,
		Releases:  w.rels,
		Clock:     w.clock,
		IDs:       w.ids,
	})
	if err != nil {
		t.Fatalf("build API: %v", err)
	}

	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/products/mikrotik-routeros", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET product = %d, body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode product response: %v", err)
	}
	latest, ok := body["latestRelease"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no latestRelease: %s", rec.Body.String())
	}

	precision, _ := latest["releaseDatePrecision"].(string)
	if precision == "" {
		t.Error("the response omits releaseDatePrecision; a consumer cannot tell how much of the date is real")
	}
	date, hasDate := latest["releaseDate"].(string)
	switch precision {
	case string(domain.PrecisionExactDay):
		if len(date) != len("2006-01-02") {
			t.Errorf("exact_day precision rendered %q, want YYYY-MM-DD", date)
		}
	case string(domain.PrecisionMonthOnly):
		if len(date) != len("2006-01") {
			t.Errorf("month_only precision rendered %q, want YYYY-MM; a day must never appear", date)
		}
	case string(domain.PrecisionUnknown):
		if hasDate && date != "" {
			t.Errorf("unknown precision still rendered a date: %q", date)
		}
	}

	// The vendor never designated a recommended version, so the field must be null
	// rather than quietly filled in with the newest release.
	if rec, present := latest["recommended"]; present && rec != nil {
		t.Errorf("recommended = %v, but no vendor designation exists; the newest release must never be substituted", rec)
	}
}

// TestMigrationsAreIdempotent proves a second deployment of the same code does not
// fail, which is the property that makes the migrate-on-start check in the binaries
// safe.
func TestMigrationsAreIdempotent(t *testing.T) {
	url := sliceDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	db := postgres.NewDB(pool, platform.NewIDGenerator())
	m := postgres.NewMigrator(db)
	if err := m.Up(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("second migrate was not a no-op: %v", err)
	}
	st, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Pending) != 0 {
		t.Errorf("%d migration(s) still pending after Up", len(st.Pending))
	}
	if len(st.Applied) == 0 {
		t.Error("no migrations recorded as applied")
	}
}

// ---------------------------------------------------------------------------
// Phase 2: multi-source conflict, the review queue, and the rss_atom engine.
// ---------------------------------------------------------------------------

// syncRegistry loads the committed dataset into the database and returns nothing but
// the assurance that it worked. Every Phase 2 test needs the same first step, and
// repeating it four times is four places for the fixture to drift.
func syncRegistry(t *testing.T, w *world) {
	t.Helper()
	if _, err := application.NewSyncRegistry(
		registry.New(os.DirFS(repoRoot), "dataset"),
		w.vendors, w.cats, w.prods, w.sources, w.ids, w.clock).
		WithSummaries(w.rels).
		Execute(context.Background()); err != nil {
		t.Fatalf("registry sync: %v", err)
	}
}

// serveDocument replays one recorded document, answering robots.txt with 404 the way
// the vertical slice's server does: a host that publishes no robots.txt imposes no
// restriction, and the fetcher must still ask.
func serveDocument(t *testing.T, contentType string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		rw.Header().Set("Content-Type", contentType)
		rw.Header().Set("Cache-Control", "private")
		_, _ = rw.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// grantCollection points a synced source at the local replay server and gives it the
// compliance it needs to be dispatchable.
//
// This is the one thing a test may do that production may not: ADR-0018's gate is a
// human judgement recorded in the registry, and the registry ships every source
// uncollectable. Granting it here against a loopback server that serves a recorded
// document is not a compliance decision about the real host, and no code path outside a
// test can reach this function.
func grantCollection(t *testing.T, w *world, src domain.Source, url string) domain.Source {
	t.Helper()
	src.URL = url
	src.Enabled = true
	src.TermsReviewStatus = domain.TermsApproved
	src.RobotsPolicyStatus = domain.RobotsNotApplicable
	src.Health = domain.SourceActive
	src.NextCheckAt = time.Now().Add(-time.Minute)
	if err := w.sources.Upsert(context.Background(), src); err != nil {
		t.Fatalf("grant collection to %s: %v", src.Slug, err)
	}
	return src
}

// collectOnce runs check, extract and validate for one source and returns every
// validation result, keyed by candidate id in the order they were extracted.
func collectOnce(t *testing.T, w *world, sourceID string) []application.ValidateResult {
	t.Helper()
	ctx := context.Background()

	checkRes, err := application.NewCheckSource(w.check).Execute(ctx, sourceID)
	if err != nil {
		t.Fatalf("check %s: %v", sourceID, err)
	}
	if checkRes.Outcome != domain.OutcomeChanged {
		t.Fatalf("check %s outcome = %q, want changed", sourceID, checkRes.Outcome)
	}
	ex, err := application.NewExtractCandidates(w.ingest).Execute(ctx, sourceID, checkRes.ArtifactID)
	if err != nil {
		t.Fatalf("extract %s: %v", sourceID, err)
	}
	if ex.Extracted == 0 {
		t.Fatalf("extraction from %s produced no candidates", sourceID)
	}

	out := make([]application.ValidateResult, 0, len(ex.CandidateIDs))
	for _, cid := range ex.CandidateIDs {
		res, err := application.NewValidateCandidate(w.ingest).Execute(ctx, cid)
		if err != nil {
			t.Fatalf("validate %s: %v", cid, err)
		}
		res.CandidateID = cid
		out = append(out, res)
	}
	return out
}

// mirrorDocument is a second official MikroTik page reporting a newer long-term release
// than the recorded changelog does.
//
// Its markup is the measured shape of testdata/fixtures/mikrotik/changelogs.fixture.html
// -- the same container, version, badge and date elements -- with one entry and a
// different version, because the fact under test is a disagreement between two sources,
// not a second page layout. It is declared here rather than committed under
// testdata/fixtures/ because nobody measured it: it is a scenario, not evidence.
const mirrorDocument = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Changelogs (mirror)</title></head>
<body>
<main class="container">
  <div class="flex items-center gap-4 py-3 changelog-header">
    <div class="grow"><div class="flex items-center justify-between"><div class="flex gap-4 items-center">
      <span class="mtk-text-sm font-bold min-w-20"> 7.23.5 </span>
      <div class="flex gap-1 text-[10px] font-bold text-white uppercase leading-none">
        <div class="rounded-[4px] bg-green p-1"> Long-term </div>
      </div>
      <span class="mtk-text-sm text-gray-500"> 2026-09-04 </span>
    </div></div></div>
  </div>
</main>
</body></html>
`

// TestTwoSourcesDisagreeAndTheDecisionReachesAHuman is W1 and W2 end to end.
//
// It proves the three things ADR-0020 and ADR-0021 exist to guarantee: a version only
// one same-tier source supports is never published on its own authority; the
// disagreement becomes a durable row a reviewer can act on and a flag the product page
// can render; and the human override that publishes it anyway is recorded with the
// actor, the reason, and the fact that nobody authenticated that actor.
func TestTwoSourcesDisagreeAndTheDecisionReachesAHuman(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	syncRegistry(t, w)

	product, err := w.prods.GetBySlug(ctx, "mikrotik-routeros")
	if err != nil {
		t.Fatalf("product not synced: %v", err)
	}

	// --- 1. The registered source publishes the recorded changelog. --------------
	fixture, err := os.ReadFile(repoRoot + "/testdata/fixtures/mikrotik/changelogs.fixture.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	first, err := w.sources.GetBySlug(ctx, product.VendorID, "changelogs")
	if err != nil {
		t.Fatalf("source not synced: %v", err)
	}
	first = grantCollection(t, w, first, serveDocument(t, "text/html; charset=utf-8", fixture).URL+"/download/changelogs")

	for _, res := range collectOnce(t, w, first.ID) {
		if res.Decision != domain.GatePassed {
			continue
		}
		if _, err := application.NewPublishRelease(w.ingest).Execute(ctx, res.CandidateID); err != nil {
			t.Fatalf("publish %s: %v", res.CandidateID, err)
		}
	}

	summary, err := w.sums.Get(ctx, "mikrotik-routeros")
	if err != nil {
		t.Fatalf("product summary: %v", err)
	}
	if summary.HasSourceConflict {
		t.Fatal("one source alone produced a conflict; a single claim cannot disagree with anything")
	}

	// --- 2. A second source of equal authority reports a different version. ------
	mirror := domain.Source{
		ID:                    w.ids.NewID("src"),
		VendorID:              first.VendorID,
		ProductID:             first.ProductID,
		Slug:                  "changelogs-mirror",
		SourceType:            first.SourceType,
		URL:                   serveDocument(t, "text/html; charset=utf-8", []byte(mirrorDocument)).URL + "/changelogs",
		Official:              true,
		QualityClass:          domain.QualityOfficialManufacturer,
		AuthenticationType:    domain.AuthNone,
		CollectorID:           first.CollectorID,
		ExpectedContentType:   first.ExpectedContentType,
		CheckFrequencySeconds: first.CheckFrequencySeconds,
		MinFrequencySeconds:   first.MinFrequencySeconds,
		Normalize:             first.Normalize,
		Health:                domain.SourceDiscovered,
		Confidence:            first.Confidence,
		ManagedBy:             domain.ManagedByAdmin,
	}
	mirror = grantCollection(t, w, mirror, mirror.URL)

	results := collectOnce(t, w, mirror.ID)
	if len(results) != 1 {
		t.Fatalf("the mirror produced %d candidates, want exactly 1", len(results))
	}
	contested := results[0]

	// --- 3. Gate 10 refuses to choose between two official sources. --------------
	if contested.Decision != domain.GateReviewRequired {
		t.Fatalf("two official sources reporting different versions decided %q (%s), want review_required",
			contested.Decision, contested.Reason)
	}
	if contested.ConflictID == "" {
		t.Fatal("the disagreement produced no conflict row; nothing outlives the candidate")
	}
	if contested.ReviewItemID == "" {
		t.Fatal("the disagreement produced no review item; no human is ever asked")
	}

	conflict, err := w.conflicts.GetConflict(ctx, contested.ConflictID)
	if err != nil {
		t.Fatalf("load conflict: %v", err)
	}
	if conflict.State != domain.ConflictOpen {
		t.Errorf("conflict state = %q, want open", conflict.State)
	}
	if len(conflict.Versions) != 2 {
		t.Errorf("conflict records versions %v, want both contested values", conflict.Versions)
	}
	if len(conflict.SourceIDs) != 2 {
		t.Errorf("conflict records sources %v, want both participants", conflict.SourceIDs)
	}

	// The contested version must not have been published on the mirror's own say-so.
	latest, err := w.rels.LatestForProduct(ctx, product.ID, "long_term")
	if err != nil {
		t.Fatalf("latest release: %v", err)
	}
	if latest.Version.Normalized() == "7.23.5" {
		t.Fatal("the contested version was published; gate 10 did not stop publication")
	}

	// --- 4. The product page can say so. -----------------------------------------
	summary, err = w.sums.Get(ctx, "mikrotik-routeros")
	if err != nil {
		t.Fatalf("product summary after the conflict: %v", err)
	}
	if !summary.HasSourceConflict {
		t.Error("has_source_conflict is still false; the read model cannot show the disagreement")
	}

	// --- 4a. The public API renders the same conflict in detail, and a real source
	// behind the release that did publish -- not just the boolean and not a 500.
	//
	// This reuses the disagreement §3 already opened rather than staging a second,
	// synthetic one, and it compares against conflict (loaded from the database above
	// via w.conflicts.GetConflict, §3) instead of a literal expected value, so this
	// test fails if the presenter and the row it renders ever disagree, not only if
	// both drift from a number typed into the test.
	api, err := httpapi.NewServer(httpapi.Deps{
		Summaries: w.sums,
		Vendors:   w.vendors,
		Releases:  w.rels,
		Clock:     w.clock,
		IDs:       w.ids,
	})
	if err != nil {
		t.Fatalf("build API: %v", err)
	}

	productRec := httptest.NewRecorder()
	api.ServeHTTP(productRec, httptest.NewRequest(http.MethodGet, "/api/v1/products/mikrotik-routeros", nil))
	if productRec.Code != http.StatusOK {
		t.Fatalf("GET product = %d, body: %s", productRec.Code, productRec.Body.String())
	}
	var productBody map[string]any
	if err := json.Unmarshal(productRec.Body.Bytes(), &productBody); err != nil {
		t.Fatalf("decode product response: %v", err)
	}

	officialSources, _ := productBody["officialSources"].([]any)
	if len(officialSources) == 0 {
		t.Fatalf("officialSources = %v, want the source that actually published a release", productBody["officialSources"])
	}
	sourceEntry, _ := officialSources[0].(map[string]any)
	if sourceEntry["url"] != first.URL {
		t.Errorf("officialSources[0].url = %v, want the real registered source URL %q", sourceEntry["url"], first.URL)
	}
	if sourceEntry["kind"] != "html" {
		t.Errorf("officialSources[0].kind = %v, want %q for an html_page source", sourceEntry["kind"], "html")
	}
	if sourceEntry["official"] != true {
		t.Errorf("officialSources[0].official = %v, want true", sourceEntry["official"])
	}

	rawConflict, ok := productBody["conflict"].(map[string]any)
	if !ok {
		t.Fatalf("conflict = %v, want the open conflict's real detail alongside hasSourceConflict: true", productBody["conflict"])
	}
	if rawConflict["channel"] != conflict.Channel {
		t.Errorf("conflict.channel = %v, want %q (the row this conflict actually opened)", rawConflict["channel"], conflict.Channel)
	}
	gotVersions, _ := rawConflict["versions"].([]any)
	if len(gotVersions) != len(conflict.Versions) {
		t.Errorf("conflict.versions = %v, want %v", gotVersions, conflict.Versions)
	}
	if sourceCount, _ := rawConflict["sourceCount"].(float64); int(sourceCount) != len(conflict.SourceIDs) {
		t.Errorf("conflict.sourceCount = %v, want %d (this conflict's real participant count)", rawConflict["sourceCount"], len(conflict.SourceIDs))
	}
	if detectedAt, _ := rawConflict["detectedAt"].(string); detectedAt == "" {
		t.Error("conflict.detectedAt is empty; a real conflict must record when it was found")
	}

	// The release that did publish must carry the manufacturer page it was actually
	// read from -- the exact bug that returned an HTTP 500 on this endpoint before
	// PresentRelease set Source/Evidence (see the background this task was scoped from).
	releaseRec := httptest.NewRecorder()
	api.ServeHTTP(releaseRec, httptest.NewRequest(http.MethodGet, "/api/v1/releases/"+latest.ID, nil))
	if releaseRec.Code != http.StatusOK {
		t.Fatalf("GET release %s = %d, body: %s", latest.ID, releaseRec.Code, releaseRec.Body.String())
	}
	var releaseBody map[string]any
	if err := json.Unmarshal(releaseRec.Body.Bytes(), &releaseBody); err != nil {
		t.Fatalf("decode release response: %v", err)
	}
	relSource, ok := releaseBody["source"].(map[string]any)
	if !ok {
		t.Fatalf("release %s carries no source object: %s", latest.ID, releaseRec.Body.String())
	}
	if url, _ := relSource["url"].(string); url == "" {
		t.Error("release source.url is empty; a published release always has real evidence behind it (evidence_id is NOT NULL)")
	}
	relEvidence, ok := releaseBody["evidence"].(map[string]any)
	if !ok {
		t.Fatalf("release %s carries no evidence object: %s", latest.ID, releaseRec.Body.String())
	}
	if excerpt, _ := relEvidence["excerpt"].(string); excerpt == "" {
		t.Error("release evidence.excerpt is empty; a published release always cites a real excerpt")
	}

	// --- 5. Re-checking the same disagreement must not grow the queue. -----------
	// D5: one open conflict per (product, channel) carries at most one open item.
	again, err := application.NewValidateCandidate(w.ingest).Execute(ctx, contested.CandidateID)
	if err != nil {
		t.Fatalf("re-validate the contested candidate: %v", err)
	}
	if again.ReviewItemID != contested.ReviewItemID {
		t.Errorf("re-validating one disagreement produced review item %q, first time %q; "+
			"the queue grows by one item per check", again.ReviewItemID, contested.ReviewItemID)
	}

	// --- 6. The queue is readable through the application layer. -----------------
	page, err := application.NewListReviewQueue(w.reviewRead).Execute(ctx, application.ReviewQueueQuery{
		Kinds: []string{"multi_source_conflict"},
	})
	if err != nil {
		t.Fatalf("list review queue: %v", err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("the queue holds %d conflict items, want exactly 1", len(page.Entries))
	}
	entry := page.Entries[0]
	if entry.Item.ID != contested.ReviewItemID {
		t.Errorf("queue lists item %q, want %q", entry.Item.ID, contested.ReviewItemID)
	}
	if entry.ProductSlug != "mikrotik-routeros" || entry.VendorSlug != "mikrotik" {
		t.Errorf("queue entry names vendor %q product %q; a reviewer cannot recognise it",
			entry.VendorSlug, entry.ProductSlug)
	}
	if entry.Item.SLAClass == "" || entry.Item.PriorityScore == 0 {
		t.Errorf("item carries SLA %q and score %d; the queue has no order",
			entry.Item.SLAClass, entry.Item.PriorityScore)
	}

	detail, err := application.NewGetReviewItem(w.reviewRead).Execute(ctx, contested.ReviewItemID)
	if err != nil {
		t.Fatalf("get review item: %v", err)
	}
	if !detail.ConflictFound || detail.Conflict.ID != contested.ConflictID {
		t.Errorf("detail links conflict %q, want %q", detail.Conflict.ID, contested.ConflictID)
	}
	if len(detail.ConflictObservations) < 2 {
		t.Errorf("detail shows %d observations; a reviewer cannot see who claims what",
			len(detail.ConflictObservations))
	}
	var sawGateTen bool
	for _, g := range detail.Gates {
		if g.Outcome == domain.GateReviewRequired {
			sawGateTen = true
		}
	}
	if !sawGateTen {
		t.Error("detail carries no failing gate; the reviewer is not told why the item exists")
	}

	// --- 7. Accepting publishes, closes the conflict, and is recorded. -----------
	decision, err := application.NewDecideReviewItem(w.reviewWrite).Accept(ctx, application.ReviewDecisionInput{
		ItemID:    contested.ReviewItemID,
		Actor:     "alex@firmscout.dev",
		Reason:    "confirmed against the vendor's own download page",
		RequestID: "req_slice_accept",
	})
	if err != nil {
		t.Fatalf("accept review item: %v", err)
	}
	if !decision.Published || decision.ReleaseID == "" {
		t.Fatalf("accepting produced published=%v release=%q; the override did nothing",
			decision.Published, decision.ReleaseID)
	}
	if decision.ConflictID != contested.ConflictID {
		t.Errorf("the decision resolved conflict %q, want %q", decision.ConflictID, contested.ConflictID)
	}

	resolved, err := w.conflicts.GetConflict(ctx, contested.ConflictID)
	if err != nil {
		t.Fatalf("reload conflict: %v", err)
	}
	if resolved.State == domain.ConflictOpen {
		t.Error("the conflict is still open after the decision that settled it")
	}

	events, err := w.audit.ListForSubject(ctx, "review_item", contested.ReviewItemID, 10)
	if err != nil {
		t.Fatalf("read audit trail: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("the accepted decision wrote %d audit rows, want exactly 1", len(events))
	}
	audit := events[0]
	if audit.ActorID != "alex@firmscout.dev" || audit.Action != application.AuditActionReviewAccepted {
		t.Errorf("audit row records actor %q action %q", audit.ActorID, audit.Action)
	}
	if audit.ActorAuthenticated {
		t.Error("the audit row claims the actor was authenticated; FirmScout has no login (ADR-0021)")
	}
	if strings.TrimSpace(audit.Reason) == "" {
		t.Error("the audit row carries no reason; a decision with no stated reason is a timestamp")
	}

	// A second decision on the same item must be refused rather than published twice.
	if _, err := application.NewDecideReviewItem(w.reviewWrite).Reject(ctx, application.ReviewDecisionInput{
		ItemID: contested.ReviewItemID, Actor: "sam@firmscout.dev", Reason: "changed my mind",
	}); err == nil {
		t.Error("a resolved review item accepted a second decision")
	}
}

// TestFortinetAdvisoryFlowsThroughTheRSSAtomEngine is W3 end to end: the recorded PSIRT
// feed, served over the loopback, read by the config-driven rss_atom engine, and routed
// exactly where ADR-0018's evidence rule says it must go.
//
// The assertion that matters is the one that looks like a failure: every advisory is
// refused automatic publication. The feed states that Fortinet published FG-IR-26-163 on
// 2026-08-12 and nothing else -- it names no affected product -- so the config scores it
// 0.55, below the 0.85 automatic-publication threshold, and gate 8 hands it to a human
// (D18). A config that let this publish would be asserting a product correlation nobody
// measured.
//
// The test then follows the one path that does publish an advisory -- a human accepting
// it -- to prove that even then it never becomes the version a reader is told to
// install. That rule lives in domain.Release.EligibleForLatest and nowhere else: every
// mechanical part of publication accepts an advisory happily.
func TestFortinetAdvisoryFlowsThroughTheRSSAtomEngine(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	syncRegistry(t, w)

	product, err := w.prods.GetBySlug(ctx, "fortinet-fortios")
	if err != nil {
		t.Fatalf("fortinet product not synced: %v", err)
	}
	feed, err := os.ReadFile(repoRoot + "/testdata/fixtures/fortinet/psirt-advisories.fixture.xml")
	if err != nil {
		t.Fatalf("read fortinet fixture: %v", err)
	}
	src, err := w.sources.GetBySlug(ctx, product.VendorID, "psirt-advisories")
	if err != nil {
		t.Fatalf("fortinet source not synced: %v", err)
	}
	// The registry ships this source with terms_review_status: pending, and that is the
	// correct state for it -- Fortinet's licence question is unresolved. The grant below
	// applies to a loopback server replaying a recorded file, not to Fortinet.
	if src.TermsReviewStatus != domain.TermsPending {
		t.Fatalf("the committed Fortinet source ships with terms %q; ADR-0018 requires pending until a human decides",
			src.TermsReviewStatus)
	}
	src = grantCollection(t, w, src, serveDocument(t, "text/xml; charset=utf-8", feed).URL+"/rss/advisory")

	results := collectOnce(t, w, src.ID)
	if len(results) != 3 {
		t.Fatalf("the recorded feed produced %d candidates, want 3", len(results))
	}

	for _, res := range results {
		candidate, err := w.ingest.Candidates.GetByID(ctx, res.CandidateID)
		if err != nil {
			t.Fatalf("load candidate %s: %v", res.CandidateID, err)
		}
		if candidate.ReleaseType != domain.ReleaseTypeAdvisory {
			t.Errorf("candidate %s has release type %q, want advisory",
				candidate.Version.Raw(), candidate.ReleaseType)
		}
		if candidate.ReleaseDate.Precision() != domain.PrecisionExactDay {
			t.Errorf("candidate %s has date precision %q; the feed publishes full RFC-1123 dates",
				candidate.Version.Raw(), candidate.ReleaseDate.Precision())
		}
		if !strings.HasPrefix(candidate.Version.Normalized(), "fg-ir-") {
			t.Errorf("candidate version %q is not an advisory identifier", candidate.Version.Normalized())
		}
		if res.Decision != domain.GateReviewRequired {
			t.Errorf("advisory %s decided %q (%s), want review_required: the feed names no affected product, "+
				"so nothing from it may publish on its own (ADR-0018, D18)",
				candidate.Version.Raw(), res.Decision, res.Reason)
		}
		if !strings.Contains(res.Reason, string(domain.GateConfidenceThreshold)) {
			t.Errorf("advisory %s was held back by %q; the confidence score is what must stop it, "+
				"because that is what survives a future terms approval", candidate.Version.Raw(), res.Reason)
		}
		if res.ReviewItemID == "" {
			t.Errorf("advisory %s was refused publication and queued for nobody", candidate.Version.Raw())
		}
	}

	// A human accepts one. This is the only path by which an advisory becomes a row in
	// releases, and it is deliberately not the pipeline's.
	accepted := results[0]
	decision, err := application.NewDecideReviewItem(w.reviewWrite).Accept(ctx, application.ReviewDecisionInput{
		ItemID: accepted.ReviewItemID,
		Actor:  "alex@firmscout.dev",
		Reason: "advisory identifier confirmed against the vendor bulletin",
	})
	if err != nil {
		t.Fatalf("accept advisory: %v", err)
	}
	if !decision.Published {
		t.Fatal("accepting the advisory published nothing")
	}

	if _, err := w.rels.LatestForProduct(ctx, product.ID, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("latest release for a product whose only release is an advisory returned %v; "+
			"a CVE identifier is not firmware anybody can install", err)
	}
}

// ---------------------------------------------------------------------------
// Fix-pass regressions. Each of these reproduces a defect that was measured in
// the shipped code, and each crosses at least two layers, which is why it lives
// here rather than in the package that owns the repair: the unit tests pin the
// rule where it is written, and these pin that nothing between the collector and
// the database quietly undoes it.
// ---------------------------------------------------------------------------

// offsetFeed is a PSIRT-shaped feed whose two advisories were published on the same
// calendar day either side of UTC.
//
// The shipped fixture escapes the bug this guards by luck: every pubDate in it is
// 00:00:00 -0700, which lands on the same day whether or not the timestamp is moved to
// UTC first. These two do not. 17:00:00 -0700 is 2026-08-13 in UTC and 01:00:00 +0200 is
// 2026-08-11, so a pipeline that normalises before reading the calendar records three
// different days for one published day, and which way it errs depends on the sign of the
// offset. Both items here state 12 August.
const offsetFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss xmlns:atom="http://www.w3.org/2005/Atom" version="2.0">
  <channel>
    <title>Fortiguard Threat Intelligence — PSIRT advisories</title>
    <link>https://www.fortiguard.com/psirt</link>
    <description>Scenario, not evidence: two offsets around one published day.</description>
    <pubDate>Sat, 05 Sep 2026 15:45:08 -0700</pubDate>
    <item>
      <title>Afternoon west of UTC</title>
      <link>https://www.fortiguard.com/psirt/FG-IR-26-901</link>
      <guid isPermaLink="true">https://www.fortiguard.com/psirt/FG-IR-26-901</guid>
      <description><![CDATA[<p>Placeholder.</p>]]></description>
      <pubDate>Wed, 12 Aug 2026 17:00:00 -0700</pubDate>
    </item>
    <item>
      <title>Early morning east of UTC</title>
      <link>https://www.fortiguard.com/psirt/FG-IR-26-902</link>
      <guid isPermaLink="true">https://www.fortiguard.com/psirt/FG-IR-26-902</guid>
      <description><![CDATA[<p>Placeholder.</p>]]></description>
      <pubDate>Wed, 12 Aug 2026 01:00:00 +0200</pubDate>
    </item>
  </channel>
</rss>
`

// TestAFeedTimestampWithAnOffsetKeepsThePublishedDay is the timezone defect end to end.
//
// The repair is one line in the collector pipeline, and a unit test already pins it
// there. What that test cannot show is that the day survives the rest of the journey:
// the domain constructor, the pgx encode, the CHECK constraints that anchor a partial
// date, and the read back out. ADR-0017's rule is about what FirmScout has on record,
// not about what a parser returned, so the assertion is made against the row.
func TestAFeedTimestampWithAnOffsetKeepsThePublishedDay(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	syncRegistry(t, w)

	product, err := w.prods.GetBySlug(ctx, "fortinet-fortios")
	if err != nil {
		t.Fatalf("fortinet product not synced: %v", err)
	}
	src, err := w.sources.GetBySlug(ctx, product.VendorID, "psirt-advisories")
	if err != nil {
		t.Fatalf("fortinet source not synced: %v", err)
	}
	src = grantCollection(t, w, src,
		serveDocument(t, "text/xml; charset=utf-8", []byte(offsetFeed)).URL+"/rss/advisory")

	results := collectOnce(t, w, src.ID)
	if len(results) != 2 {
		t.Fatalf("the feed produced %d candidates, want 2", len(results))
	}

	const published = "2026-08-12"
	for _, res := range results {
		candidate, err := w.ingest.Candidates.GetByID(ctx, res.CandidateID)
		if err != nil {
			t.Fatalf("load candidate %s: %v", res.CandidateID, err)
		}
		day, ok := candidate.ReleaseDate.ExactDay()
		if !ok {
			t.Fatalf("advisory %s recorded precision %q; an RFC 1123 pubDate states a day",
				candidate.Version.Normalized(), candidate.ReleaseDate.Precision())
		}
		if got := day.Format("2006-01-02"); got != published {
			t.Errorf("advisory %s: the feed published %s, FirmScout recorded %s",
				candidate.Version.Normalized(), published, got)
		}
	}

	// Through the database and back. A release row is the thing ADR-0017 is about, and
	// it is written by a different layer than the one that parsed the date.
	accepted := results[0]
	if _, err := application.NewDecideReviewItem(w.reviewWrite).Accept(ctx, application.ReviewDecisionInput{
		ItemID: accepted.ReviewItemID,
		Actor:  "alex@firmscout.dev",
		Reason: "advisory identifier confirmed against the vendor bulletin",
	}); err != nil {
		t.Fatalf("accept advisory: %v", err)
	}
	stored, _, err := w.rels.ListForProduct(ctx, product.ID, application.ReleaseListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list stored releases: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("accepting one advisory stored %d releases, want 1", len(stored))
	}
	day, ok := stored[0].ReleaseDate.ExactDay()
	if !ok {
		t.Fatalf("the stored release lost its day precision: %q", stored[0].ReleaseDate.Precision())
	}
	if got := day.Format("2006-01-02"); got != published {
		t.Errorf("the stored release is dated %s; the feed published %s", got, published)
	}
}

// revisedMirrorDocument is the mirror source changing its mind: the same page shape as
// mirrorDocument, reporting a different long-term version.
//
// A source revising a claim is ordinary -- a vendor corrects a typo, a mirror catches
// up -- and it is the case the review queue got wrong.
const revisedMirrorDocument = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Changelogs (mirror)</title></head>
<body>
<main class="container">
  <div class="flex items-center gap-4 py-3 changelog-header">
    <div class="grow"><div class="flex items-center justify-between"><div class="flex gap-4 items-center">
      <span class="mtk-text-sm font-bold min-w-20"> 7.23.9 </span>
      <div class="flex gap-1 text-[10px] font-bold text-white uppercase leading-none">
        <div class="rounded-[4px] bg-green p-1"> Long-term </div>
      </div>
      <span class="mtk-text-sm text-gray-500"> 2026-09-05 </span>
    </div></div></div>
  </div>
</main>
</body></html>
`

// TestAcceptingASupersededConflictPublishesTheLiveClaim is the review-queue defect end
// to end.
//
// A source opened a disagreement, a queue item was filed naming its candidate, and then
// the source revised its claim. The measured failure was that the item went on naming
// the withdrawn candidate, so accepting it published a version no source claimed any
// more -- FirmScout inventing a release out of its own bookkeeping, which is the single
// thing the catalogue must never do. The assertion is therefore on what "latest" says
// after a human accepts, because that is what a reader is told to install.
func TestAcceptingASupersededConflictPublishesTheLiveClaim(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	syncRegistry(t, w)

	product, err := w.prods.GetBySlug(ctx, "mikrotik-routeros")
	if err != nil {
		t.Fatalf("product not synced: %v", err)
	}
	fixture, err := os.ReadFile(repoRoot + "/testdata/fixtures/mikrotik/changelogs.fixture.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	first, err := w.sources.GetBySlug(ctx, product.VendorID, "changelogs")
	if err != nil {
		t.Fatalf("source not synced: %v", err)
	}
	first = grantCollection(t, w, first,
		serveDocument(t, "text/html; charset=utf-8", fixture).URL+"/download/changelogs")
	for _, res := range collectOnce(t, w, first.ID) {
		if res.Decision != domain.GatePassed {
			continue
		}
		if _, err := application.NewPublishRelease(w.ingest).Execute(ctx, res.CandidateID); err != nil {
			t.Fatalf("publish %s: %v", res.CandidateID, err)
		}
	}

	// --- The mirror disagrees, and a human is asked about 7.23.5. ----------------
	mirror := domain.Source{
		ID:                    w.ids.NewID("src"),
		VendorID:              first.VendorID,
		ProductID:             first.ProductID,
		Slug:                  "changelogs-mirror",
		SourceType:            first.SourceType,
		Official:              true,
		QualityClass:          domain.QualityOfficialManufacturer,
		AuthenticationType:    domain.AuthNone,
		CollectorID:           first.CollectorID,
		ExpectedContentType:   first.ExpectedContentType,
		CheckFrequencySeconds: first.CheckFrequencySeconds,
		MinFrequencySeconds:   first.MinFrequencySeconds,
		Normalize:             first.Normalize,
		Health:                domain.SourceDiscovered,
		Confidence:            first.Confidence,
		ManagedBy:             domain.ManagedByAdmin,
	}
	mirror = grantCollection(t, w, mirror,
		serveDocument(t, "text/html; charset=utf-8", []byte(mirrorDocument)).URL+"/changelogs")

	stale := collectOnce(t, w, mirror.ID)
	if len(stale) != 1 {
		t.Fatalf("the mirror produced %d candidates, want 1", len(stale))
	}
	if stale[0].ReviewItemID == "" {
		t.Fatal("the disagreement reached nobody")
	}
	staleCandidate, err := w.ingest.Candidates.GetByID(ctx, stale[0].CandidateID)
	if err != nil {
		t.Fatalf("load the first contested candidate: %v", err)
	}
	if staleCandidate.Version.Normalized() != "7.23.5" {
		t.Fatalf("the mirror first claimed %q, want 7.23.5", staleCandidate.Version.Normalized())
	}

	// --- The mirror revises its claim to 7.23.9. ---------------------------------
	mirror = grantCollection(t, w, mirror,
		serveDocument(t, "text/html; charset=utf-8", []byte(revisedMirrorDocument)).URL+"/changelogs")
	live := collectOnce(t, w, mirror.ID)
	if len(live) != 1 {
		t.Fatalf("the revised mirror produced %d candidates, want 1", len(live))
	}
	liveCandidate, err := w.ingest.Candidates.GetByID(ctx, live[0].CandidateID)
	if err != nil {
		t.Fatalf("load the revised candidate: %v", err)
	}
	if liveCandidate.Version.Normalized() != "7.23.9" {
		t.Fatalf("the revised mirror claims %q, want 7.23.9", liveCandidate.Version.Normalized())
	}
	if live[0].ReviewItemID == "" {
		t.Fatal("the revised disagreement reached nobody")
	}

	// The queue must name what is in dispute now, not what was.
	item, err := w.reviews.GetByID(ctx, live[0].ReviewItemID)
	if err != nil {
		t.Fatalf("load the queue item: %v", err)
	}
	if item.SubjectType == application.SubjectTypeCandidateRelease &&
		item.SubjectID == stale[0].CandidateID {
		t.Errorf("the queue item still names the withdrawn candidate %s (7.23.5); "+
			"accepting it would publish a version no source claims", stale[0].CandidateID)
	}

	// --- A human accepts. What gets published is the claim that is live. ---------
	if _, err := application.NewDecideReviewItem(w.reviewWrite).Accept(ctx, application.ReviewDecisionInput{
		ItemID: live[0].ReviewItemID,
		Actor:  "alex@firmscout.dev",
		Reason: "confirmed against the vendor's own download page",
	}); err != nil {
		t.Fatalf("accept the queued disagreement: %v", err)
	}

	latest, err := w.rels.LatestForProduct(ctx, product.ID, "long_term")
	if err != nil {
		t.Fatalf("latest release: %v", err)
	}
	if latest.Version.Normalized() == "7.23.5" {
		t.Fatal("accepting the queue item published 7.23.5, the version the mirror withdrew; " +
			"no source claims it and FirmScout is now recommending a release it invented")
	}

	// And the withdrawn candidate must not be left parked in review with nothing
	// pointing at it, which is the other half of the same defect.
	orphan, err := w.ingest.Candidates.GetByID(ctx, stale[0].CandidateID)
	if err != nil {
		t.Fatalf("reload the withdrawn candidate: %v", err)
	}
	if orphan.State == domain.CandidateHumanReviewRequired {
		open, err := w.reviews.FindOpenBySubject(ctx,
			application.SubjectTypeCandidateRelease, stale[0].CandidateID)
		if err == nil {
			t.Logf("the withdrawn candidate is still queued as %s, which is acceptable", open.ID)
		} else if errors.Is(err, domain.ErrNotFound) {
			t.Error("the withdrawn candidate is still awaiting review with nothing in the queue naming it")
		} else {
			t.Fatalf("look for an item naming the withdrawn candidate: %v", err)
		}
	}
}

// TestATakedownAfterQueueingRefusesPublicationOnAccept is the compliance defect end to
// end.
//
// ADR-0018's gate was enforced when a source is dispatched and fetched, and nowhere on
// the way out. A candidate collected while a source was permitted therefore stayed
// publishable after the permission was withdrawn, and the queue is exactly where such a
// candidate waits -- often for days. A takedown that does not reach data already
// collected is not a takedown.
//
// The refusal has to be an error rather than a silent skip, because the decision runs in
// a unit of work: an error rolls the whole thing back, so the item stays open for a
// human to see rather than being quietly consumed.
func TestATakedownAfterQueueingRefusesPublicationOnAccept(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	syncRegistry(t, w)

	product, err := w.prods.GetBySlug(ctx, "fortinet-fortios")
	if err != nil {
		t.Fatalf("fortinet product not synced: %v", err)
	}
	feed, err := os.ReadFile(repoRoot + "/testdata/fixtures/fortinet/psirt-advisories.fixture.xml")
	if err != nil {
		t.Fatalf("read fortinet fixture: %v", err)
	}
	src, err := w.sources.GetBySlug(ctx, product.VendorID, "psirt-advisories")
	if err != nil {
		t.Fatalf("fortinet source not synced: %v", err)
	}
	src = grantCollection(t, w, src,
		serveDocument(t, "text/xml; charset=utf-8", feed).URL+"/rss/advisory")

	results := collectOnce(t, w, src.ID)
	if len(results) == 0 || results[0].ReviewItemID == "" {
		t.Fatal("the advisory feed queued nothing; there is no takedown to test")
	}
	queued := results[0]

	// The operator receives a takedown and records it the only way ADR-0018 recognises.
	src.TermsReviewStatus = domain.TermsProhibited
	if err := w.sources.Upsert(ctx, src); err != nil {
		t.Fatalf("record the takedown: %v", err)
	}

	_, err = application.NewDecideReviewItem(w.reviewWrite).Accept(ctx, application.ReviewDecisionInput{
		ItemID: queued.ReviewItemID,
		Actor:  "alex@firmscout.dev",
		Reason: "looks fine to me",
	})
	if err == nil {
		t.Fatal("accepting the queued item published data from a source under a takedown")
	}
	if !errors.Is(err, domain.ErrNotPermitted) {
		t.Errorf("the refusal is %v; it must carry domain.ErrNotPermitted so a caller can "+
			"tell a compliance refusal from a transient failure", err)
	}

	// Nothing was published, and the work was not silently consumed.
	if _, err := w.rels.LatestForProduct(ctx, product.ID, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a release exists for the prohibited source's product: %v", err)
	}
	item, err := w.reviews.GetByID(ctx, queued.ReviewItemID)
	if err != nil {
		t.Fatalf("reload the queue item: %v", err)
	}
	if item.State != "open" && item.State != "in_progress" {
		t.Errorf("the refused item is in state %q; the unit of work did not roll back and "+
			"a reviewer will never see why nothing happened", item.State)
	}
}

// ---------------------------------------------------------------------------
// Phase 3: the device-first catalogue.
// ---------------------------------------------------------------------------

// TestAModelNumberFindsTheFirmware is the whole product claim of this phase, proved
// across every layer at once: registry YAML, the sync use case, PostgreSQL's generated
// search vector, the summary projection, and the public HTTP API.
//
// The claim, in the repository owner's words, is that a company managing a fleet finds
// firmware THROUGH ITS DEVICE MODELS and in no other way. So the test starts where that
// company starts -- with the string stamped on the chassis, and nothing else -- and
// refuses to use any knowledge a fleet manager would not have. It never looks a device
// up by slug to begin with; the slug is something the search result hands it.
//
// The negative half matters as much as the positive one. A device must NOT report a
// latest release of its own (ADR-0024 D4): the honest answer to "which RouterOS release
// belongs on this switch" is that FirmScout has not established one, and a device page
// that inherited the operating system's newest version would be telling an operator to
// flash an image nobody verified their hardware takes. That failure would look like a
// working feature, which is why it is asserted here rather than left to inspection.
func TestAModelNumberFindsTheFirmware(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	syncRegistry(t, w)

	// The operating system needs real releases, or "the firmware is over there" would
	// point at an empty page and the test would pass on a technicality. These come
	// through the ordinary collector pipeline against the recorded fixture, so what the
	// device resolves to is a release the publish path actually admitted.
	fixture, err := os.ReadFile(repoRoot + "/testdata/fixtures/mikrotik/changelogs.fixture.html")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := serveDocument(t, "text/html; charset=utf-8", fixture)
	vendor, err := w.vendors.GetBySlug(ctx, "mikrotik")
	if err != nil {
		t.Fatalf("vendor not synced: %v", err)
	}
	src, err := w.sources.GetBySlug(ctx, vendor.ID, "changelogs")
	if err != nil {
		t.Fatalf("source not synced: %v", err)
	}
	src = grantCollection(t, w, src, srv.URL+"/download/changelogs")
	published := 0
	for _, res := range collectOnce(t, w, src.ID) {
		if res.Decision != domain.GatePassed {
			continue
		}
		pub, err := application.NewPublishRelease(w.ingest).Execute(ctx, res.CandidateID)
		if err != nil {
			t.Fatalf("publish %s: %v", res.CandidateID, err)
		}
		if pub.Published {
			published++
		}
	}
	if published == 0 {
		t.Fatal("the pipeline published no RouterOS release, so there is no firmware for a device to resolve to")
	}

	api, err := httpapi.NewServer(httpapi.Deps{
		Summaries: w.sums, Vendors: w.vendors, Releases: w.rels, Clock: w.clock, IDs: w.ids,
	})
	if err != nil {
		t.Fatalf("build API: %v", err)
	}
	getJSON := func(path string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s (%d): %v; body: %s", path, rec.Code, err, rec.Body.String())
		}
		return rec.Code, body
	}

	// --- 1. The only thing the fleet manager has: a product code. ----------------
	//
	// A42G-HbeP shares no lexeme with "hAP be lite", so a hit cannot come from the
	// product name, from the slug, or from a trigram similarity on either. The only
	// path that can produce it is the model_number alias reaching aliases_text and
	// then the generated search vector -- which is exactly the mechanism ADR-0024 D9
	// chose over a devices table, and this is the assertion that would notice if it
	// were ever quietly replaced.
	const modelNumber = "A42G-HbeP"
	code, searchBody := getJSON("/api/v1/search?q=" + neturl.QueryEscape(modelNumber))
	if code != http.StatusOK {
		t.Fatalf("GET /search?q=%s = %d", modelNumber, code)
	}
	results, _ := searchBody["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("searching for the product code %q returned nothing; a fleet manager holding "+
			"this string cannot reach its device", modelNumber)
	}
	hit, _ := results[0].(map[string]any)
	if got, _ := hit["modelIdentifier"].(string); got != modelNumber {
		t.Errorf("search hit echoed modelIdentifier %q, want %q; the operator cannot confirm "+
			"this row is the thing they hold", got, modelNumber)
	}
	deviceSlug, _ := hit["slug"].(string)
	if deviceSlug == "" {
		t.Fatalf("the search hit carries no slug to follow: %v", hit)
	}

	// --- 2. The device page names the operating system, and no version. ----------
	code, device := getJSON("/api/v1/products/" + deviceSlug)
	if code != http.StatusOK {
		t.Fatalf("GET the device found by search = %d", code)
	}
	if got, _ := device["modelIdentifier"].(string); got != modelNumber {
		t.Errorf("device modelIdentifier = %q, want %q", got, modelNumber)
	}
	runs, _ := device["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("device runs %d products, want exactly 1; the registry declares one runs_os edge", len(runs))
	}
	osRef, _ := runs[0].(map[string]any)
	osSlug, _ := osRef["slug"].(string)
	if osSlug != "mikrotik-routeros" {
		t.Fatalf("device runs %q, want mikrotik-routeros", osSlug)
	}

	// The three claims are kept apart on purpose (D10). This is the one that says
	// FirmScout has NOT established which release fits this model, and it must be
	// present rather than omitted: an absent key reads as "they apply".
	applicability, ok := device["firmwareApplicability"].(map[string]any)
	if !ok {
		t.Fatalf("the device response omits firmwareApplicability entirely; an absent key "+
			"reads as 'every release applies', which is the claim this phase refuses to make: %v", device)
	}
	if verified, _ := applicability["verified"].(bool); verified {
		t.Error("firmwareApplicability.verified is true for a device; nothing has verified " +
			"which RouterOS image this model takes")
	}
	if basis, _ := applicability["basis"].(string); basis != "runs_os_unverified" {
		t.Errorf("firmwareApplicability.basis = %q, want runs_os_unverified", basis)
	}
	if device["latestRelease"] != nil {
		t.Errorf("the device reports a latest release of its own (%v); a hardware model has no "+
			"release stream, and inheriting the operating system's would tell an operator to "+
			"flash an unverified image", device["latestRelease"])
	}

	// A device's own /latest is a 404 by design, not an oversight. Asserting it keeps
	// a future "helpful" fallback from silently turning into an applicability claim.
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/products/"+deviceSlug+"/latest", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET the device's /latest = %d, want 404; FirmScout has no per-model answer "+
			"and must say so rather than substitute the operating system's newest release", rec.Code)
	}

	// --- 3. Following the edge reaches firmware that actually exists. ------------
	code, os := getJSON("/api/v1/products/" + osSlug)
	if code != http.StatusOK {
		t.Fatalf("GET the operating system the device runs = %d", code)
	}
	osApplicability, _ := os["firmwareApplicability"].(map[string]any)
	if verified, _ := osApplicability["verified"].(bool); !verified {
		t.Error("the operating system's own releases are reported as unverified; they are " +
			"mapped to it directly and need no inference")
	}
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/products/"+osSlug+"/releases", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the operating system's releases = %d", rec.Code)
	}
	var history struct {
		Releases []struct {
			RawVersion string `json:"rawVersion"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
		t.Fatalf("decode release history: %v", err)
	}
	if len(history.Releases) == 0 {
		t.Fatal("the journey from a model number ends at an operating system with no releases; " +
			"the fleet manager has been walked to an empty page")
	}
	t.Logf("%s -> %s -> %d release(s), newest %q",
		modelNumber, osSlug, len(history.Releases), history.Releases[0].RawVersion)

	// --- 4. The operating system is not made device-shaped by the reverse edge. --
	// Six devices point at RouterOS. Its own page must still report itself as software:
	// no model identifier, no outgoing runs edge. The relationship is directed, and a
	// summary that leaked the reverse direction would put "runs: hAP be lite" on an
	// operating system's page.
	if os["modelIdentifier"] != nil {
		t.Errorf("the operating system carries modelIdentifier %v; it is not a hardware model", os["modelIdentifier"])
	}
	if osRuns, _ := os["runs"].([]any); len(osRuns) != 0 {
		t.Errorf("the operating system runs %d products; the runs edge is directed device -> OS "+
			"and must not appear reversed", len(osRuns))
	}
}
