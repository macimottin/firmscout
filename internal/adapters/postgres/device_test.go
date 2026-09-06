package postgres

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/macimottin/firmscout/internal/domain"
)

// A fleet's inventory is a list of model numbers, so these tests are all one question
// asked from different sides: can FirmScout hold a hardware model, say which operating
// system it runs, and be found by the string stamped on the chassis -- without ever
// claiming which release of that operating system belongs on it. See ADR-0024.

// seedDeviceCatalogue creates the vendor, the operating system the devices run, one
// architecture family and two device products. Two devices rather than one because the
// interesting assertions are about ordering and about exact replacement, and neither is
// observable with a single edge.
//
// The device products carry no default release type on purpose: a device with no
// version stream of its own publishes no release type, and leaving the column empty is
// what makes that absence data rather than a defaulted guess.
func seedDeviceCatalogue(t *testing.T, db *DB) (os, switchDevice, wirelessDevice domain.Product) {
	t.Helper()
	ctx := context.Background()

	v := newVendor("ven_mikrotik", "mikrotik")
	v.Name = "MikroTik"
	if err := NewVendorRepo(db).Upsert(ctx, v); err != nil {
		t.Fatalf("seed vendor: %v", err)
	}

	products := NewProductRepo(db)
	family := domain.ProductFamily{
		ID:       "fam_arm32",
		VendorID: v.ID,
		Slug:     "arm-32bit",
		Name:     "ARM 32bit",
	}
	if err := products.UpsertFamily(ctx, family); err != nil {
		t.Fatalf("seed family: %v", err)
	}

	os = domain.Product{
		ID:                 "prd_routeros",
		VendorID:           v.ID,
		Slug:               "mikrotik-routeros",
		Name:               "RouterOS",
		DefaultReleaseType: domain.ReleaseTypeEmbeddedOS,
		LifecycleStatus:    domain.LifecycleActive,
		SecurityCritical:   true,
	}
	switchDevice = domain.Product{
		ID:              "prd_crs328",
		VendorID:        v.ID,
		ProductFamilyID: family.ID,
		Slug:            "mikrotik-crs328-24p-4s-rm",
		Name:            "CRS328-24P-4S+RM",
		ModelIdentifier: "CRS328-24P-4S+RM",
		LifecycleStatus: domain.LifecycleUnknown,
	}
	wirelessDevice = domain.Product{
		ID:              "prd_hapbelite",
		VendorID:        v.ID,
		ProductFamilyID: family.ID,
		Slug:            "mikrotik-hap-be-lite",
		Name:            "hAP be lite",
		ModelIdentifier: "A42G-HbeP",
		LifecycleStatus: domain.LifecycleUnknown,
	}
	for _, p := range []domain.Product{os, switchDevice, wirelessDevice} {
		if err := products.Upsert(ctx, p); err != nil {
			t.Fatalf("seed product %s: %v", p.Slug, err)
		}
	}
	return os, switchDevice, wirelessDevice
}

