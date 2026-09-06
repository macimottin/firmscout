-- +goose Up
-- +goose StatementBegin

-- Phase 2 follow-up: the product page has a real "verify against the manufacturer"
-- link and a real "here is what is disputed" banner waiting on data that
-- product_summaries has never carried.
--
-- official_sources is a JSONB array rather than a join to sources at read time,
-- because product_summaries exists precisely so a product page is one indexed read
-- (see its own comment in 00001). It holds {slug, url, kind, official} for every
-- source that has actually contributed a currently-mapped, non-withdrawn release to
-- the product -- kind is stored as the raw sources.source_type value, not the public
-- vocabulary, so the one Go function that maps a source_type to a public kind
-- (domain.PublicSourceKind) is the only place that mapping happens, for a release's
-- source and a product's officialSources alike; storing an already-mapped string here
-- would be a second copy of that decision, sitting in a column nothing could migrate
-- if the vocabulary ever changed. DEFAULT '[]'::jsonb rather than NULL: a product with
-- no contributing source yet has an empty list, not an absent one, and every reader of
-- this column can jsonb_array_elements it without a NULL check.
--
-- conflict_channel/conflict_versions/conflict_source_count/conflict_detected_at are
-- the detail behind has_source_conflict when it is true: which channel, which
-- versions, how many sources, and when the disagreement was first detected. They stay
-- four flat columns rather than one JSONB blob because, unlike officialSources, this
-- is a single fixed-shape record, not a variable-length list, and a flat shape lets
-- the all-or-nothing CHECK below say something a JSONB shape could not enforce as
-- cheaply.
ALTER TABLE product_summaries
    ADD COLUMN official_sources        JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN conflict_channel        TEXT,
    ADD COLUMN conflict_versions       TEXT[],
    ADD COLUMN conflict_source_count   INTEGER,
    ADD COLUMN conflict_detected_at    TIMESTAMPTZ;

-- The four conflict_* columns describe one conflict record: either there is an open
-- conflict and all four are populated, or there is not and all four are NULL. Nothing
-- upstream can ever have a channel with no detection time or a detection time with no
-- channel, so this is the same choice 00001 made for releases_withdrawn_consistency
-- (a set of columns that are jointly present or jointly absent, enforced once here
-- instead of trusted to every future writer of this table). conflict_channel can
-- legitimately be the empty string -- domain.SourceConflict.Channel documents that a
-- conflict with no stated channel is real, not missing -- so this constraint tests
-- IS NULL, never = '' or similar, to keep that case distinct from "no conflict".
ALTER TABLE product_summaries
    ADD CONSTRAINT product_summaries_conflict_consistency CHECK (
        (conflict_channel IS NULL AND conflict_versions IS NULL
             AND conflict_source_count IS NULL AND conflict_detected_at IS NULL)
     OR (conflict_channel IS NOT NULL AND conflict_versions IS NOT NULL
             AND conflict_source_count IS NOT NULL AND conflict_detected_at IS NOT NULL)
    );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE product_summaries
    DROP CONSTRAINT IF EXISTS product_summaries_conflict_consistency,
    DROP COLUMN IF EXISTS official_sources,
    DROP COLUMN IF EXISTS conflict_channel,
    DROP COLUMN IF EXISTS conflict_versions,
    DROP COLUMN IF EXISTS conflict_source_count,
    DROP COLUMN IF EXISTS conflict_detected_at;
-- +goose StatementEnd
