// Package postgres is FirmScout's PostgreSQL persistence adapter.
//
// It implements the repository, unit-of-work and queue ports declared in
// internal/application. The adapter is deliberately not written to be portable to
// another database: it uses partial unique indexes, generated tsvector columns,
// pg_trgm, SELECT ... FOR UPDATE SKIP LOCKED and ON CONFLICT because those are the
// features that make the schema's invariants structural rather than conventional.
//
// Two rules hold everywhere in this package:
//
//  1. No driver error escapes. pgx.ErrNoRows becomes domain.ErrNotFound and a unique
//     violation becomes domain.ErrConflict, so a use case never has to know which
//     adapter produced a failure. See translate.
//  2. No SQL is built by string concatenation of caller data. Every value reaches
//     PostgreSQL as a bound parameter.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// Config describes how to reach PostgreSQL and how large a pool to keep.
//
// Every field except URL has a working default, because a misconfigured pool is a
// production incident rather than a compile error and the safe values are known.
type Config struct {
	// URL is a libpq connection string or postgres:// URL. It is the only required
	// field.
	URL string
	// MaxConns bounds the pool. Default 10.
	MaxConns int32
	// MinConns keeps warm connections so a burst does not pay TLS and auth latency.
	MinConns int32
	// MaxConnLifetime recycles connections so a long-lived pool does not pin an old
	// backend across a failover. Default one hour.
	MaxConnLifetime time.Duration
	// MaxConnIdleTime releases connections a quiet process is not using. Default 30
	// minutes.
	MaxConnIdleTime time.Duration
	// ConnectTimeout bounds the initial dial. Default 10 seconds.
	ConnectTimeout time.Duration
	// ApplicationName appears in pg_stat_activity, which is what makes "which service
	// is holding this lock" answerable.
	ApplicationName string
	// IDs mints identifiers for the rows this adapter has to create itself, such as
	// validation results and usage records, where the port gives it no id. A built-in
	// ULID generator is used when this is nil.
	IDs application.IDGenerator
}

func (c *Config) applyDefaults() {
	if c.MaxConns <= 0 {
		c.MaxConns = 10
	}
	if c.MinConns < 0 {
		c.MinConns = 0
	}
	if c.MaxConnLifetime <= 0 {
		c.MaxConnLifetime = time.Hour
	}
	if c.MaxConnIdleTime <= 0 {
		c.MaxConnIdleTime = 30 * time.Minute
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.ApplicationName == "" {
		c.ApplicationName = "firmscout"
	}
}

// DB owns the connection pool and provides the transaction plumbing every repository
// in this package shares. It implements application.UnitOfWork.
//
// Repositories hold a *DB rather than a pool, and resolve their executor per call
// through q, so the same repository value works inside and outside a transaction.
type DB struct {
	pool *pgxpool.Pool
	ids  application.IDGenerator
}

var _ application.UnitOfWork = (*DB)(nil)

// Open connects to PostgreSQL, verifies the connection and returns a ready DB. It
// fails rather than returning a pool that has never successfully connected, so a bad
// connection string is a start-up error instead of a request-time one.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	cfg.applyDefaults()
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("postgres: a connection URL is required")
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse connection URL: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["application_name"] = cfg.ApplicationName

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	return NewDB(pool, cfg.IDs), nil
}

// NewDB wraps an existing pool. It exists for callers that own pool construction --
// tests, and processes that share one pool across several adapters.
func NewDB(pool *pgxpool.Pool, ids application.IDGenerator) *DB {
	if ids == nil {
		ids = ulidGenerator{}
	}
	return &DB{pool: pool, ids: ids}
}

// Pool exposes the underlying pool for operational code such as health checks and
// metrics collection. Repositories never use it directly; they go through q so that an
// ambient transaction is honoured.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// Close releases every pooled connection. It is safe to call more than once.
func (d *DB) Close() {
	if d != nil && d.pool != nil {
		d.pool.Close()
	}
}

// Ping reports whether the database is reachable, for readiness probes.
func (d *DB) Ping(ctx context.Context) error {
	return translate(d.pool.Ping(ctx))
}

// txKey is the private context key under which an open transaction travels. It is
// unexported and of a unique type so nothing outside this package can inject or read
// a transaction, which is what keeps pgx out of the application layer.
type txKey struct{}

// querier is the intersection of *pgxpool.Pool and pgx.Tx that repositories need.
// Depending on it rather than on a concrete type is what lets one repository method
// serve both the transactional and the autocommit case.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

var (
	_ querier = (*pgxpool.Pool)(nil)
	_ querier = (pgx.Tx)(nil)
)

// q returns the executor for this call: the transaction carried by ctx when there is
// one, and the pool otherwise.
func (d *DB) q(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok && tx != nil {
		return tx
	}
	return d.pool
}

