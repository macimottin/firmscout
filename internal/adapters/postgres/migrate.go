package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	migrations "github.com/macimottin/firmscout/database/migrations"
)

// Migrator applies the embedded SQL migrations.
//
// It is deliberately about two hundred lines rather than a dependency. A migration
// runner has to do exactly three things -- order the files, run each one once, and
// record that it ran -- and the cost of a third-party tool is not the code it saves
// but the fact that it becomes the only thing that can read the migration history. The
// files keep goose's annotation format so `goose` and `psql` remain usable against the
// same directory.
type Migrator struct {
	db  *DB
	fsy fs.FS
}

// NewMigrator returns a Migrator over the embedded migration files.
func NewMigrator(db *DB) *Migrator {
	return &Migrator{db: db, fsy: migrations.FS}
}

// NewMigratorFS returns a Migrator over an arbitrary filesystem of *.sql files. It
// exists so tests can exercise the runner against a fixture without shipping fixture
// migrations in the binary.
func NewMigratorFS(db *DB, fsys fs.FS) *Migrator {
	return &Migrator{db: db, fsy: fsys}
}

// migrationsTable is created on first run and is the only state the runner keeps.
const migrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// Migration is one parsed migration file.
type Migration struct {
	// Version is the filename, which is also the ordering key. Using the whole
	// filename rather than the numeric prefix means a version can never be recorded
	// as applied while a differently named file with the same number sits unapplied.
	Version string
	// Up holds the statements that move the schema forward, in file order.
	Up []string
	// Down holds the reverse statements. The runner does not apply them: rolling a
	// schema back automatically is how data is lost during an incident. They are
	// parsed so that they are at least syntactically present, and exposed for tests
	// and for tooling that wants to render them.
	Down []string
}

// Status reports which migrations have been applied and which are pending, newest
// state first for the applied set. It is what a deployment health check reads to
// decide whether the running code matches the schema.
type Status struct {
	Applied []string
	Pending []string
}

// Up applies every pending migration in filename order.
//
// Each migration runs in its own transaction together with the INSERT that records it,
// so a migration and the fact that it was applied can never disagree: either both are
// committed or neither is. A failure stops the run, leaving later migrations pending.
//
// Up is idempotent. Running it against an already-current database performs one query
// and applies nothing.
func (m *Migrator) Up(ctx context.Context) error {
	if err := m.ensureTable(ctx); err != nil {
		return err
	}

	all, err := m.load()
	if err != nil {
		return err
	}
	applied, err := m.appliedSet(ctx)
	if err != nil {
		return err
	}

	for _, mig := range all {
		if applied[mig.Version] {
			continue
		}
		if err := m.apply(ctx, mig); err != nil {
			return fmt.Errorf("postgres: migration %s: %w", mig.Version, err)
		}
	}
	return nil
}

// Status returns the applied and pending migration versions.
func (m *Migrator) Status(ctx context.Context) (Status, error) {
	if err := m.ensureTable(ctx); err != nil {
		return Status{}, err
	}
	all, err := m.load()
	if err != nil {
		return Status{}, err
	}
	appliedSet, err := m.appliedSet(ctx)
	if err != nil {
		return Status{}, err
	}

	var st Status
	for _, mig := range all {
		if appliedSet[mig.Version] {
			st.Applied = append(st.Applied, mig.Version)
		} else {
			st.Pending = append(st.Pending, mig.Version)
		}
	}
	// A version recorded in the table but no longer on disk is reported too: it means
	// the database is ahead of the binary, which is the dangerous direction.
	known := make(map[string]bool, len(all))
	for _, mig := range all {
		known[mig.Version] = true
	}
	for v := range appliedSet {
		if !known[v] {
			st.Applied = append(st.Applied, v)
		}
	}
	sort.Strings(st.Applied)
	sort.Strings(st.Pending)
	return st, nil
}

// Migrations returns the parsed migration set, for tooling and tests.
func (m *Migrator) Migrations() ([]Migration, error) { return m.load() }

func (m *Migrator) ensureTable(ctx context.Context) error {
	_, err := m.db.q(ctx).Exec(ctx, migrationsTable)
	return wrap("migrate.ensureTable", err)
}

func (m *Migrator) appliedSet(ctx context.Context) (map[string]bool, error) {
	rows, err := m.db.q(ctx).Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, wrap("migrate.applied", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, wrap("migrate.applied.scan", err)
		}
		out[v] = true
	}
	return out, wrap("migrate.applied.rows", rows.Err())
}

