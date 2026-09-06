package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// CandidateRepo is the PostgreSQL implementation of application.CandidateRepository.
//
// Candidates are observations that may be wrong. They live in their own table with
// their own state machine precisely so that "we saw this" and "we assert this" are
// different rows in different tables, and no amount of collector enthusiasm can turn
// the first into the second.
type CandidateRepo struct{ db *DB }

var _ application.CandidateRepository = (*CandidateRepo)(nil)

// NewCandidateRepo returns a candidate repository bound to db.
func NewCandidateRepo(db *DB) *CandidateRepo { return &CandidateRepo{db: db} }

const candidateColumns = `
    id, source_id, collector_run_id, evidence_id, product_id, product_family_id,
    product_match_hint, product_match_status, raw_version, normalized_version,
    release_type, proposed_release_type, channel, region, hardware_revision,
    deployment_mode, release_date, release_date_precision, publication_date,
    release_notes_url, confidence_score, dedupe_key, state, rejection_reason,
    published_release_id, discovered_at, updated_at`

// scanCandidate reads a candidate row. It reconstructs the version and both dates
// through their domain constructors, so a row that violated the date-precision rule is
// rejected on read rather than becoming a date with invented precision.
func scanCandidate(row interface{ Scan(...any) error }) (domain.CandidateRelease, error) {
	var (
		c            domain.CandidateRelease
		runID        *string
		evidenceID   *string
		productID    *string
		familyID     *string
		matchHint    *string
		matchStatus  string
		rawVersion   string
		normVersion  string
		releaseType  *string
		proposedType *string
		channel      *string
		region       *string
		hardwareRev  *string
		deployMode   *string
		releaseDate  *time.Time
		precision    string
		pubDate      *time.Time
		notesURL     *string
		state        string
		rejection    *string
		publishedID  *string
	)
	if err := row.Scan(
		&c.ID, &c.SourceID, &runID, &evidenceID, &productID, &familyID,
		&matchHint, &matchStatus, &rawVersion, &normVersion,
		&releaseType, &proposedType, &channel, &region, &hardwareRev,
		&deployMode, &releaseDate, &precision, &pubDate,
		&notesURL, &c.Confidence, &c.DedupeKey, &state, &rejection,
		&publishedID, &c.DiscoveredAt, &c.UpdatedAt,
	); err != nil {
		return domain.CandidateRelease{}, err
	}

	v, err := version(rawVersion, normVersion)
	if err != nil {
		return domain.CandidateRelease{}, err
	}
	rd, err := partialDate(releaseDate, precision)
	if err != nil {
		return domain.CandidateRelease{}, err
	}
	pd, err := publicationDate(pubDate)
	if err != nil {
		return domain.CandidateRelease{}, err
	}

	c.CollectorRunID = str(runID)
	c.EvidenceID = str(evidenceID)
	c.ProductID = str(productID)
	c.ProductFamilyID = str(familyID)
	c.ProductMatchHint = str(matchHint)
	c.ProductMatchStatus = domain.ProductMatchStatus(matchStatus)
	c.Version = v
	c.ReleaseType = domain.ReleaseType(str(releaseType))
	c.ProposedReleaseType = domain.ReleaseType(str(proposedType))
	c.Applicability = domain.Applicability{
		HardwareRevision: str(hardwareRev),
		Region:           str(region),
		Channel:          str(channel),
		DeploymentMode:   str(deployMode),
	}
	c.ReleaseDate = rd
	c.PublicationDate = pd
	c.ReleaseNotesURL = str(notesURL)
	c.State = domain.CandidateState(state)
	c.RejectionReason = str(rejection)
	c.PublishedReleaseID = str(publishedID)
	return c, nil
}

// GetByID returns the candidate with the given id, or domain.ErrNotFound.
func (r *CandidateRepo) GetByID(ctx context.Context, id string) (domain.CandidateRelease, error) {
	row := r.db.q(ctx).QueryRow(ctx, `SELECT`+candidateColumns+` FROM candidate_releases WHERE id = $1`, id)
	c, err := scanCandidate(row)
	if err != nil {
		return domain.CandidateRelease{}, wrap("candidate.GetByID", err)
	}
	return c, nil
}