// newID mints an identifier with the given prefix for rows this adapter creates on its
// own behalf.
func (d *DB) newID(prefix string) string {
	if d == nil || d.ids == nil {
		return ulidGenerator{}.NewID(prefix)
	}
	return d.ids.NewID(prefix)
}

// Within runs fn inside a single database transaction.
//
// The transaction is placed in the context fn receives; repositories pick it up from
// there, so a use case never handles a pgx.Tx. fn returning an error rolls back and
// that error is returned unchanged, so errors.Is against a domain sentinel still works
// through the transaction boundary. A panic also rolls back and is re-raised.
//
// Calls nest: when ctx already carries a transaction, fn joins it rather than opening
// a second one. Two transactions on two connections would deadlock against each other
// as readily as they would commit, and a nested unit of work almost always means "make
// sure this is atomic", not "make this independently durable".
func (d *DB) Within(ctx context.Context, fn func(ctx context.Context) error) error {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok && tx != nil {
		return fn(ctx)
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return translate(err)
	}

	committed := false
	defer func() {
		if !committed {
			// Use a background context so a cancelled request context cannot
			// prevent the rollback from being sent.
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()

	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return translate(err)
	}
	committed = true
	return nil
}

// ---------------------------------------------------------------------------
// Error translation
// ---------------------------------------------------------------------------

// dbError carries a domain sentinel together with the driver error that produced it,
// so errors.Is finds the sentinel while an operator still sees the PostgreSQL detail
// in the message.
type dbError struct {
	sentinel error
	cause    error
}

func (e *dbError) Error() string {
	if e.cause == nil {
		return "postgres: " + e.sentinel.Error()
	}
	return "postgres: " + e.sentinel.Error() + ": " + e.cause.Error()
}

// Unwrap returns both the sentinel and the cause so errors.Is matches the sentinel and
// errors.As can still reach a *pgconn.PgError for diagnostics.
func (e *dbError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.sentinel}
	}
	return []error{e.sentinel, e.cause}
}

// PostgreSQL SQLSTATE codes this adapter recognises.
const (
	sqlstateUniqueViolation     = "23505"
	sqlstateForeignKeyViolation = "23503"
	sqlstateNotNullViolation    = "23502"
	sqlstateCheckViolation      = "23514"
	sqlstateExclusionViolation  = "23P01"
	sqlstateSerializationFail   = "40001"
	sqlstateDeadlockDetected    = "40P01"
)

// translate converts a driver error into a domain error at this package's boundary.
//
// The mapping is deliberate rather than exhaustive:
//
//   - pgx.ErrNoRows becomes domain.ErrNotFound, and the driver error is dropped
//     entirely so that pgx.ErrNoRows cannot escape this package even through
//     errors.Is.
//   - A unique or exclusion violation becomes domain.ErrConflict: the caller asked
//     for something the schema already forbids.
//   - A check, not-null or foreign-key violation becomes domain.ErrValidation. These
//     are the schema rejecting a value -- a month-precision date anchored off day 1,
//     a release with no evidence -- which is a validation failure, not a conflict.
//   - A serialization failure or deadlock is left as-is and wrapped only for context,
//     because the correct response is to retry the whole unit of work, and pretending
//     it was a conflict would hide that.
//
// Any other error is returned unchanged. Translating errors that have no domain
// meaning would only obscure them.
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &dbError{sentinel: domain.ErrNotFound}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case sqlstateUniqueViolation, sqlstateExclusionViolation:
			return &dbError{sentinel: domain.ErrConflict, cause: err}
		case sqlstateCheckViolation, sqlstateNotNullViolation, sqlstateForeignKeyViolation:
			return &dbError{sentinel: domain.ErrValidation, cause: err}
		case sqlstateSerializationFail, sqlstateDeadlockDetected:
			return err
		}
	}
	return err
}

// wrap adds the failing operation to a translated error. Every repository method
// returns errors through it, so a failure names the query that produced it.
// isNoRows reports whether err is pgx's empty-result sentinel, before translate has
// converted it. It exists for the callers that treat "not there" as a normal branch
// rather than a failure.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, translate(err))
}

// notFound builds a domain.ErrNotFound for the case where a statement legitimately
// affected no rows, which pgx reports as a zero command tag rather than an error.
func notFound(op string) error {
	return fmt.Errorf("%s: %w", op, &dbError{sentinel: domain.ErrNotFound})
}

// ---------------------------------------------------------------------------
// Identifiers
// ---------------------------------------------------------------------------

// ulidGenerator is the fallback application.IDGenerator used when a caller supplies
// none. It produces prefixed, lexicographically sortable identifiers of the shape
// "src_01JQ...", which matters because several list queries paginate on id order.
type ulidGenerator struct{}

