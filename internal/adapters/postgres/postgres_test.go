package postgres

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// These are integration tests. They run against a real PostgreSQL because everything
// worth testing in this package is a property of PostgreSQL: a partial unique index, a
// CHECK constraint, SKIP LOCKED, a generated tsvector column. A mock would assert that
// the adapter sends the SQL the adapter sends, which is worth nothing.
//
// They are skipped, loudly and by name, when no database is configured. A skipped test
// is never a passing test: `go test` reports SKIP, and CI is expected to fail a build
// whose adapter tests all skipped.

// skipMessage is the exact text a skipped run reports, so that a reader of CI output
// knows precisely what to set to make these tests run.
const skipMessage = "set FIRMSCOUT_TEST_DATABASE_URL to run PostgreSQL adapter tests"

var (
	testDatabaseURL string

	migrateOnce sync.Once
	migrateErr  error
)

// TestMain reads the database URL once for the whole package. It cannot call Skip
// itself -- there is no *testing.T at that point -- so it records the URL and each test
// skips through testDB.
func TestMain(m *testing.M) {
	testDatabaseURL = strings.TrimSpace(os.Getenv("FIRMSCOUT_TEST_DATABASE_URL"))
	os.Exit(m.Run())
}

// testDB returns a connected DB with the schema migrated and every table truncated.
//
// Truncation rather than a transaction-per-test is deliberate: several of these tests
// need two concurrent connections (queue leasing), which a single test transaction
// cannot provide.
func testDB(t *testing.T) *DB {
	t.Helper()
	if testDatabaseURL == "" {
		t.Skip(skipMessage)
	}

	ctx := context.Background()
	db, err := Open(ctx, Config{URL: testDatabaseURL, MaxConns: 8, ApplicationName: "firmscout-test"})
	if err != nil {
		t.Fatalf("connect to FIRMSCOUT_TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(db.Close)

	migrateOnce.Do(func() { migrateErr = NewMigrator(db).Up(ctx) })
	if migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}
	truncateAll(t, db)
	return db
}

// truncateAll empties every table except the migration ledger and the release_types
// vocabulary, which is seeded by the migration and referenced by foreign keys.
func truncateAll(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()

	rows, err := db.pool.Query(ctx,
		`SELECT tablename FROM pg_tables
          WHERE schemaname = 'public'
            AND tablename NOT IN ('schema_migrations', 'release_types')`)
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
		// Quote through pgx's identifier sanitiser. The names come from the
		// PostgreSQL catalogue rather than from a caller, but interpolating an
		// identifier without quoting it is a habit worth not having.
		names = append(names, pgx.Identifier{n}.Sanitize())
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(names) == 0 {
		return
	}
	if _, err := db.pool.Exec(ctx,
		`TRUNCATE `+strings.Join(names, ", ")+` RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// pool returns the raw pool for the few assertions that need to inspect or corrupt the
// database behind the adapter's back.
func pool(db *DB) *pgxpool.Pool { return db.pool }

// ---------------------------------------------------------------------------
// Fixture builders
// ---------------------------------------------------------------------------

func newVendor(id, slug string) domain.Vendor {
	return domain.Vendor{
		ID:          id,
		Slug:        slug,
		Name:        strings.ToUpper(slug[:1]) + slug[1:],
		HomepageURL: "https://" + slug + ".example",
		ManagedBy:   domain.ManagedByRegistry,
	}
}

func newProduct(id, vendorID, slug string) domain.Product {
	return domain.Product{
		ID:                 id,
		VendorID:           vendorID,
		Slug:               slug,
		Name:               slug,
		DefaultReleaseType: domain.ReleaseTypeFirmware,
		LifecycleStatus:    domain.LifecycleActive,
		ManagedBy:          domain.ManagedByRegistry,
	}
}

// newSource returns a source that is dispatchable: enabled, active, robots allowed,
// terms approved and due now. Each exclusion test takes this and breaks exactly one
// thing, so the test names what it is proving.
func newSource(id, vendorID, slug string) domain.Source {
	return domain.Source{
		ID:                    id,
		VendorID:              vendorID,
		Slug:                  slug,
		SourceType:            domain.SourceTypeHTMLPage,
		URL:                   "https://" + slug + ".example/changelog",
		Official:              true,
		QualityClass:          domain.QualityOfficialManufacturer,
		RobotsPolicyStatus:    domain.RobotsAllowed,
		TermsReviewStatus:     domain.TermsApproved,
		AuthenticationType:    domain.AuthNone,
		Enabled:               true,
		CollectorID:           "html.generic",
		CheckFrequencySeconds: 3600,
		Normalize: domain.NormalizeConfig{
			SectionSelector: "#changelog",
			Strip:           []string{"script", ".ads"},
		},
		NextCheckAt: time.Now().Add(-time.Hour),
		Health:      domain.SourceActive,
		Confidence:  0.9,
	}
}

func newEvidence(id, sourceID string) domain.Evidence {
	return domain.Evidence{
		ID:              id,
		SourceID:        sourceID,
		SourceURL:       "https://example.test/changelog",
		SourceType:      domain.SourceTypeHTMLPage,
		Official:        true,
		RetrievedAt:     time.Now().UTC(),
		Excerpt:         "RouterOS 7.24.2 released",
		RawValue:        "7.24.2",
		NormalizedValue: "7.24.2",
		DiscoveryMethod: domain.DiscoveryDeterministic,
		Confidence:      0.95,
	}
}

func mustVersion(t *testing.T, raw string) domain.VersionString {
	t.Helper()
	v, err := domain.NewVersionString(raw)
	if err != nil {
		t.Fatalf("build version %q: %v", raw, err)
	}
	return v
}

func mustExactDate(t *testing.T, y int, m time.Month, d int) domain.PartialDate {
	t.Helper()
	pd, err := domain.NewExactDate(y, m, d)
	if err != nil {
		t.Fatalf("build date: %v", err)
	}
	return pd
}

func mustMonthDate(t *testing.T, y int, m time.Month) domain.PartialDate {
	t.Helper()
	pd, err := domain.NewMonthDate(y, m)
	if err != nil {
		t.Fatalf("build date: %v", err)
	}
	return pd
}

// seedCatalog creates a vendor and a product and returns both.
func seedCatalog(t *testing.T, db *DB) (domain.Vendor, domain.Product) {
	t.Helper()
	ctx := context.Background()

	v := newVendor("ven_mikrotik", "mikrotik")
	if err := NewVendorRepo(db).Upsert(ctx, v); err != nil {
		t.Fatalf("seed vendor: %v", err)
	}
	p := newProduct("prd_ccr2004", v.ID, "ccr2004")
	if err := NewProductRepo(db).Upsert(ctx, p); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	return v, p
}

// seedEvidence creates a source and an evidence record referencing it, which every
// release needs.
func seedEvidence(t *testing.T, db *DB, vendorID string) domain.Evidence {
	t.Helper()
	ctx := context.Background()

	s := newSource("src_changelog", vendorID, "changelog")
	if err := NewSourceRepo(db).Upsert(ctx, s); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	e := newEvidence("ev_1", s.ID)
	if err := NewEvidenceRepo(db).Insert(ctx, e); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	return e
}

// applicationReviewItem builds a review item of a kind the schema's CHECK constraint
// accepts. Ambiguous product matches are the archetypal entry: FirmScout refuses to
// guess which of four Catalyst models a release note meant.
func applicationReviewItem(vendorID, productID string) application.ReviewItem {
	return application.ReviewItem{
		ID:            "rev_1",
		Kind:          "product_match_ambiguous",
		SubjectType:   "candidate_release",
		SubjectID:     "cand_1",
		VendorID:      vendorID,
		ProductID:     productID,
		Title:         "Catalyst 9300 matched four products",
		Detail:        "the release note names a family, not a model",
		Payload:       map[string]string{"candidates": "4"},
		PriorityScore: 70,
		SLAClass:      "high",
	}
}

// usageRecord builds one metered request with a quota weight of five, so that a
// double-counted retry is visible in the aggregate rather than hidden by a weight of
// one that happens to match the request count.
func usageRecord(id, idempotencyKey, apiKeyID string, at time.Time) application.UsageRecord {
	return application.UsageRecord{
		ID:             id,
		IdempotencyKey: idempotencyKey,
		ConsumerID:     "con_1",
		APIKeyID:       apiKeyID,
		Endpoint:       "/v1/products/{slug}/latest",
		Method:         "GET",
		StatusCode:     200,
		QuotaWeight:    5,
		DurationMS:     12,
		BytesOut:       2048,
		VendorSlug:     "mikrotik",
		ProductSlug:    "ccr2004",
		OccurredAt:     at,
	}
}

// mustYearDate builds a year-precision date, the coarsest precision a vendor can
// publish and the one a window boundary is most likely to get wrong.
func mustYearDate(t *testing.T, y int) domain.PartialDate {
	t.Helper()
	pd, err := domain.NewYearDate(y)
	if err != nil {
		t.Fatalf("build date: %v", err)
	}
	return pd
}