// TestMigration00005ShapesTheDeviceSchema checks the objects 00005 adds, and checks
// them by their definition rather than by their name.
//
// The partial unique index is the reason: an index called
// release_mappings_family_latest_idx that indexed something else, or whose predicate
// differed from 00001's by a space, would pass a name check and enforce nothing. The
// predicate is the guarantee, so the predicate is what is asserted.
func TestMigration00005ShapesTheDeviceSchema(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	assertShape := func(t *testing.T, when string) {
		t.Helper()

		var indexDef string
		if err := pool(db).QueryRow(ctx,
			`SELECT indexdef FROM pg_indexes
              WHERE schemaname = 'public' AND indexname = 'release_mappings_family_latest_idx'`,
		).Scan(&indexDef); err != nil {
			t.Fatalf("%s: read release_mappings_family_latest_idx: %v", when, err)
		}
		for _, want := range []string{
			"UNIQUE INDEX",
			"product_family_id, COALESCE(channel, ''::text)",
			"WHERE ((is_latest_observed = true) AND (product_family_id IS NOT NULL))",
		} {
			if !strings.Contains(indexDef, want) {
				t.Errorf("%s: index definition %q does not contain %q", when, indexDef, want)
			}
		}

		var runsNotNull bool
		var runsDefault string
		if err := pool(db).QueryRow(ctx,
			`SELECT attnotnull, pg_get_expr(d.adbin, d.adrelid)
               FROM pg_attribute a
               JOIN pg_class c ON c.oid = a.attrelid
               LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
              WHERE c.relname = 'product_summaries' AND a.attname = 'runs'`,
		).Scan(&runsNotNull, &runsDefault); err != nil {
			t.Fatalf("%s: read product_summaries.runs: %v", when, err)
		}
		// NOT NULL with an empty-array default is what lets every reader treat the
		// column as a list without a null check, and makes a summary row written
		// before this migration read as "runs nothing", which is true of it.
		if !runsNotNull || !strings.Contains(runsDefault, "'[]'::jsonb") {
			t.Errorf("%s: runs is (notnull=%v, default=%q), want NOT NULL DEFAULT '[]'::jsonb", when, runsNotNull, runsDefault)
		}

		var hasModelIdentifier bool
		if err := pool(db).QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
                             WHERE table_name = 'product_summaries' AND column_name = 'model_identifier')`,
		).Scan(&hasModelIdentifier); err != nil {
			t.Fatalf("%s: check product_summaries.model_identifier: %v", when, err)
		}
		if !hasModelIdentifier {
			t.Errorf("%s: product_summaries has no model_identifier column", when)
		}

		var relationships int
		if err := pool(db).QueryRow(ctx,
			`SELECT count(*) FROM pg_tables
              WHERE schemaname = 'public' AND tablename = 'product_relationships'`).Scan(&relationships); err != nil {
			t.Fatalf("%s: check product_relationships: %v", when, err)
		}
		if relationships != 1 {
			t.Errorf("%s: product_relationships table count = %d, want 1", when, relationships)
		}
	}

	// testDB already migrated to head.
	assertShape(t, "after the first Up")

	// Every process start calls Up, so a second run must be a no-op rather than a
	// duplicate-object error.
	if err := NewMigrator(db).Up(ctx); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	assertShape(t, "after a second Up")

	var ledger int
	if err := pool(db).QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations
          WHERE version = '00005_product_relationships_and_device_summary.sql'`).Scan(&ledger); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	if ledger != 1 {
		t.Errorf("schema_migrations rows for 00005 = %d, want 1", ledger)
	}
}

