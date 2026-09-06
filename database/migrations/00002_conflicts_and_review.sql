-- +goose Up
-- +goose StatementBegin

-- Phase 2: multi-source conflict detection, the review queue's read and decision
-- surfaces, and the advisory release type.
--
-- Design rules this file continues from 00001:
--
--  1. Dates carry explicit precision, and a CHECK forces the stored DATE to be the
--     canonical anchor so a month-precision date can never be read as a real day.
--  2. Nothing here orders a version string.
--  3. A derived flag is derived from a table, never from a rule duplicated in SQL.
--  4. Every state column carries a CHECK, so an unknown state is a failed write rather
--     than a row nothing can interpret.

-- The advisory release type. It is deliberately distinct from security_update: a
-- security update is a release that fixes a vulnerability, an advisory is the vendor's
-- document saying one exists. A catalogue that cannot tell them apart answers "what
-- should I install" with a document identifier.
INSERT INTO release_types (id, name, description, sort_order) VALUES
    ('advisory', 'Security advisory',
     'Vendor security advisory document; not itself an installable artifact', 105)
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- What each source currently claims.
-- ---------------------------------------------------------------------------

-- One row per (source, product, channel): the newest release that source has been
-- observed to report.
--
-- This is a projection, not a log. candidate_releases already holds every observation
-- ever made, but answering "what does this source say is newest" from it would require
-- ordering candidates, and the only orderings available are the version string (which
-- has no order -- ADR-0017) and discovery time (which says when FirmScout looked, not
-- which release is newer: a 46-entry changelog is discovered in one second). The
-- ordering rule lives in domain.LaterObservation and this table stores its result.
CREATE TABLE source_observations (
    id                      TEXT PRIMARY KEY,
    source_id               TEXT NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    product_id              TEXT NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    -- The empty string, never NULL. This column is part of a unique key, and two NULLs
    -- do not conflict in PostgreSQL, so a NULL channel would allow two "current" rows
    -- for one source. The empty string is the same canonical no-channel value every
    -- COALESCE(channel, '') expression in 00001 already uses.
    channel                 TEXT NOT NULL DEFAULT '',

    raw_version             TEXT NOT NULL,
    normalized_version      TEXT NOT NULL,
    release_date            DATE,
    release_date_precision  TEXT NOT NULL DEFAULT 'unknown'
                            CHECK (release_date_precision IN ('exact_day', 'month_only', 'year_only', 'unknown')),

    candidate_id            TEXT REFERENCES candidate_releases (id) ON DELETE SET NULL,
    evidence_id             TEXT REFERENCES evidence (id) ON DELETE SET NULL,

    observed_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    first_observed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT source_observations_version_not_blank CHECK (length(btrim(normalized_version)) > 0),
    CONSTRAINT source_observations_date_precision CHECK (
        (release_date_precision = 'unknown'    AND release_date IS NULL)
     OR (release_date_precision = 'exact_day'  AND release_date IS NOT NULL)
     OR (release_date_precision = 'month_only' AND release_date IS NOT NULL
             AND EXTRACT(DAY FROM release_date) = 1)
     OR (release_date_precision = 'year_only'  AND release_date IS NOT NULL
             AND EXTRACT(DAY FROM release_date) = 1 AND EXTRACT(MONTH FROM release_date) = 1)
    )
);

-- One current claim per source, product and channel. This is the constraint that makes
-- the table a projection rather than a second log.
CREATE UNIQUE INDEX source_observations_current_idx
    ON source_observations (source_id, product_id, channel);

-- The conflict query reads every source's claim for one product and channel.
CREATE INDEX source_observations_product_idx
    ON source_observations (product_id, channel);

-- ---------------------------------------------------------------------------
-- Disagreements the authority ladder could not settle.
-- ---------------------------------------------------------------------------

-- A conflict is a row, not a boolean derived from the observations above, because
-- whether a disagreement counts as a conflict depends on the source authority ladder
-- (domain.QualityClass.Authority). Re-deriving that ladder in SQL would put a second
-- copy of it where no test compares the two. See ADR-0020.
CREATE TABLE source_conflicts (
    id                  TEXT PRIMARY KEY,
    product_id          TEXT NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    channel             TEXT NOT NULL DEFAULT '',
    state               TEXT NOT NULL DEFAULT 'open'
                        CHECK (state IN ('open', 'resolved')),
    -- The authority rank at which the disagreement sits, so a queue can distinguish two
    -- official sources disagreeing from two community sources disagreeing.
    authority_rank      INTEGER NOT NULL DEFAULT 0,
    -- The participants at detection time. Arrays rather than a child table: the
    -- per-source detail a reviewer needs is the *current* claim, which is read live from
    -- source_observations, and what this row must preserve is which versions were in
    -- dispute when it opened.
    versions            TEXT[] NOT NULL DEFAULT '{}',
    source_ids          TEXT[] NOT NULL DEFAULT '{}',

    review_item_id      TEXT REFERENCES review_items (id) ON DELETE SET NULL,

    detected_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at         TIMESTAMPTZ,
    resolved_by         TEXT,
    resolution          TEXT,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A disagreement needs at least two participants. A one-sided "conflict" is a bug
    -- in the detector, and it should fail the write rather than reach a product page.
    CONSTRAINT source_conflicts_participants CHECK (
        state <> 'open'
     OR (COALESCE(array_length(versions, 1), 0) >= 2
         AND COALESCE(array_length(source_ids, 1), 0) >= 2)
    ),
    CONSTRAINT source_conflicts_resolution_consistency CHECK (
        (state = 'open'     AND resolved_at IS NULL)
     OR (state = 'resolved' AND resolved_at IS NOT NULL)
    )
);

-- At most one open conflict per product and channel. The upsert in the conflict
-- repository relies on this: without it, every re-check of both sources would open
-- another conflict and the review queue would fill with copies of one disagreement.
CREATE UNIQUE INDEX source_conflicts_open_idx
    ON source_conflicts (product_id, channel)
    WHERE state = 'open';

CREATE INDEX source_conflicts_queue_idx
    ON source_conflicts (detected_at DESC)
    WHERE state = 'open';

-- ---------------------------------------------------------------------------
-- Review queue and audit trail.
-- ---------------------------------------------------------------------------

-- "Is there already an item for this candidate" is the question the review queue asks
-- most often and the one 00001 left unindexed.
CREATE INDEX review_items_subject_idx ON review_items (subject_type, subject_id);

-- Whether the platform verified the actor's identity.
--
-- FirmScout has no login. A reviewer's name arrives as an asserted string on the
-- request and is recorded as one; this column is what stops a later reader -- or a
-- later version of this system, after authentication exists -- from mistaking an
-- assertion for a verification. It defaults to false because false is the truth for
-- every row that exists today. See ADR-0021.
ALTER TABLE audit_events
    ADD COLUMN actor_authenticated BOOLEAN NOT NULL DEFAULT false;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE audit_events DROP COLUMN IF EXISTS actor_authenticated;
DROP INDEX IF EXISTS review_items_subject_idx;
DROP TABLE IF EXISTS source_conflicts;
DROP TABLE IF EXISTS source_observations;
-- Only removable while nothing references it; releases.release_type is a RESTRICT
-- foreign key, so this fails loudly if an advisory was published, which is the correct
-- outcome for a down migration that would otherwise orphan a row.
DELETE FROM release_types WHERE id = 'advisory';
-- +goose StatementEnd
