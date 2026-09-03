package postgres

import (
	"context"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// EvidenceRepo is the PostgreSQL implementation of application.EvidenceRepository.
//
// Evidence is the difference between a catalogue and a rumour, which is why the port
// has an Insert and a Get and nothing else: a provenance record that could be edited
// after the fact would not be provenance.
type EvidenceRepo struct{ db *DB }

var _ application.EvidenceRepository = (*EvidenceRepo)(nil)

// NewEvidenceRepo returns an evidence repository bound to db.
func NewEvidenceRepo(db *DB) *EvidenceRepo { return &EvidenceRepo{db: db} }

const evidenceColumns = `
    id, source_id, source_url, source_type, official, retrieved_at, artifact_id,
    content_hash, excerpt, raw_value, normalized_value, collector_id, collector_version,
    discovery_method, ai_model_id, ai_prompt_version, confidence_score, created_at`

// Insert stores a provenance record.
//
// The excerpt is truncated to domain.MaxExcerptLength rather than rejected, because
// losing a whole fact over an over-long quotation would be the wrong trade -- but the
// limit itself is enforced, since a short quotation used to verify a fact is a
// different thing legally from a reproduction of a vendor's release notes.
//
// The schema separately refuses an ai_assisted record that names no model and prompt,
// so an AI-influenced fact can always be traced back to what produced it.
func (r *EvidenceRepo) Insert(ctx context.Context, e domain.Evidence) error {
	e.Excerpt = domain.TruncateExcerpt(e.Excerpt)
	if err := e.Validate(); err != nil {
		return err
	}
	id := e.ID
	if id == "" {
		id = r.db.newID("ev")
	}
	var confidence *float64
	if e.Confidence != 0 {
		confidence = &e.Confidence
	}

	_, err := r.db.q(ctx).Exec(ctx,
		`INSERT INTO evidence (
            id, source_id, source_url, source_type, official, retrieved_at, artifact_id,
            content_hash, excerpt, raw_value, normalized_value, collector_id,
            collector_version, discovery_method, ai_model_id, ai_prompt_version,
            confidence_score, created_at
         ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
            COALESCE($18::timestamptz, now())
         )`,
		id, nullString(e.SourceID), e.SourceURL, nullString(string(e.SourceType)),
		e.Official, e.RetrievedAt.UTC(), nullString(e.ArtifactID),
		nullString(e.ContentHash), e.Excerpt, nullString(e.RawValue),
		nullString(e.NormalizedValue), nullString(e.CollectorID),
		nullString(e.CollectorVersion), string(e.DiscoveryMethod),
		nullString(e.AIModelID), nullString(e.AIPromptVersion),
		confidence, nullTime(e.CreatedAt))
	return wrap("evidence.Insert", err)
}

// GetByID returns the evidence record with the given id, or domain.ErrNotFound.
func (r *EvidenceRepo) GetByID(ctx context.Context, id string) (domain.Evidence, error) {
	var (
		e           domain.Evidence
		sourceID    *string
		sourceType  *string
		artifactID  *string
		contentHash *string
		rawValue    *string
		normValue   *string
		collectorID *string
		collectorV  *string
		method      string
		aiModel     *string
		aiPrompt    *string
		confidence  *float64
	)
	err := r.db.q(ctx).QueryRow(ctx,
		`SELECT`+evidenceColumns+` FROM evidence WHERE id = $1`, id).Scan(
		&e.ID, &sourceID, &e.SourceURL, &sourceType, &e.Official, &e.RetrievedAt,
		&artifactID, &contentHash, &e.Excerpt, &rawValue, &normValue, &collectorID,
		&collectorV, &method, &aiModel, &aiPrompt, &confidence, &e.CreatedAt)
	if err != nil {
		return domain.Evidence{}, wrap("evidence.GetByID", err)
	}
	e.SourceID = str(sourceID)
	e.SourceType = domain.SourceType(str(sourceType))
	e.ArtifactID = str(artifactID)
	e.ContentHash = str(contentHash)
	e.RawValue = str(rawValue)
	e.NormalizedValue = str(normValue)
	e.CollectorID = str(collectorID)
	e.CollectorVersion = str(collectorV)
	e.DiscoveryMethod = domain.DiscoveryMethod(method)
	e.AIModelID = str(aiModel)
	e.AIPromptVersion = str(aiPrompt)
	e.Confidence = float(confidence)
	return e, nil
}