// UpsertByDedupeKey inserts a candidate, or returns the existing row when the
// (source, dedupe key) pair is already present. The bool reports whether a new row was
// created.
//
// This is what makes re-running a check idempotent: a watcher that fetches the same
// changelog twice produces the same dedupe keys and therefore the same candidates, so
// the second run creates nothing.
//
// The conflict branch updates only updated_at. Overwriting the stored fields would let
// a later, worse extraction quietly replace an earlier one that a human may already be
// reviewing; a genuinely different observation has a different dedupe key by
// construction. The (xmax = 0) test in RETURNING is how PostgreSQL distinguishes a row
// this statement inserted from one it merely updated -- an inserted row has no
// previous version, so its xmax is zero.
//
// Note that domain.Applicability.Note has no column in candidate_releases and is not
// persisted here; only the four constraint fields that participate in the dedupe key
// are stored.
func (r *CandidateRepo) UpsertByDedupeKey(ctx context.Context, c domain.CandidateRelease) (domain.CandidateRelease, bool, error) {
	if err := c.Validate(); err != nil {
		return domain.CandidateRelease{}, false, err
	}
	id := c.ID
	if id == "" {
		id = r.db.newID("cand")
	}
	releaseDate, precision := dateParams(c.ReleaseDate)

	row := r.db.q(ctx).QueryRow(ctx,
		`INSERT INTO candidate_releases (
            id, source_id, collector_run_id, evidence_id, product_id, product_family_id,
            product_match_hint, product_match_status, raw_version, normalized_version,
            release_type, proposed_release_type, channel, region, hardware_revision,
            deployment_mode, release_date, release_date_precision, publication_date,
            release_notes_url, confidence_score, dedupe_key, state, rejection_reason,
            published_release_id, discovered_at, updated_at
         ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
            $17, $18, $19, $20, $21, $22, $23, $24, $25,
            COALESCE($26::timestamptz, now()), COALESCE($27::timestamptz, now())
         )
         ON CONFLICT (source_id, dedupe_key) DO UPDATE SET updated_at = EXCLUDED.updated_at
         RETURNING`+candidateColumns+`, (xmax = 0) AS inserted`,
		id, c.SourceID, nullString(c.CollectorRunID), nullString(c.EvidenceID),
		nullString(c.ProductID), nullString(c.ProductFamilyID),
		nullString(c.ProductMatchHint), string(c.ProductMatchStatus),
		c.Version.Raw(), c.Version.Normalized(),
		nullString(string(c.ReleaseType)), nullString(string(c.ProposedReleaseType)),
		nullString(c.Applicability.Channel), nullString(c.Applicability.Region),
		nullString(c.Applicability.HardwareRevision), nullString(c.Applicability.DeploymentMode),
		releaseDate, precision, publicationDateParam(c.PublicationDate),
		nullString(c.ReleaseNotesURL), c.Confidence, c.DedupeKey, string(c.State),
		nullString(c.RejectionReason), nullString(c.PublishedReleaseID),
		nullTime(c.DiscoveredAt), nullTime(c.UpdatedAt),
	)

	var inserted bool
	out, err := scanCandidate(candidateRowWithFlag{row: row, inserted: &inserted})
	if err != nil {
		return domain.CandidateRelease{}, false, wrap("candidate.UpsertByDedupeKey", err)
	}
	return out, inserted, nil
}

// candidateRowWithFlag adapts a row that carries one trailing boolean column to the
// Scan signature scanCandidate expects, so the insert-or-update flag travels back in
// the same round trip as the row.
type candidateRowWithFlag struct {
	row      interface{ Scan(...any) error }
	inserted *bool
}

func (c candidateRowWithFlag) Scan(dest ...any) error {
	return c.row.Scan(append(dest, c.inserted)...)
}

// UpdateState moves a candidate to a new state and records the rejection reason.
//
// The state machine itself is enforced in the domain, not here: the repository writes
// the state the use case decided on. Enforcing transitions in SQL as well would
// duplicate the rule in a place where it cannot be unit tested.
func (r *CandidateRepo) UpdateState(ctx context.Context, id string, state domain.CandidateState, rejectionReason string) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE candidate_releases
            SET state = $2, rejection_reason = $3, updated_at = now()
          WHERE id = $1`, id, string(state), nullString(rejectionReason))
	if err != nil {
		return wrap("candidate.UpdateState", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("candidate.UpdateState")
	}
	return nil
}

// SetPublishedRelease records which release a candidate became. It is the link that
// lets a published fact be traced back to the observation and the check that produced
// it.
func (r *CandidateRepo) SetPublishedRelease(ctx context.Context, candidateID, releaseID string) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE candidate_releases
            SET published_release_id = $2, updated_at = now()
          WHERE id = $1`, candidateID, releaseID)
	if err != nil {
		return wrap("candidate.SetPublishedRelease", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("candidate.SetPublishedRelease")
	}
	return nil
}

