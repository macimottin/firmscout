package postgres

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// ArtifactStore records artifact metadata in PostgreSQL and delegates the bytes to a
// blob store.
//
// The split exists because the two halves have genuinely different lifetimes and
// costs. The metadata row is small, permanent enough to satisfy a foreign key from
// source_checks and evidence, and is what retention policy and deduplication reason
// about. The bytes are large, disposable, and belong wherever storage is cheapest --
// a local directory in development, S3 in AWS. Recording only the bytes, as an earlier
// arrangement did, leaves source_checks.artifact_id pointing at nothing.
//
// Deduplication happens on the content hash, which is unique in the table. An
// unchanged source therefore stores nothing new: the existing row's last_seen_at is
// refreshed and its reference count incremented. Since the overwhelming majority of
// checks find no change, this is the largest single storage saving in the system, and
// it falls out of the schema rather than needing a cleanup job.
type ArtifactStore struct {
	db   *DB
	blob application.BlobStore
	// retention classifies newly stored artifacts. Recent artifacts are needed for
	// a bounded operational window and then become disposable; nothing here is
	// permanent, because an artifact can always be re-fetched while its evidence
	// excerpt and hash cannot.
	retention string
	ttl       time.Duration
}

var _ application.ArtifactStore = (*ArtifactStore)(nil)

// ArtifactOption configures the store.
type ArtifactOption func(*ArtifactStore)

// WithArtifactRetention sets the retention class and lifetime of newly stored
// artifacts.
func WithArtifactRetention(class string, ttl time.Duration) ArtifactOption {
	return func(s *ArtifactStore) {
		s.retention = class
		s.ttl = ttl
	}
}

// NewArtifactStore builds the store over a blob backend.
func NewArtifactStore(db *DB, blob application.BlobStore, opts ...ArtifactOption) *ArtifactStore {
	s := &ArtifactStore{
		db:        db,
		blob:      blob,
		retention: "temporarily_required",
		ttl:       90 * 24 * time.Hour,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Put stores content and returns its artifact id, reporting created=false when the
// same content was already held.
func (s *ArtifactStore) Put(ctx context.Context, hash, contentType string, body []byte) (string, bool, error) {
	if hash == "" {
		return "", false, fmt.Errorf("artifact: content hash is required: %w", domain.ErrValidation)
	}

	// Look first: an artifact already recorded needs no second copy of its bytes.
	var existingID string
	err := s.db.q(ctx).QueryRow(ctx,
		`UPDATE source_artifacts
            SET last_seen_at = now(), reference_count = reference_count + 1
          WHERE content_hash = $1
      RETURNING id`, hash).Scan(&existingID)
	if err == nil {
		return existingID, false, nil
	}
	if !isNoRows(err) {
		return "", false, wrap("touch existing artifact", err)
	}

	key, _, err := s.blob.PutBlob(ctx, hash, body)
	if err != nil {
		return "", false, fmt.Errorf("artifact: store bytes: %w", err)
	}

	id := s.db.newID("art")
	var expires *time.Time
	if s.ttl > 0 {
		t := time.Now().UTC().Add(s.ttl)
		expires = &t
	}

	// Two concurrent checks of the same source can race here; the unique index on
	// content_hash decides, and the loser reads back the winner's row rather than
	// failing the check.
	err = s.db.q(ctx).QueryRow(ctx,
		`INSERT INTO source_artifacts
            (id, content_hash, content_type, byte_size, storage_backend, storage_key,
             compression, retention_class, expires_at, first_seen_at, last_seen_at, reference_count)
         VALUES ($1, $2, $3, $4, $5, $6, 'gzip', $7, $8, now(), now(), 1)
         ON CONFLICT (content_hash) DO UPDATE
            SET last_seen_at = now(), reference_count = source_artifacts.reference_count + 1
      RETURNING id, (xmax = 0) AS inserted`,
		id, hash, nullString(contentType), int64(len(body)),
		s.blob.Backend(), nullString(key), s.retention, expires).Scan(&id, new(bool))
	if err != nil {
		return "", false, wrap("record artifact", err)
	}
	return id, true, nil
}

// Get returns the stored bytes for an artifact id.
func (s *ArtifactStore) Get(ctx context.Context, id string) (io.ReadCloser, error) {
	var key, backend string
	err := s.db.q(ctx).QueryRow(ctx,
		`SELECT COALESCE(storage_key, content_hash), storage_backend
           FROM source_artifacts WHERE id = $1`, id).Scan(&key, &backend)
	if err != nil {
		return nil, wrap("artifact "+id, err)
	}
	rc, err := s.blob.GetBlob(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("artifact %s: read bytes: %w", id, err)
	}
	return rc, nil
}

// Touch refreshes an artifact's last-seen marker, which is what keeps a still-relevant
// artifact out of the retention sweep.
func (s *ArtifactStore) Touch(ctx context.Context, id string, at time.Time) error {
	tag, err := s.db.q(ctx).Exec(ctx,
		`UPDATE source_artifacts SET last_seen_at = $2 WHERE id = $1`, id, at.UTC())
	if err != nil {
		return wrap("touch artifact", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("artifact %s: %w", id, domain.ErrNotFound)
	}
	return nil
}
