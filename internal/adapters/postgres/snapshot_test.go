package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// A snapshot exists so that cloning this repository and running it produces a populated
// catalogue rather than an empty one. The property that makes that work is the
// round-trip: export, load into a catalogue that has never fetched anything, and get the
// same facts back. These tests run against a real PostgreSQL because every interesting
// part -- slug resolution, date anchoring, the derived summary -- is in the SQL.

// seedSnapshotFacts publishes two releases: one at exact-day precision and one at
// month precision, so a test can prove the format does not widen a date it was never
// given. Returns the vendor and product they belong to.
func seedSnapshotFacts(t *testing.T, db *DB) (domain.Vendor, domain.Product) {
	t.Helper()
	ctx := context.Background()

	v, p := seedCatalog(t, db)
	e := seedEvidence(t, db, v.ID)

	exact, err := domain.NewExactDate(2026, time.September, 4)
	if err != nil {
		t.Fatalf("exact date: %v", err)
	}
	month, err := domain.NewMonthDate(2026, time.February)
	if err != nil {
		t.Fatalf("month date: %v", err)
	}

	releases := NewReleaseRepo(db)
	for _, tc := range []struct {
		id, version string
		date        domain.PartialDate
		latest      bool
	}{
		{"rel_snapshot_exact0000000", "7.23.5", exact, true},
		{"rel_snapshot_month0000000", "7.19.0", month, false},
	} {
		version, err := domain.NewVersionString(tc.version)
		if err != nil {
			t.Fatalf("version %s: %v", tc.version, err)
		}
		rel := domain.Release{
			ID: tc.id, VendorID: v.ID, Version: version,
			ReleaseType: domain.ReleaseTypeEmbeddedOS, Channel: "stable",
			ReleaseDate: tc.date, EvidenceID: e.ID,
			FirstObservedAt: time.Now().UTC(), LastVerifiedAt: time.Now().UTC(),
			PublishedAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
		}
		mapping := domain.ReleaseProductMapping{
			ReleaseID: tc.id, ProductID: p.ID,
			Applicability:    domain.Applicability{Channel: "stable"},
			IsLatestObserved: tc.latest,
		}
		if err := releases.Insert(ctx, rel, []domain.ReleaseProductMapping{mapping}); err != nil {
			t.Fatalf("seed release %s: %v", tc.version, err)
		}
	}
	return v, p
}

// emptyTheCatalogue removes every observed fact while leaving the registry in place,
// which is exactly the state a fresh clone is in after `registry sync`: it knows the
// vocabulary and has collected nothing.
func emptyTheCatalogue(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{
		"release_product_mappings", "product_summaries", "releases", "evidence",
	} {
		if _, err := db.pool.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("empty %s: %v", table, err)
		}
	}
}