var _ application.IDGenerator = ulidGenerator{}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns prefix + "_" + a 26-character Crockford base32 ULID.
func (ulidGenerator) NewID(prefix string) string {
	var b [16]byte
	ms := uint64(time.Now().UTC().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	// crypto/rand.Read never fails on any supported platform; it panics internally
	// on a broken entropy source rather than returning an error.
	_, _ = rand.Read(b[6:])

	// Encode the 128 bits as 26 base32 characters, most significant first.
	out := make([]byte, 26)
	var acc, bits uint32
	idx := 25
	for i := 15; i >= 0; i-- {
		acc |= uint32(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[idx] = crockford[acc&0x1f]
			idx--
			acc >>= 5
			bits -= 5
		}
	}
	if idx >= 0 {
		out[idx] = crockford[acc&0x1f]
	}
	if prefix == "" {
		return string(out)
	}
	return prefix + "_" + string(out)
}

// ---------------------------------------------------------------------------
// Null and value helpers
// ---------------------------------------------------------------------------

// nullString maps Go's "no value is the empty string" convention onto SQL NULL. The
// schema uses NULL for absent optional text throughout, and storing an empty string instead would
// make "the vendor published no support URL" indistinguishable from "the vendor
// published an empty one".
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// nullInt maps a zero int to NULL, for optional numeric columns whose absence is
// meaningful (min_frequency_seconds) rather than a real zero.
func nullInt(i int) *int {
	if i == 0 {
		return nil
	}
	return &i
}

func nullBool(b *bool) *bool { return b }

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func tim(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

func integer(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func float(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func int64val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// dateParams renders a PartialDate as the (anchor, precision) pair the schema stores.
// The anchor is NULL for an unknown date, which is what the release_date_precision
// CHECK constraints require.
func dateParams(d domain.PartialDate) (*time.Time, string) {
	if !d.Known() {
		return nil, string(domain.PrecisionUnknown)
	}
	a := d.Anchor().UTC()
	return &a, string(d.Precision())
}

// partialDate is the inverse of dateParams. It goes through domain.NewPartialDate
// rather than assembling the value directly, so a row that somehow violated the
// anchoring rule is rejected on read instead of becoming a date with false precision.
func partialDate(anchor *time.Time, precision string) (domain.PartialDate, error) {
	p := domain.DatePrecision(precision)
	if p == "" {
		p = domain.PrecisionUnknown
	}
	if p == domain.PrecisionUnknown || anchor == nil {
		return domain.UnknownDate, nil
	}
	return domain.NewPartialDate(*anchor, p)
}

// publicationDateParam renders a publication date for storage.
//
// The schema gives publication_date a DATE column but no precision column, unlike
// release_date. Rather than store a month- or year-precision anchor that would read
// back as a real day -- manufacturing precision the vendor never published, which
// ADR-0017 exists to prevent -- only an exact-day publication date is persisted. A
// coarser one round-trips as unknown. This is a known schema gap, recorded here so it
// is fixed in the schema rather than papered over in the adapter.
func publicationDateParam(d domain.PartialDate) *time.Time {
	if _, ok := d.ExactDay(); !ok {
		return nil
	}
	a := d.Anchor().UTC()
	return &a
}

// publicationDate reads back a stored publication date. Because only exact-day values
// are ever written, a non-NULL column is always day-precise.
func publicationDate(anchor *time.Time) (domain.PartialDate, error) {
	if anchor == nil {
		return domain.UnknownDate, nil
	}
	return domain.NewPartialDate(*anchor, domain.PrecisionExactDay)
}

// version rebuilds a VersionString from the two columns the schema stores. Both are
// NOT NULL in every table that has them, so an error here means the row is corrupt.
func version(raw, normalized string) (domain.VersionString, error) {
	return domain.NewVersionStringWithNormalized(raw, normalized)
}

// clampLimit bounds a caller-supplied page size. A limit of zero or less means "use
// the default"; anything above max is capped, because an unbounded page is a denial of
// service the API layer should not be able to request by accident.
func clampLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

// ---------------------------------------------------------------------------
// Cursors
// ---------------------------------------------------------------------------

// Cursors are keyset pagination positions, not offsets: an offset paginator silently
// skips or repeats rows when the underlying table changes between pages, which for a
// catalogue that is continuously ingesting is not a hypothetical.

const cursorSep = "\x1f"

// encodeCursor packs the sort-key values of the last row of a page into an opaque
// token. It is base64url so it survives a query string unescaped.
func encodeCursor(parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, cursorSep)))
}

// decodeCursor unpacks a token into exactly n parts. An unparseable or wrong-shaped
// cursor is an error rather than a silent restart from the beginning, because silently
// serving page one in response to a corrupted "next page" token is how a client ends
// up in an infinite loop.
func decodeCursor(cursor string, n int) ([]string, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("postgres: malformed cursor: %w", domain.ErrValidation)
	}
	parts := strings.Split(string(raw), cursorSep)
	if len(parts) != n {
		return nil, fmt.Errorf("postgres: malformed cursor: %w", domain.ErrValidation)
	}
	return parts, nil
}
