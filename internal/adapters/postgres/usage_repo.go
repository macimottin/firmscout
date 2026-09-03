package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// UsageRepo implements both application.UsageRecorder and
// application.APIKeyRepository.
//
// They share a type because they share a transaction: authenticating a request and
// metering it are two halves of the same story about one consumer, and splitting them
// across two adapters only creates an opportunity for them to disagree about which
// consumer a key belongs to.
type UsageRepo struct{ db *DB }

var (
	_ application.UsageRecorder    = (*UsageRepo)(nil)
	_ application.APIKeyRepository = (*UsageRepo)(nil)
)

// NewUsageRepo returns a usage and API key repository bound to db.
func NewUsageRepo(db *DB) *UsageRepo { return &UsageRepo{db: db} }

// ---------------------------------------------------------------------------
// Metering
// ---------------------------------------------------------------------------

// Record persists one metered request and folds it into the daily and monthly
// aggregates, in a single transaction.
//
// The idempotency key is what makes this safe to retry. Recording is asynchronous
// relative to the response -- the customer's latency does not pay for the meter -- so
// a retry after a partial failure is normal, and double-counting a request against a
// quota is a billing error rather than a cosmetic one. A repeated key inserts nothing
// and, crucially, increments nothing: the aggregate update runs only when the INSERT
// actually created a row.
//
// Anonymous traffic (no consumer) is recorded but not aggregated, because
// usage_aggregates is keyed by consumer and there is no quota to enforce against
// nobody.
func (r *UsageRepo) Record(ctx context.Context, rec application.UsageRecord) error {
	id := rec.ID
	if id == "" {
		id = r.db.newID("use")
	}
	idempotencyKey := rec.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = id
	}
	quotaWeight := rec.QuotaWeight
	if quotaWeight <= 0 {
		quotaWeight = 1
	}
	occurredAt := rec.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	occurredAt = occurredAt.UTC()

	return r.db.Within(ctx, func(ctx context.Context) error {
		q := r.db.q(ctx)

		var insertedID string
		err := q.QueryRow(ctx,
			`INSERT INTO usage_records (
                id, idempotency_key, consumer_id, api_key_id, endpoint, method,
                status_code, quota_weight, duration_ms, bytes_out, vendor_slug,
                product_slug, rate_limited, occurred_at
             ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
             ON CONFLICT (idempotency_key) DO NOTHING
             RETURNING id`,
			id, idempotencyKey, nullString(rec.ConsumerID), nullString(rec.APIKeyID),
			rec.Endpoint, rec.Method, rec.StatusCode, quotaWeight,
			nullInt(rec.DurationMS), rec.BytesOut, nullString(rec.VendorSlug),
			nullString(rec.ProductSlug), rec.RateLimited, occurredAt).Scan(&insertedID)
		if err != nil {
			if errors.Is(translate(err), domain.ErrNotFound) {
				// DO NOTHING returned no row: this request was already metered.
				return nil
			}
			return wrap("usage.Record", err)
		}

		if rec.ConsumerID == "" {
			return nil
		}

		var errorCount, rateLimitedCount int64
		if rec.StatusCode >= 400 {
			errorCount = 1
		}
		if rec.RateLimited {
			rateLimitedCount = 1
		}

		day := time.Date(occurredAt.Year(), occurredAt.Month(), occurredAt.Day(), 0, 0, 0, 0, time.UTC)
		month := time.Date(occurredAt.Year(), occurredAt.Month(), 1, 0, 0, 0, 0, time.UTC)

		for _, period := range []struct {
			kind  string
			start time.Time
		}{{"day", day}, {"month", month}} {
			if _, err := q.Exec(ctx,
				`INSERT INTO usage_aggregates (
                    consumer_id, period_start, period_kind, request_count,
                    quota_consumed, error_count, rate_limited_count, bytes_out, updated_at
                 ) VALUES ($1, $2::date, $3, 1, $4, $5, $6, $7, now())
                 ON CONFLICT (consumer_id, period_kind, period_start) DO UPDATE SET
                    request_count      = usage_aggregates.request_count + 1,
                    quota_consumed     = usage_aggregates.quota_consumed + EXCLUDED.quota_consumed,
                    error_count        = usage_aggregates.error_count + EXCLUDED.error_count,
                    rate_limited_count = usage_aggregates.rate_limited_count + EXCLUDED.rate_limited_count,
                    bytes_out          = usage_aggregates.bytes_out + EXCLUDED.bytes_out,
                    updated_at         = now()`,
				rec.ConsumerID, period.start, period.kind, int64(quotaWeight),
				errorCount, rateLimitedCount, rec.BytesOut); err != nil {
				return wrap("usage.Record.aggregate", err)
			}
		}
		return nil
	})
}