// TestSnapshotRoundTripsThroughAnEmptyCatalogue is the whole feature in one test.
func TestSnapshotRoundTripsThroughAnEmptyCatalogue(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seedSnapshotFacts(t, db)

	repo := NewSnapshotRepo(db)
	exported, err := repo.ExportReleaseFacts(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(exported) != 2 {
		t.Fatalf("exported %d facts, want 2", len(exported))
	}

	emptyTheCatalogue(t, db)

	imported, skipped, err := repo.ImportReleaseFacts(ctx, exported)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 2 || skipped != 0 {
		t.Fatalf("import reported %d imported / %d skipped, want 2 / 0", imported, skipped)
	}

	back, err := repo.ExportReleaseFacts(ctx)
	if err != nil {
		t.Fatalf("re-export: %v", err)
	}
	if len(back) != len(exported) {
		t.Fatalf("re-export returned %d facts, want %d", len(back), len(exported))
	}
	for i := range exported {
		w, g := exported[i], back[i]
		if w.Release.ID != g.Release.ID || w.Release.Version != g.Release.Version {
			t.Errorf("fact %d: %s/%s -> %s/%s", i, w.Release.ID, w.Release.Version, g.Release.ID, g.Release.Version)
		}
		if w.Release.ReleaseDate != g.Release.ReleaseDate {
			t.Errorf("fact %d date: %+v -> %+v", i, w.Release.ReleaseDate, g.Release.ReleaseDate)
		}
		if w.Evidence.Excerpt != g.Evidence.Excerpt || w.Evidence.SourceURL != g.Evidence.SourceURL {
			t.Errorf("fact %d lost its provenance: %+v -> %+v", i, w.Evidence, g.Evidence)
		}
		if len(g.Products) != 1 || g.Products[0].Slug != w.Products[0].Slug {
			t.Errorf("fact %d products: %+v -> %+v", i, w.Products, g.Products)
		}
	}
}

// TestSnapshotImportIsIdempotent. A local collection may have verified or withdrawn a
// release since the snapshot was taken, and a re-import that clobbered it would silently
// undo what the catalogue learned.
func TestSnapshotImportIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seedSnapshotFacts(t, db)

	repo := NewSnapshotRepo(db)
	facts, err := repo.ExportReleaseFacts(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	imported, skipped, err := repo.ImportReleaseFacts(ctx, facts)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if imported != 0 || skipped != len(facts) {
		t.Fatalf("re-import reported %d imported / %d skipped, want 0 / %d", imported, skipped, len(facts))
	}
}

// TestSnapshotImportRefusesAnUnknownProduct: the file carries facts, not vocabulary, so
// a product the registry has never heard of is an error naming the fix -- not a silently
// dropped record, which would be an import that "succeeds" while losing data.
func TestSnapshotImportRefusesAnUnknownProduct(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seedSnapshotFacts(t, db)

	repo := NewSnapshotRepo(db)
	facts, err := repo.ExportReleaseFacts(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	emptyTheCatalogue(t, db)
	facts[0].Products[0].Slug = "a-product-nobody-registered"

	if _, _, err := repo.ImportReleaseFacts(ctx, facts); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("import error = %v, want a not-found naming the missing product", err)
	}

	// Nothing was written: the import runs in one transaction precisely so that a bad
	// record cannot leave a half-loaded catalogue behind.
	var n int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM releases`).Scan(&n); err != nil {
		t.Fatalf("count releases: %v", err)
	}
	if n != 0 {
		t.Errorf("%d release(s) written despite a failed import; the transaction did not roll back", n)
	}
}

// TestSnapshotPreservesDatePrecision applies the date discipline to the file format: a
// month-precision release survives as month precision, never widened to a day the
// vendor did not publish, and comes back anchored the way the schema's CHECK requires.
func TestSnapshotPreservesDatePrecision(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seedSnapshotFacts(t, db)

	repo := NewSnapshotRepo(db)
	facts, err := repo.ExportReleaseFacts(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	var monthOnly *application.ReleaseFact
	for i := range facts {
		if facts[i].Release.ReleaseDate.Precision == string(domain.PrecisionMonthOnly) {
			monthOnly = &facts[i]
		}
	}
	if monthOnly == nil {
		t.Fatal("no month-precision release in the fixture; this test proves nothing")
	}
	if got := monthOnly.Release.ReleaseDate.Value; got != "2026-02" {
		t.Fatalf("month-precision release exported as %q; a day was invented", got)
	}

	emptyTheCatalogue(t, db)
	if _, _, err := repo.ImportReleaseFacts(ctx, facts); err != nil {
		t.Fatalf("import: %v", err)
	}

	var stored time.Time
	var precision string
	if err := db.pool.QueryRow(ctx,
		`SELECT release_date, release_date_precision FROM releases WHERE id = $1`,
		monthOnly.Release.ID).Scan(&stored, &precision); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if precision != string(domain.PrecisionMonthOnly) {
		t.Errorf("stored precision = %q, want month_only", precision)
	}
	if stored.Day() != 1 {
		t.Errorf("stored day = %d, want the month anchor 1", stored.Day())
	}
}

// TestSnapshotRefusesAFabricatedRecord: the file is editable by anyone, so the importer
// validates rather than trusting that the exporter wrote it.
func TestSnapshotRefusesAFabricatedRecord(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seedSnapshotFacts(t, db)

	repo := NewSnapshotRepo(db)
	original, err := repo.ExportReleaseFacts(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	emptyTheCatalogue(t, db)

	for _, tc := range []struct {
		name   string
		mutate func(*application.ReleaseFact)
	}{
		{"no evidence excerpt", func(f *application.ReleaseFact) { f.Evidence.Excerpt = "" }},
		{"no source url", func(f *application.ReleaseFact) { f.Evidence.SourceURL = "" }},
		{"no products", func(f *application.ReleaseFact) { f.Products = nil }},
		{"no vendor", func(f *application.ReleaseFact) { f.Vendor = "" }},
		{"a date value with no precision", func(f *application.ReleaseFact) {
			f.Release.ReleaseDate.Precision = string(domain.PrecisionUnknown)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edited := make([]application.ReleaseFact, len(original))
			copy(edited, original)
			edited[0].Products = append([]application.SnapshotMapping(nil), original[0].Products...)
			tc.mutate(&edited[0])

			if _, _, err := repo.ImportReleaseFacts(ctx, edited); err == nil {
				t.Fatal("a record with no provenance was imported")
			}
		})
	}
}