// TestMigrationsApplyToAFreshDatabase runs the whole ledger from nothing on a database
// of its own.
//
// The shared test database reached head incrementally, which is the deployment case but
// not the Quick Start case: somebody cloning this repository creates an empty database
// and runs every migration in sequence, and a file that only applies on top of an
// already-populated schema would pass every other test in this package and fail the
// first thing a new contributor does.
func TestMigrationsApplyToAFreshDatabase(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := "firmscout_migrate_" + strings.ToLower(NewDB(nil, nil).newID(""))
	if _, err := pool(db).Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create a scratch database: %v", err)
	}

	u, err := url.Parse(testDatabaseURL)
	if err != nil {
		t.Fatalf("parse FIRMSCOUT_TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	fresh, err := Open(ctx, Config{URL: u.String(), MaxConns: 2, ApplicationName: "firmscout-migrate-test"})
	if err != nil {
		t.Fatalf("connect to the scratch database: %v", err)
	}
	closed := false
	closeFresh := func() {
		if !closed {
			fresh.Close()
			closed = true
		}
	}
	t.Cleanup(func() {
		closeFresh()
		// WITH (FORCE) rather than a retry loop: a test that leaves a database behind
		// makes the next run fail on a name it did not choose.
		if _, err := pool(db).Exec(ctx,
			`DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); err != nil {
			t.Errorf("drop the scratch database %s: %v", name, err)
		}
	})

	m := NewMigrator(fresh)
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up on an empty database: %v", err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("second Up on the fresh database: %v", err)
	}

	st, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.Pending) != 0 {
		t.Errorf("pending migrations = %v, want none", st.Pending)
	}
	var applied bool
	for _, v := range st.Applied {
		if v == "00005_product_relationships_and_device_summary.sql" {
			applied = true
		}
	}
	if !applied {
		t.Fatalf("00005 is not among the applied migrations: %v", st.Applied)
	}

	// The objects, not just the ledger row: a migration whose statements silently did
	// nothing would still be recorded as applied.
	var relationships, indexes, columns int
	if err := fresh.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM pg_tables
                  WHERE schemaname = 'public' AND tablename = 'product_relationships'),
                (SELECT count(*) FROM pg_indexes
                  WHERE schemaname = 'public' AND indexname = 'release_mappings_family_latest_idx'),
                (SELECT count(*) FROM information_schema.columns
                  WHERE table_name = 'product_summaries'
                    AND column_name IN ('model_identifier', 'runs'))`,
	).Scan(&relationships, &indexes, &columns); err != nil {
		t.Fatalf("inspect the fresh schema: %v", err)
	}
	if relationships != 1 || indexes != 1 || columns != 2 {
		t.Errorf("fresh schema = (%d table, %d index, %d columns), want (1, 1, 2)",
			relationships, indexes, columns)
	}

	closeFresh()
}

func TestReplaceRelationshipsIsExactReplacement(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	os, device, otherDevice := seedDeviceCatalogue(t, db)
	products := NewProductRepo(db)

	// A second operating system, so that the replacement below removes a real edge
	// rather than merely shortening a list to zero.
	swos := domain.Product{
		ID:                 "prd_swos",
		VendorID:           os.VendorID,
		Slug:               "mikrotik-swos",
		Name:               "SwOS",
		DefaultReleaseType: domain.ReleaseTypeEmbeddedOS,
		LifecycleStatus:    domain.LifecycleActive,
	}
	if err := products.Upsert(ctx, swos); err != nil {
		t.Fatalf("seed SwOS: %v", err)
	}

	both := []domain.ProductRelationship{
		{
			ToProductID:  swos.ID,
			Kind:         domain.RelationRunsOS,
			SourceNote:   `https://mikrotik.com/product/crs328_24p_4s_rm: Operating System "RouterOS / SwitchOS".`,
			RegistryPath: "dataset/products/mikrotik/devices/crs328-24p-4s-rm.yaml",
		},
		{
			ID:           "prl_fixed",
			ToProductID:  os.ID,
			Kind:         domain.RelationRunsOS,
			SourceNote:   `https://mikrotik.com/product/crs328_24p_4s_rm: Operating System "RouterOS / SwitchOS".`,
			ManagedBy:    domain.ManagedByRegistry,
			RegistryPath: "dataset/products/mikrotik/devices/crs328-24p-4s-rm.yaml",
		},
	}
	if err := products.ReplaceRelationships(ctx, device.ID, both); err != nil {
		t.Fatalf("ReplaceRelationships: %v", err)
	}

	got, err := products.ListRelationships(ctx, device.ID)
	if err != nil {
		t.Fatalf("ListRelationships: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("relationships = %d, want 2", len(got))
	}
	// Ordered by the target's slug, not by insertion order and not by id: two runs of
	// the same sync mint different ids, and an unstable order makes a registry diff
	// noise.
	if got[0].ToProductID != os.ID || got[1].ToProductID != swos.ID {
		t.Errorf("targets = [%s %s], want [%s %s] ordered by target slug",
			got[0].ToProductID, got[1].ToProductID, os.ID, swos.ID)
	}
	if got[0].ID != "prl_fixed" {
		t.Errorf("a supplied id was not preserved: %q", got[0].ID)
	}
	if got[1].ID == "" {
		t.Error("an edge with no id was stored without one being minted")
	}
	if got[0].FromProductID != device.ID {
		t.Errorf("FromProductID = %q, want the product the write was addressed to", got[0].FromProductID)
	}
	if got[0].Kind != domain.RelationRunsOS {
		t.Errorf("Kind = %q, want runs_os", got[0].Kind)
	}
	if got[0].ManagedBy != domain.ManagedByRegistry {
		t.Errorf("ManagedBy = %q, want registry (the default an unset value takes)", got[0].ManagedBy)
	}
	if got[0].RegistryPath == "" || got[0].SourceNote == "" {
		t.Errorf("provenance was lost: path %q, note %q", got[0].RegistryPath, got[0].SourceNote)
	}

	// The registry file is the source of truth: an edge deleted from it must stop
	// being asserted rather than linger and keep telling an operator this switch runs
	// an operating system somebody has since retracted.
	if err := products.ReplaceRelationships(ctx, device.ID, both[1:]); err != nil {
		t.Fatalf("second ReplaceRelationships: %v", err)
	}
	got, err = products.ListRelationships(ctx, device.ID)
	if err != nil {
		t.Fatalf("ListRelationships after replacement: %v", err)
	}
	if len(got) != 1 || got[0].ToProductID != os.ID {
		t.Fatalf("after replacement = %+v, want only the RouterOS edge", got)
	}

	// Replacing one product's edges must not touch another's.
	if err := products.ReplaceRelationships(ctx, otherDevice.ID, []domain.ProductRelationship{
		{ToProductID: os.ID, Kind: domain.RelationRunsOS},
	}); err != nil {
		t.Fatalf("ReplaceRelationships for the second device: %v", err)
	}
	if err := products.ReplaceRelationships(ctx, device.ID, nil); err != nil {
		t.Fatalf("ReplaceRelationships with an empty set: %v", err)
	}
	if got, err := products.ListRelationships(ctx, device.ID); err != nil || len(got) != 0 {
		t.Errorf("emptying one product's edges = (%v, %v), want none and no error", got, err)
	}
	if got, err := products.ListRelationships(ctx, otherDevice.ID); err != nil || len(got) != 1 {
		t.Errorf("the other device's edges = (%v, %v), want its one edge intact", got, err)
	}

	// Two documents claiming the same edge is a contributor error, and the database
	// says so rather than the product page rendering the operating system twice.
	err = products.ReplaceRelationships(ctx, device.ID, []domain.ProductRelationship{
		{ToProductID: os.ID, Kind: domain.RelationRunsOS},
		{ToProductID: os.ID, Kind: domain.RelationRunsOS},
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a duplicated edge = %v, want domain.ErrConflict", err)
	}
	// The failed replacement rolled back whole: the surviving edge is the one that was
	// there before, not a half-applied set.
	if got, err := products.ListRelationships(ctx, device.ID); err != nil || len(got) != 0 {
		t.Errorf("after a rejected replacement = (%v, %v), want the previous empty set", got, err)
	}
}

// TestProductRelationshipRulesAreEnforcedByTheDatabase writes rows the repository would
// have refused, straight through the pool.
//
// The point is that these are properties of the schema, not habits of the Go layer: an
// admin path, a psql session or a future writer that forgets to call Validate must hit
// the same wall.
func TestProductRelationshipRulesAreEnforcedByTheDatabase(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	os, device, _ := seedDeviceCatalogue(t, db)

	insert := func(id, from, to, kind string) error {
		_, err := pool(db).Exec(ctx,
			`INSERT INTO product_relationships (id, from_product_id, to_product_id, relation_kind)
             VALUES ($1, $2, $3, $4)`, id, from, to, kind)
		return err
	}

	cases := []struct {
		name  string
		id    string
		from  string
		to    string
		kind  string
		state string
	}{
		{
			name: "a product running itself",
			id:   "prl_self", from: device.ID, to: device.ID, kind: "runs_os",
			state: sqlstateCheckViolation,
		},
		{
			// The vocabulary widens by a decision record and a one-word migration,
			// never by a YAML file spelling a kind nobody has evidence for.
			name: "an undeclared relation kind",
			id:   "prl_kind", from: device.ID, to: os.ID, kind: "contains",
			state: sqlstateCheckViolation,
		},
		{
			name: "an edge pointing at a product that does not exist",
			id:   "prl_ghost", from: device.ID, to: "prd_nonexistent", kind: "runs_os",
			state: sqlstateForeignKeyViolation,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := insert(tc.id, tc.from, tc.to, tc.kind)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("PostgreSQL accepted %s: err = %v", tc.name, err)
			}
			if pgErr.Code != tc.state {
				t.Fatalf("SQLSTATE = %s (%s), want %s", pgErr.Code, pgErr.Message, tc.state)
			}
		})
	}

	if err := insert("prl_ok", device.ID, os.ID, "runs_os"); err != nil {
		t.Fatalf("a legitimate edge was rejected: %v", err)
	}
	err := insert("prl_dup", device.ID, os.ID, "runs_os")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlstateUniqueViolation {
		t.Fatalf("a duplicate (from, kind, to) = %v, want a unique violation", err)
	}
}

// TestDeleteOSWithDevicesIsRefused pins the asymmetry in 00005's two foreign keys.
//
// Deleting a device takes its edges with it, because an edge describes the device.
// Deleting an operating system that devices point at is refused, because the
// alternative is a set of device pages that silently stop naming any operating system
// while still claiming to be devices.
func TestDeleteOSWithDevicesIsRefused(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	os, device, _ := seedDeviceCatalogue(t, db)

	if err := NewProductRepo(db).ReplaceRelationships(ctx, device.ID, []domain.ProductRelationship{
		{ToProductID: os.ID, Kind: domain.RelationRunsOS},
	}); err != nil {
		t.Fatalf("ReplaceRelationships: %v", err)
	}

	_, err := pool(db).Exec(ctx, `DELETE FROM products WHERE id = $1`, os.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlstateForeignKeyViolation {
		t.Fatalf("deleting an operating system devices point at = %v, want a foreign-key violation", err)
	}

	// The device end cascades, so removing a device does not leave an edge behind.
	if _, err := pool(db).Exec(ctx, `DELETE FROM products WHERE id = $1`, device.ID); err != nil {
		t.Fatalf("deleting a device: %v", err)
	}
	var left int
	if err := pool(db).QueryRow(ctx,
		`SELECT count(*) FROM product_relationships WHERE from_product_id = $1`, device.ID).Scan(&left); err != nil {
		t.Fatalf("count surviving edges: %v", err)
	}
	if left != 0 {
		t.Errorf("edges surviving a deleted device = %d, want 0", left)
	}
}

// TestSummaryCarriesModelIdentifierAndRuns is the whole feature end to end on the read
// side: a device with no releases of its own is still a readable, searchable product
// page that names the operating system whose releases the operator is after -- and
// still claims no version for itself.
func TestSummaryCarriesModelIdentifierAndRuns(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	os, device, _ := seedDeviceCatalogue(t, db)
	products := NewProductRepo(db)
	releases := NewReleaseRepo(db)
	summaries := NewSummaryRepo(db)

	if err := products.ReplaceRelationships(ctx, device.ID, []domain.ProductRelationship{
		{ToProductID: os.ID, Kind: domain.RelationRunsOS},
	}); err != nil {
		t.Fatalf("ReplaceRelationships: %v", err)
	}
	if err := releases.RefreshProductSummary(ctx, device.ID); err != nil {
		t.Fatalf("RefreshProductSummary: %v", err)
	}
	// Refreshing twice must not duplicate the runs array or fail.
	if err := releases.RefreshProductSummary(ctx, device.ID); err != nil {
		t.Fatalf("second RefreshProductSummary: %v", err)
	}

	sum, err := summaries.Get(ctx, device.Slug)
	if err != nil {
		t.Fatalf("summary Get: %v", err)
	}
	if sum.ModelIdentifier != "CRS328-24P-4S+RM" {
		t.Errorf("ModelIdentifier = %q, want the vendor's published product code", sum.ModelIdentifier)
	}
	if sum.FamilyName != "ARM 32bit" {
		t.Errorf("FamilyName = %q, want ARM 32bit", sum.FamilyName)
	}
	if len(sum.Runs) != 1 {
		t.Fatalf("Runs = %+v, want exactly the RouterOS edge", sum.Runs)
	}
	if sum.Runs[0].Slug != os.Slug || sum.Runs[0].Name != os.Name || sum.Runs[0].Kind != domain.RelationRunsOS {
		t.Errorf("Runs[0] = %+v, want {mikrotik-routeros RouterOS runs_os}", sum.Runs[0])
	}

	// The honest half. FirmScout has no release mapped to this switch, so it claims
	// none -- rather than inheriting RouterOS's latest and telling an operator to
	// flash an image this hardware may not take. See ADR-0024 D4.
	if sum.ReleaseCount != 0 {
		t.Errorf("ReleaseCount = %d, want 0: no release is mapped to a device", sum.ReleaseCount)
	}
	if sum.LatestReleaseID != "" || sum.LatestRawVersion != "" {
		t.Errorf("a device reported a latest release (%q %q); it has none", sum.LatestReleaseID, sum.LatestRawVersion)
	}
	if len(sum.OfficialSources) != 0 {
		t.Errorf("OfficialSources = %v, want empty: no source has contributed a release to this device", sum.OfficialSources)
	}

	// An operating system's own summary is unchanged by any of this: it runs nothing,
	// and it has no model identifier.
	if err := releases.RefreshProductSummary(ctx, os.ID); err != nil {
		t.Fatalf("RefreshProductSummary for the operating system: %v", err)
	}
	osSum, err := summaries.Get(ctx, os.Slug)
	if err != nil {
		t.Fatalf("summary Get for the operating system: %v", err)
	}
	if len(osSum.Runs) != 0 || osSum.ModelIdentifier != "" {
		t.Errorf("the operating system's summary = {runs %+v, model %q}, want neither",
			osSum.Runs, osSum.ModelIdentifier)
	}

	// A refresh that runs before the edges are written records "runs nothing", which
	// is why the registry sync refreshes last. Proving it here means the ordering is a
	// documented property rather than an accident nobody would notice breaking.
	if err := products.ReplaceRelationships(ctx, device.ID, nil); err != nil {
		t.Fatalf("clear relationships: %v", err)
	}
	if err := releases.RefreshProductSummary(ctx, device.ID); err != nil {
		t.Fatalf("RefreshProductSummary after clearing: %v", err)
	}
	sum, err = summaries.Get(ctx, device.Slug)
	if err != nil {
		t.Fatalf("summary Get after clearing: %v", err)
	}
	if len(sum.Runs) != 0 {
		t.Errorf("Runs after the edge was retracted = %+v, want none", sum.Runs)
	}
}

// TestModelNumberAliasIsSearchable is the regression guard for the one thing a fleet
// manager actually does: paste the string stamped on the chassis.
//
// The wireless device is the load-bearing case. Its product code, A42G-HbeP, shares no
// lexeme with its name, "hAP be lite", so a hit on the code can only have come through
// the model_number alias flattened into aliases_text -- which is precisely the
// mechanism ADR-0024 D9 chose over adding model_identifier to the generated search
// vector.
func TestModelNumberAliasIsSearchable(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	_, switchDevice, wirelessDevice := seedDeviceCatalogue(t, db)
	products := NewProductRepo(db)
	releases := NewReleaseRepo(db)
	summaries := NewSummaryRepo(db)

	for _, d := range []domain.Product{switchDevice, wirelessDevice} {
		alias, err := domain.NewProductAlias("", d.ID, d.ModelIdentifier, domain.AliasModelNumber)
		if err != nil {
			t.Fatalf("build alias for %s: %v", d.Slug, err)
		}
		if err := products.ReplaceAliases(ctx, d.ID, []domain.ProductAlias{alias}); err != nil {
			t.Fatalf("seed alias for %s: %v", d.Slug, err)
		}
		if err := releases.RefreshProductSummary(ctx, d.ID); err != nil {
			t.Fatalf("RefreshProductSummary for %s: %v", d.Slug, err)
		}
	}

	cases := []struct {
		query string
		want  string
	}{
		{query: "CRS328-24P-4S+RM", want: switchDevice.Slug},
		{query: "A42G-HbeP", want: wirelessDevice.Slug},
		// Case is not part of a product code as a human types it.
		{query: "a42g-hbep", want: wirelessDevice.Slug},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got, err := summaries.Search(ctx, tc.query, 20)
			if err != nil {
				t.Fatalf("Search(%q): %v", tc.query, err)
			}
			if len(got) == 0 {
				t.Fatalf("Search(%q) returned nothing; a pasted model number must find the device", tc.query)
			}
			if got[0].ProductSlug != tc.want {
				t.Errorf("Search(%q) best hit = %q, want %q", tc.query, got[0].ProductSlug, tc.want)
			}
			if got[0].ModelIdentifier == "" {
				t.Error("the hit carries no model identifier to echo back to the caller")
			}
		})
	}
}

// TestFamilyLatestFlagIsUnique closes the hole 00005 exists to close.
//
// 00001's partial unique index excludes family-targeted rows from its predicate
// entirely, so before this migration an unlimited number of them could each claim to be
// latest for the same family and channel, and ClearLatestFlag's WHERE product_id = $1
// could never clear one. That was unreachable while zero families existed; registering
// the first families is what makes it reachable, so this asserts the mirror index does
// the work.
func TestFamilyLatestFlagIsUnique(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	os, _, _ := seedDeviceCatalogue(t, db)
	releases := NewReleaseRepo(db)
	e := seedEvidence(t, db, os.VendorID)

	observed := time.Now().UTC()
	first := newRelease(t, "rel_1", os.VendorID, e.ID, "7.24.2", "stable", observed)
	if err := releases.Insert(ctx, first, []domain.ReleaseProductMapping{{
		ID:               "rmap_fam_1",
		ReleaseID:        first.ID,
		ProductFamilyID:  "fam_arm32",
		Applicability:    domain.Applicability{Channel: "stable"},
		IsLatestObserved: true,
	}}); err != nil {
		t.Fatalf("first family-targeted mapping: %v", err)
	}

	second := newRelease(t, "rel_2", os.VendorID, e.ID, "7.24.3", "stable", observed.Add(time.Hour))
	err := releases.Insert(ctx, second, []domain.ReleaseProductMapping{{
		ID:               "rmap_fam_2",
		ReleaseID:        second.ID,
		ProductFamilyID:  "fam_arm32",
		Applicability:    domain.Applicability{Channel: "stable"},
		IsLatestObserved: true,
	}})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a second latest-observed mapping for the same family and channel = %v, want domain.ErrConflict", err)
	}

	// A different channel is a different claim and stays legal, exactly as it is for
	// product-targeted rows.
	third := newRelease(t, "rel_3", os.VendorID, e.ID, "7.23.5", "long-term", observed)
	if err := releases.Insert(ctx, third, []domain.ReleaseProductMapping{{
		ID:               "rmap_fam_3",
		ReleaseID:        third.ID,
		ProductFamilyID:  "fam_arm32",
		Applicability:    domain.Applicability{Channel: "long-term"},
		IsLatestObserved: true,
	}}); err != nil {
		t.Fatalf("a family mapping on another channel was rejected: %v", err)
	}
}
