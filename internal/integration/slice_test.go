package integration_test

import (
	"context"
	"encoding/json"
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
	db      *postgres.DB
	ids     application.IDGenerator
	clock   application.Clock
	vendors *postgres.VendorRepo
	cats    *postgres.CategoryRepo
	prods   *postgres.ProductRepo
	sources *postgres.SourceRepo
	rels    *postgres.ReleaseRepo
	sums    *postgres.SummaryRepo
	ingest  application.IngestDeps
	check   application.CheckSourceDeps
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
		db:      db,
		ids:     ids,
		clock:   clock,
		vendors: postgres.NewVendorRepo(db),
		cats:    postgres.NewCategoryRepo(db),
		prods:   postgres.NewProductRepo(db),
		sources: postgres.NewSourceRepo(db),
		rels:    postgres.NewReleaseRepo(db),
		sums:    postgres.NewSummaryRepo(db),
	}
	events := platform.NewEventPublisher(platform.NewLogger(platform.Config{
		Service: "slice-test", LogLevel: "error", LogFormat: "json",
	}))

	w.ingest = application.IngestDeps{
		Sources: w.sources, Products: w.prods,
		Candidates: postgres.NewCandidateRepo(db),
		Releases:   w.rels,
		Evidence:   postgres.NewEvidenceRepo(db),
		Reviews:    postgres.NewReviewRepo(db),
		Artifacts:  store, Registry: reg,
		Queue:  postgres.NewQueue(db),
		Events: events, UoW: db, Clock: clock, IDs: ids,
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
	sync := application.NewSyncRegistry(
		registry.New(os.DirFS(repoRoot), "dataset"),
		w.vendors, w.cats, w.prods, w.sources, w.ids, w.clock)
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