// ListByState returns candidates in a given state, oldest first, capped at limit.
// Oldest first is what stops a backlog from starving: a candidate discovered an hour
// ago is processed before one discovered a second ago.
func (r *CandidateRepo) ListByState(ctx context.Context, state domain.CandidateState, limit int) ([]domain.CandidateRelease, error) {
	limit = clampLimit(limit, 100, 1000)
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT`+candidateColumns+`
           FROM candidate_releases
          WHERE state = $1
          ORDER BY discovered_at
          LIMIT $2`, string(state), limit)
	if err != nil {
		return nil, wrap("candidate.ListByState", err)
	}
	defer rows.Close()

	out := make([]domain.CandidateRelease, 0, limit)
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, wrap("candidate.ListByState.scan", err)
		}
		out = append(out, c)
	}
	return out, wrap("candidate.ListByState.rows", rows.Err())
}

// RecordValidation stores the verdict of each validation gate that ran.
//
// Re-running validation replaces the previous verdicts for the gates that ran rather
// than appending to them, so the table answers "why is this candidate in the state it
// is in" with one row per gate instead of an archaeology exercise. Verdicts for gates
// that did not run this time are left in place. The whole set is written in one
// transaction so a reviewer never sees a half-recorded verdict.
func (r *CandidateRepo) RecordValidation(ctx context.Context, candidateID string, results []domain.GateResult) error {
	if len(results) == 0 {
		return nil
	}
	gates := make([]string, 0, len(results))
	for _, res := range results {
		gates = append(gates, string(res.Gate))
	}

	return r.db.Within(ctx, func(ctx context.Context) error {
		q := r.db.q(ctx)
		if _, err := q.Exec(ctx,
			`DELETE FROM validation_results WHERE candidate_id = $1 AND gate = ANY($2::text[])`,
			candidateID, gates); err != nil {
			return wrap("candidate.RecordValidation.delete", err)
		}
		for _, res := range results {
			order := res.Order
			if order == 0 {
				order = domain.GateIndex(res.Gate)
			}
			evaluatedBy := res.EvaluatedBy
			if evaluatedBy == "" {
				evaluatedBy = "deterministic"
			}
			if _, err := q.Exec(ctx,
				`INSERT INTO validation_results (
                    id, candidate_id, gate, gate_order, outcome, detail, evaluated_by, evaluated_at
                 ) VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8::timestamptz, now()))`,
				r.db.newID("val"), candidateID, string(res.Gate), order,
				string(res.Outcome), nullString(res.Detail), evaluatedBy,
				nullTime(res.EvaluatedAt)); err != nil {
				return wrap("candidate.RecordValidation.insert", err)
			}
		}
		return nil
	})
}

// ListValidationResults returns the recorded verdict of every gate that ran for a
// candidate, in gate order.
//
// RecordValidation has always written these rows and nothing had ever read them back,
// which meant a reviewer could see that a candidate needed review but not which gate
// said so. A review queue without the failing gate is a queue nobody can act on.
func (r *CandidateRepo) ListValidationResults(ctx context.Context, candidateID string) ([]domain.GateResult, error) {
	rows, err := r.db.q(ctx).Query(ctx,
		`SELECT gate, gate_order, outcome, detail, evaluated_by, evaluated_at
           FROM validation_results
          WHERE candidate_id = $1
          ORDER BY gate_order`, candidateID)
	if err != nil {
		return nil, wrap("candidate.ListValidationResults", err)
	}
	defer rows.Close()

	var out []domain.GateResult
	for rows.Next() {
		var (
			res     domain.GateResult
			gate    string
			outcome string
			detail  *string
		)
		if err := rows.Scan(&gate, &res.Order, &outcome, &detail, &res.EvaluatedBy, &res.EvaluatedAt); err != nil {
			return nil, wrap("candidate.ListValidationResults.scan", err)
		}
		res.Gate = domain.ValidationGate(gate)
		res.Outcome = domain.GateOutcome(outcome)
		res.Detail = str(detail)
		out = append(out, res)
	}
	return out, wrap("candidate.ListValidationResults.rows", rows.Err())
}

// SetResolvedProduct records the product a candidate was matched to.
//
// The match is written back because publication must act on a settled fact: resolving
// the hint again at publication time could produce a different answer if an alias was
// added in between, and a release attached to the wrong product is worse than one that
// is late.
func (r *CandidateRepo) SetResolvedProduct(ctx context.Context, candidateID, productID string) error {
	tag, err := r.db.q(ctx).Exec(ctx,
		`UPDATE candidate_releases
            SET product_id = $2, product_match_status = 'unique', updated_at = now()
          WHERE id = $1`, candidateID, productID)
	if err != nil {
		return wrap("candidate.SetResolvedProduct", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("candidate %s: %w", candidateID, domain.ErrNotFound)
	}
	return nil
}
