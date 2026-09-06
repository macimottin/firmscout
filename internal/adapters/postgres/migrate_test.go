package postgres

import (
	"context"
	"strings"
	"testing"
)

func TestMigrateUpIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	m := NewMigrator(db)

	// testDB already ran Up once. Running it again must be a no-op rather than a
	// duplicate-object error, because every process start calls it.
	if err := m.Up(ctx); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("third Up: %v", err)
	}

	st, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.Pending) != 0 {
		t.Errorf("pending migrations after Up = %v, want none", st.Pending)
	}
	if len(st.Applied) == 0 {
		t.Fatal("Status reported no applied migrations")
	}
	if st.Applied[0] != "00001_initial.sql" {
		t.Errorf("first applied migration = %q, want 00001_initial.sql", st.Applied[0])
	}

	// The ledger must hold exactly one row per migration; a second Up that inserted a
	// duplicate would violate the primary key, but a runner that skipped the insert
	// entirely would also pass Up twice, so count explicitly.
	var n int
	if err := pool(db).QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version = '00001_initial.sql'`).Scan(&n); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	if n != 1 {
		t.Errorf("schema_migrations rows for the initial migration = %d, want 1", n)
	}

	// And the schema is actually there.
	var tables int
	if err := pool(db).QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'public'`).Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables < 29 {
		t.Errorf("public tables = %d, want at least the 29 of the initial schema", tables)
	}
}

// TestMigration00002UpAndDown proves 00002 applies cleanly over 00001 (which testDB
// already did, since it always migrates to head) and that its Down statements actually
// remove what Up added, rather than merely being syntactically present.
//
// The runner never applies Down automatically -- rolling a schema back automatically is
// how data is lost during an incident (migrate.go's Migration doc comment) -- so this
// test runs the parsed Down statements directly, and re-applies Up afterwards to leave
// the shared test database in the state every other test in this package expects.
func TestMigration00002UpAndDown(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	hasTable := func(name string) bool {
		var n int
		if err := pool(db).QueryRow(ctx,
			`SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = $1`,
			name).Scan(&n); err != nil {
			t.Fatalf("check table %s: %v", name, err)
		}
		return n == 1
	}
	hasActorAuthenticated := func() bool {
		var exists bool
		if err := pool(db).QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
                             WHERE table_name = 'audit_events' AND column_name = 'actor_authenticated')`,
		).Scan(&exists); err != nil {
			t.Fatalf("check audit_events.actor_authenticated: %v", err)
		}
		return exists
	}
	hasAdvisoryType := func() bool {
		var exists bool
		if err := pool(db).QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM release_types WHERE id = 'advisory')`).Scan(&exists); err != nil {
			t.Fatalf("check release_types: %v", err)
		}
		return exists
	}

	if !hasTable("source_observations") || !hasTable("source_conflicts") {
		t.Fatal("00002's tables are missing after Up")
	}
	if !hasActorAuthenticated() {
		t.Fatal("00002 did not add audit_events.actor_authenticated")
	}
	if !hasAdvisoryType() {
		t.Fatal("00002 did not seed the advisory release type")
	}

	migs, err := NewMigrator(db).Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	var mig *Migration
	for i := range migs {
		if migs[i].Version == "00002_conflicts_and_review.sql" {
			mig = &migs[i]
		}
	}
	if mig == nil {
		t.Fatal("00002_conflicts_and_review.sql is not among the embedded migrations")
	}
	if len(mig.Down) == 0 {
		t.Fatal("00002 has no Down statements")
	}

	tx, err := pool(db).Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	for i, stmt := range mig.Down {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("down statement %d (%q): %v", i+1, stmt, err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, mig.Version); err != nil {
		t.Fatalf("remove ledger row for 00002: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the down migration: %v", err)
	}
	committed = true

	if hasTable("source_observations") || hasTable("source_conflicts") {
		t.Error("Down left 00002's tables behind")
	}
	if hasActorAuthenticated() {
		t.Error("Down left audit_events.actor_authenticated behind")
	}
	if hasAdvisoryType() {
		t.Error("Down left the advisory release type behind")
	}

	// Restore head so every other test in this package still finds the schema it
	// expects; testDB's migrateOnce ran Up exactly once for the whole package and will
	// not run it again.
	if err := NewMigrator(db).Up(ctx); err != nil {
		t.Fatalf("re-apply Up after Down: %v", err)
	}
	if !hasTable("source_observations") || !hasTable("source_conflicts") {
		t.Fatal("re-applying Up did not restore 00002's tables")
	}
	if !hasActorAuthenticated() || !hasAdvisoryType() {
		t.Fatal("re-applying Up did not restore 00002's column and seed row")
	}
}

func TestParseGooseSplitsSections(t *testing.T) {
	const src = `
-- +goose Up
-- +goose StatementBegin
CREATE TABLE a (id TEXT PRIMARY KEY);
CREATE TABLE b (id TEXT PRIMARY KEY);
-- +goose StatementEnd

CREATE INDEX a_idx ON a (id);

-- +goose Down
DROP TABLE b;
DROP TABLE a;
`
	up, down, err := parseGoose(src)
	if err != nil {
		t.Fatalf("parseGoose: %v", err)
	}
	if len(up) != 2 {
		t.Fatalf("up statements = %d (%q), want 2: the block counts as one", len(up), up)
	}
	if !strings.Contains(up[0], "CREATE TABLE a") || !strings.Contains(up[0], "CREATE TABLE b") {
		t.Errorf("first up statement did not keep the whole block: %q", up[0])
	}
	if !strings.Contains(up[1], "CREATE INDEX") {
		t.Errorf("second up statement = %q, want the index", up[1])
	}
	if len(down) != 2 {
		t.Errorf("down statements = %d (%q), want 2", len(down), down)
	}
}

func TestParseGooseKeepsSemicolonsInsideLiterals(t *testing.T) {
	const src = `
-- +goose Up
CREATE TABLE t (slug TEXT CHECK (slug ~ '^[a-z;]+$'));
`
	up, _, err := parseGoose(src)
	if err != nil {
		t.Fatalf("parseGoose: %v", err)
	}
	if len(up) != 1 {
		t.Fatalf("up statements = %d (%q), want 1: the semicolon is inside a literal", len(up), up)
	}
}

func TestParseGooseRejectsUnterminatedBlock(t *testing.T) {
	const src = `
-- +goose Up
-- +goose StatementBegin
SELECT 1;
`
	if _, _, err := parseGoose(src); err == nil {
		t.Fatal("parseGoose accepted an unterminated StatementBegin")
	}
}

func TestEmbeddedMigrationsParse(t *testing.T) {
	// This one needs no database: it proves the shipped files are parseable, which is
	// the failure that would otherwise only appear at deployment time.
	migs, err := NewMigrator(&DB{}).Migrations()
	if err != nil {
		t.Fatalf("parse embedded migrations: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("no embedded migrations found")
	}
	for _, m := range migs {
		if len(m.Up) == 0 {
			t.Errorf("migration %s has no Up statements", m.Version)
		}
		if len(m.Down) == 0 {
			t.Errorf("migration %s has no Down statements", m.Version)
		}
	}
}