// QuotaConsumed returns how much of a consumer's monthly quota has been used in the
// period beginning at periodStart.
//
// It reads the aggregate rather than summing usage_records, because quota enforcement
// runs on the request path and a sum over a month of raw records is not a query you
// want between a customer and their response. A consumer with no rows has consumed
// nothing, which is zero rather than a not-found error.
func (r *UsageRepo) QuotaConsumed(ctx context.Context, consumerID string, periodStart time.Time) (int64, error) {
	start := periodStart.UTC()
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)

	var consumed int64
	err := r.db.q(ctx).QueryRow(ctx,
		`SELECT quota_consumed
           FROM usage_aggregates
          WHERE consumer_id = $1 AND period_kind = 'month' AND period_start = $2::date`,
		consumerID, start).Scan(&consumed)
	if err != nil {
		if errors.Is(translate(err), domain.ErrNotFound) {
			return 0, nil
		}
		return 0, wrap("usage.QuotaConsumed", err)
	}
	return consumed, nil
}

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

// ResolveByHash returns the consumer a key belongs to and the key's id, or
// domain.ErrNotFound.
//
// The lookup is by SHA-256 hash because the plaintext key is never stored: it is shown
// once at creation and cannot be recovered, so a database disclosure does not hand an
// attacker working credentials. Revoked and expired keys do not resolve, which is what
// makes revocation take effect on the next request rather than at the next cache
// expiry.
func (r *UsageRepo) ResolveByHash(ctx context.Context, keyHash string) (application.APIConsumer, string, error) {
	var (
		c     application.APIConsumer
		keyID string
	)
	err := r.db.q(ctx).QueryRow(ctx,
		`SELECT c.id, c.name, c.plan, c.status, c.monthly_quota, c.rate_limit_per_min, k.id
           FROM api_keys k
           JOIN api_consumers c ON c.id = k.consumer_id
          WHERE k.key_hash = $1
            AND k.status = 'active'
            AND (k.expires_at IS NULL OR k.expires_at > now())`, keyHash).
		Scan(&c.ID, &c.Name, &c.Plan, &c.Status, &c.MonthlyQuota, &c.RateLimitPerMin, &keyID)
	if err != nil {
		return application.APIConsumer{}, "", wrap("apikey.ResolveByHash", err)
	}
	return c, keyID, nil
}

// Create stores a new API key for a consumer and returns the key's id.
//
// It stores only the hash and a short prefix. The prefix exists so a customer can
// identify which of their keys a log line refers to without the key itself being
// recoverable; the schema constrains it to between 4 and 16 characters so it cannot
// quietly become the whole secret.
func (r *UsageRepo) Create(ctx context.Context, consumerID, keyHash, keyPrefix, label string) (string, error) {
	id := r.db.newID("key")
	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO api_keys (id, consumer_id, key_hash, key_prefix, label)
         VALUES ($1, $2, $3, $4, $5)`,
		id, consumerID, keyHash, keyPrefix, nullString(label))
	if err != nil {
		return "", wrap("apikey.Create", err)
	}
	return id, nil
}

// Revoke marks a key revoked with a reason and a timestamp.
//
// Revoking an already-revoked key is reported as domain.ErrNotFound rather than
// silently succeeding, so a support tool cannot report "revoked" for a key it did not
// actually change -- for instance one that was already rotated out by someone else.
func (r *UsageRepo) Revoke(ctx context.Context, keyID, reason string, at time.Time) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE api_keys
            SET status         = 'revoked',
                revoked_reason = $2,
                revoked_at     = COALESCE($3::timestamptz, now())
          WHERE id = $1 AND status <> 'revoked'`,
		keyID, nullString(reason), nullTime(at))
	if err != nil {
		return wrap("apikey.Revoke", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("apikey.Revoke")
	}
	return nil
}

// TouchLastUsed records when a key was last seen.
//
// The write only moves the timestamp forward. Requests are metered asynchronously and
// can therefore be recorded out of order, and a last-used time that goes backwards
// would make "this key has been idle for 90 days, revoke it" unsafe to act on.
func (r *UsageRepo) TouchLastUsed(ctx context.Context, keyID string, at time.Time) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE api_keys
            SET last_used_at = GREATEST(COALESCE(last_used_at, 'epoch'::timestamptz),
                                        COALESCE($2::timestamptz, now()))
          WHERE id = $1`, keyID, nullTime(at))
	if err != nil {
		return wrap("apikey.TouchLastUsed", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("apikey.TouchLastUsed")
	}
	return nil
}