// apply runs one migration and records it, in a single transaction.
func (m *Migrator) apply(ctx context.Context, mig Migration) error {
	tx, err := m.db.pool.Begin(ctx)
	if err != nil {
		return translate(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	for i, stmt := range mig.Up {
		// Passing no arguments makes pgx use the simple protocol, which is what
		// allows a StatementBegin/StatementEnd block containing many semicolon
		// separated statements to be executed as one unit.
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("statement %d: %w", i+1, translate(err))
		}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`, mig.Version); err != nil {
		return translate(err)
	}
	return translate(tx.Commit(ctx))
}

// load reads and parses every *.sql file, sorted by filename.
func (m *Migrator) load() ([]Migration, error) {
	names, err := fs.Glob(m.fsy, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("postgres: list migrations: %w", err)
	}
	sort.Strings(names)

	out := make([]Migration, 0, len(names))
	for _, name := range names {
		content, err := fs.ReadFile(m.fsy, name)
		if err != nil {
			return nil, fmt.Errorf("postgres: read migration %s: %w", name, err)
		}
		up, down, err := parseGoose(string(content))
		if err != nil {
			return nil, fmt.Errorf("postgres: parse migration %s: %w", name, err)
		}
		if len(up) == 0 {
			return nil, fmt.Errorf("postgres: migration %s has no Up statements", name)
		}
		out = append(out, Migration{Version: name, Up: up, Down: down})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// goose annotation parsing
// ---------------------------------------------------------------------------

const (
	annUp             = "+goose up"
	annDown           = "+goose down"
	annStatementBegin = "+goose statementbegin"
	annStatementEnd   = "+goose statementend"
	annNoTransaction  = "+goose no transaction"
)

// parseGoose splits a migration file into its Up and Down statements.
//
// Two statement forms are recognised, and the difference matters:
//
//   - Inside a StatementBegin/StatementEnd pair, everything is one statement no
//     matter how many semicolons it contains. This is how a function body, a DO block
//     or -- as in FirmScout's initial migration -- a whole schema is kept atomic.
//   - Outside such a pair, a semicolon at the end of a line terminates a statement.
//
// Semicolons inside string literals and dollar-quoted bodies are respected, because
// splitting a CHECK (slug ~ '^[a-z]+;?$') in half would produce two invalid statements
// and a confusing syntax error rather than a clear one.
func parseGoose(content string) (up, down []string, err error) {
	const (
		sectionNone = iota
		sectionUp
		sectionDown
	)
	section := sectionNone
	inBlock := false

	var buf strings.Builder
	var dollarTag string
	inSingle := false

	flush := func(target *[]string) {
		s := strings.TrimSpace(buf.String())
		buf.Reset()
		if s == "" {
			return
		}
		*target = append(*target, s)
	}
	current := func() *[]string {
		if section == sectionDown {
			return &down
		}
		return &up
	}

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)

		// Annotations are comments, so they are only meaningful outside a quoted
		// region. A line beginning with "--" inside a dollar-quoted body is content.
		if dollarTag == "" && !inSingle && strings.HasPrefix(trimmed, "--") {
			ann := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "--")))
			switch {
			case ann == annUp:
				if buf.Len() > 0 {
					flush(current())
				}
				section = sectionUp
				continue
			case ann == annDown:
				if buf.Len() > 0 {
					flush(current())
				}
				section = sectionDown
				continue
			case ann == annStatementBegin:
				if inBlock {
					return nil, nil, fmt.Errorf("nested StatementBegin")
				}
				flush(current())
				inBlock = true
				continue
			case ann == annStatementEnd:
				if !inBlock {
					return nil, nil, fmt.Errorf("StatementEnd without StatementBegin")
				}
				flush(current())
				inBlock = false
				continue
			case ann == annNoTransaction:
				// Recognised and ignored: this runner always uses a transaction,
				// and the initial schema does not need CONCURRENTLY.
				continue
			case strings.HasPrefix(ann, "+goose"):
				// An unknown goose directive is skipped rather than treated as SQL.
				continue
			}
			if section == sectionNone {
				continue
			}
			// An ordinary comment inside a section is kept, so the statement text
			// stays readable in an error message.
			buf.WriteString(line)
			buf.WriteString("\n")
			continue
		}

		if section == sectionNone {
			continue
		}

		buf.WriteString(line)
		buf.WriteString("\n")

		if inBlock {
			continue
		}

		// Outside a StatementBegin block, decide whether this line terminated a
		// statement by scanning it for an unquoted trailing semicolon.
		inSingle, dollarTag = scanLine(line, inSingle, dollarTag)
		if !inSingle && dollarTag == "" && endsStatement(line) {
			flush(current())
		}
	}

	if inBlock {
		return nil, nil, fmt.Errorf("unterminated StatementBegin")
	}
	if buf.Len() > 0 {
		flush(current())
	}
	return up, down, nil
}

// scanLine advances the single-quote and dollar-quote state across one line and
// returns the state at end of line. A -- comment ends the scan, since nothing after it
// on that line is SQL.
func scanLine(line string, inSingle bool, dollarTag string) (bool, string) {
	for i := 0; i < len(line); i++ {
		switch {
		case dollarTag != "":
			if strings.HasPrefix(line[i:], dollarTag) {
				i += len(dollarTag) - 1
				dollarTag = ""
			}
		case inSingle:
			if line[i] == '\'' {
				// '' is an escaped quote, not a terminator.
				if i+1 < len(line) && line[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
		default:
			if line[i] == '\'' {
				inSingle = true
				continue
			}
			if line[i] == '-' && i+1 < len(line) && line[i+1] == '-' {
				// Rest of the line is a comment.
				return inSingle, dollarTag
			}
			if line[i] == '$' {
				if tag, ok := dollarQuoteTag(line[i:]); ok {
					dollarTag = tag
					i += len(tag) - 1
				}
			}
		}
	}
	return inSingle, dollarTag
}

// dollarQuoteTag recognises a $$ or $tag$ opener at the start of s.
func dollarQuoteTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '$' {
			return s[:i+1], true
		}
		alnum := c == '_' ||
			(c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(i > 1 && c >= '0' && c <= '9')
		if !alnum {
			return "", false
		}
	}
	return "", false
}

// endsStatement reports whether the line's last non-comment, non-whitespace character
// is a semicolon.
func endsStatement(line string) bool {
	if idx := strings.Index(line, "--"); idx >= 0 {
		line = line[:idx]
	}
	line = strings.TrimSpace(line)
	return strings.HasSuffix(line, ";")
}
