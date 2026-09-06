-- +goose Up
-- +goose StatementBegin

-- Phase 3: a fleet's inventory is a list of model numbers, and until now the catalogue
-- had nowhere to put one. See ADR-0024.
--
-- Almost nothing here is new. A hardware model is a products row with model_identifier
-- set -- a column 00001 already declared and nothing has ever populated -- so it
-- inherits aliases, categories, the summary projection and full-text search unchanged.
-- What has no home in 31 tables is the navigation edge: nothing anywhere links a device
-- to the operating system whose releases the operator is actually looking for.

-- product_relationships is that edge, and only that edge.
--
-- relation_kind has exactly one member. "This device runs that operating system" is the
-- only product-to-product relationship FirmScout has measured evidence for; containment,
-- succession and bundling are plausible and unevidenced, and a vocabulary invented ahead
-- of its data produces columns nobody can populate honestly. It is a CHECK rather than a
-- lookup table because, unlike release_types, this vocabulary is a domain rule with a Go
-- constant behind it (domain.RelationKind) rather than registry data a contributor adds.
--
-- The two foreign keys deliberately differ. Deleting a device should take its edges with
-- it, so from_product_id CASCADEs. Deleting an operating system that devices point at
-- must be refused rather than silently leaving those device pages with nothing to name,
-- so to_product_id RESTRICTs -- the same split 00001 chose between
-- products.product_family_id (SET NULL) and releases.evidence_id (RESTRICT).
--
-- There is no evidence_id, on purpose. Every fact-bearing table in this schema carries
-- provenance, and an edge asserting a device fact is no exception -- but evidence rows
-- are collector artefacts, carrying content_hash, collector_id, collector_version and
-- discovery_method, and a relationship a human asserts in a reviewed pull request has
-- none of those. ADR-0016 already draws that line: registry facts are provenanced by Git
-- and by registry_path, observed facts by evidence. source_note carries the URL, the
-- fetch timestamp and the excerpt, exactly as product_aliases.source_note does.
CREATE TABLE product_relationships (
    id                  TEXT PRIMARY KEY,
    from_product_id     TEXT NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    to_product_id       TEXT NOT NULL REFERENCES products (id) ON DELETE RESTRICT,
    relation_kind       TEXT NOT NULL
                        CHECK (relation_kind IN ('runs_os')),
    source_note         TEXT,
    managed_by          TEXT NOT NULL DEFAULT 'registry'
                        CHECK (managed_by IN ('registry', 'admin')),
    registry_path       TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One assertion per (device, kind, target). A registry sync replaces a product's
    -- edges wholesale, so a duplicate here means two documents claim the same edge, and
    -- that is a contributor error worth a constraint violation rather than two rows the
    -- product page would render twice.
    UNIQUE (from_product_id, relation_kind, to_product_id),
    -- A product cannot run itself. The same invariant categories_no_self_parent states,
    -- for the same reason: the cycle is degenerate, it is cheap to exclude, and nothing
    -- downstream would render it sensibly.
    CONSTRAINT product_relationships_no_self CHECK (from_product_id <> to_product_id)
);

-- PostgreSQL indexes the referenced side of a foreign key, never the referencing side.
-- to_product_id RESTRICTs, so every attempt to delete an operating system has to check
-- this table, and without this index that check is a sequential scan. The reverse
-- question a later phase will ask -- "which devices run this OS?" -- reads the same
-- index. The (from_product_id, ...) direction needs no index of its own: the UNIQUE
-- constraint above already provides one with from_product_id as its leading column.
CREATE INDEX product_relationships_to_idx ON product_relationships (to_product_id);

-- The summary projection is the only thing a product page and search read, so a value
-- that is not a column here is a value no page can show.
--
-- model_identifier is display only -- "Product code: A42G-HbeP" on the page. It is
-- deliberately NOT added to the generated search_vector: a device is found by its model
-- number through a model_number product_alias, which is already flattened into
-- aliases_text at weight B, and regenerating a GENERATED ALWAYS column to index a string
-- the aliases already index would be work in exchange for a duplicate.
--
-- runs mirrors official_sources exactly: a JSONB array of {slug, name, kind}, because
-- product_summaries exists precisely so a product page is one indexed read, and a join
-- to products at read time would give that up for a list that is one element long in
-- every row this phase writes. DEFAULT '[]'::jsonb rather than NULL so every reader can
-- jsonb_array_elements it without a NULL check, and so an existing summary row written
-- before this migration reads as "runs nothing", which is true of it.
ALTER TABLE product_summaries
    ADD COLUMN model_identifier TEXT,
    ADD COLUMN runs             JSONB NOT NULL DEFAULT '[]'::jsonb;

-- 00001's release_mappings_latest_idx is partial on "product_id IS NOT NULL", so
-- family-targeted mapping rows are not merely permitted to duplicate the latest-observed
-- flag -- they fall outside the index's predicate entirely, and an unlimited number of
-- them may each claim to be latest for the same family and channel. ClearLatestFlag's
-- WHERE product_id = $1 cannot address one either, so nothing could ever clear one.
--
-- That hole has been unreachable because the registry contained zero families. This
-- change registers the first five, which makes a family the obvious next place somebody
-- maps a release, so the change that makes the hole reachable is the change that closes
-- it. Nothing writes a family-targeted mapping today and this phase deliberately writes
-- none (ADR-0024: a family here claims a shared image FILE, never a shared version), so
-- the index policies a path rather than constraining one -- which is exactly what 00003
-- did for review items.
--
-- The COALESCE(channel, '') expression is copied character for character from
-- release_mappings_latest_idx. A partial unique index is only usable by a predicate that
-- matches it exactly, and two expressions that differ by a space are two different
-- indexes.
CREATE UNIQUE INDEX release_mappings_family_latest_idx
    ON release_product_mappings (product_family_id, COALESCE(channel, ''))
    WHERE is_latest_observed = true AND product_family_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS release_mappings_family_latest_idx;
ALTER TABLE product_summaries
    DROP COLUMN IF EXISTS model_identifier,
    DROP COLUMN IF EXISTS runs;
DROP TABLE IF EXISTS product_relationships;
-- +goose StatementEnd
