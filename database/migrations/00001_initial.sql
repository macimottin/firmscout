-- +goose Up
-- +goose StatementBegin

-- FirmScout initial schema.
--
-- Design rules enforced structurally by this file, not by convention:
--
--  1. Registry tables carry `managed_by` and `registry_path` so rows synchronised from the
--     Git registry are distinguishable from rows created through an admin path.
--  2. `releases` is APPEND-ONLY. There is no UPDATE statement against it in the query set.
--     Withdrawals and corrections are new rows referencing the original.
--  3. Dates carry explicit precision. CHECK constraints force the stored DATE to be a
--     canonical anchor so a month-precision date can never be mistaken for a real day.
--  4. Version strings are TEXT. Nothing in this schema orders them.
--  5. Every candidate and every release points at evidence.
--  6. Compliance status gates dispatch (ADR-0018).

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ---------------------------------------------------------------------------
-- Catalog context: identity and naming.
-- ---------------------------------------------------------------------------

CREATE TABLE vendors (
    id              TEXT PRIMARY KEY,
    slug            TEXT NOT NULL UNIQUE,
    name            TEXT NOT NULL,
    legal_name      TEXT,
    homepage_url    TEXT,
    support_url     TEXT,
    country_code    TEXT,
    notes           TEXT,
    managed_by      TEXT NOT NULL DEFAULT 'registry'
                    CHECK (managed_by IN ('registry', 'admin')),
    registry_path   TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT vendors_slug_shape CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$')
);

CREATE TABLE categories (
    id              TEXT PRIMARY KEY,
    slug            TEXT NOT NULL UNIQUE,
    name            TEXT NOT NULL,
    parent_id       TEXT REFERENCES categories (id) ON DELETE RESTRICT,
    description     TEXT,
    managed_by      TEXT NOT NULL DEFAULT 'registry'
                    CHECK (managed_by IN ('registry', 'admin')),
    registry_path   TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT categories_slug_shape CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    CONSTRAINT categories_no_self_parent CHECK (parent_id IS NULL OR parent_id <> id)
);

CREATE TABLE product_families (
    id              TEXT PRIMARY KEY,
    vendor_id       TEXT NOT NULL REFERENCES vendors (id) ON DELETE RESTRICT,
    slug            TEXT NOT NULL,
    name            TEXT NOT NULL,
    description     TEXT,
    managed_by      TEXT NOT NULL DEFAULT 'registry'
                    CHECK (managed_by IN ('registry', 'admin')),
    registry_path   TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (vendor_id, slug),
    CONSTRAINT product_families_slug_shape CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$')
);

-- The vocabulary that prevents "everything is firmware". A lookup table rather than a
-- PostgreSQL ENUM so that adding a type is registry data, not a migration.
CREATE TABLE release_types (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    description     TEXT NOT NULL,
    sort_order      INTEGER NOT NULL DEFAULT 0
);

INSERT INTO release_types (id, name, description, sort_order) VALUES
    ('firmware',             'Firmware',                'Device firmware image',                                   10),
    ('bios',                 'BIOS',                    'System BIOS or UEFI system firmware',                     20),
    ('bmc_firmware',         'BMC firmware',            'Baseboard management controller firmware',                30),
    ('driver',               'Device driver',           'Host operating system device driver',                      40),
    ('operating_system',     'Operating system',        'General-purpose operating system release',                 50),
    ('embedded_os',          'Embedded operating system','Appliance or device operating system',                    60),
    ('application_software', 'Application software',    'Installable application software',                        70),
    ('saas_release',         'SaaS release',            'Hosted service release with an independent version',      80),
    ('management_platform',  'Management platform',     'Management or controller platform version',               90),
    ('security_update',      'Security update',         'Release published specifically to address a vulnerability',100),
    ('documentation_only',   'Documentation-only update','Documentation change with no software artifact',         110),
    ('unknown',              'Unknown',                 'Type not yet determined; requires classification',        999);

CREATE TABLE products (
    id                  TEXT PRIMARY KEY,
    vendor_id           TEXT NOT NULL REFERENCES vendors (id) ON DELETE RESTRICT,
    product_family_id   TEXT REFERENCES product_families (id) ON DELETE SET NULL,
    slug                TEXT NOT NULL UNIQUE,
    name                TEXT NOT NULL,
    model_identifier    TEXT,
    description         TEXT,
    default_release_type TEXT REFERENCES release_types (id) ON DELETE RESTRICT,
    lifecycle_status    TEXT NOT NULL DEFAULT 'active'
                        CHECK (lifecycle_status IN ('active', 'maintenance', 'eol_announced', 'eol', 'eos', 'unknown')),
    popularity_score    INTEGER NOT NULL DEFAULT 0,
    security_critical   BOOLEAN NOT NULL DEFAULT false,
    managed_by          TEXT NOT NULL DEFAULT 'registry'
                        CHECK (managed_by IN ('registry', 'admin')),
    registry_path       TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT products_slug_shape CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$')
);

CREATE INDEX products_vendor_idx ON products (vendor_id);
CREATE INDEX products_family_idx ON products (product_family_id);

CREATE TABLE product_categories (
    product_id      TEXT NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    category_id     TEXT NOT NULL REFERENCES categories (id) ON DELETE RESTRICT,
    PRIMARY KEY (product_id, category_id)
);

CREATE TABLE product_aliases (
    id              TEXT PRIMARY KEY,
    product_id      TEXT NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    alias           TEXT NOT NULL,
    normalized_alias TEXT NOT NULL,
    alias_kind      TEXT NOT NULL DEFAULT 'marketing_name'
                    CHECK (alias_kind IN ('marketing_name', 'model_number', 'sku', 'regional_name',
                                          'legacy_name', 'vendor_internal', 'common_misspelling')),
    source_note     TEXT,
    managed_by      TEXT NOT NULL DEFAULT 'registry'
                    CHECK (managed_by IN ('registry', 'admin')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (product_id, normalized_alias)
);

CREATE INDEX product_aliases_normalized_idx ON product_aliases (normalized_alias);
CREATE INDEX product_aliases_trgm_idx ON product_aliases USING gin (normalized_alias gin_trgm_ops);

-- ---------------------------------------------------------------------------
-- Sourcing context: where facts come from, and whether they changed.
-- ---------------------------------------------------------------------------

CREATE TABLE collector_definitions (
    id              TEXT PRIMARY KEY,
    collector_id    TEXT NOT NULL,
    version         INTEGER NOT NULL,
    engine          TEXT NOT NULL
                    CHECK (engine IN ('html_selectors', 'text_regex', 'json_path', 'rss_atom',
                                      'xml_xpath', 'pdf_text', 'github_releases', 'code')),
    vendor_id       TEXT REFERENCES vendors (id) ON DELETE RESTRICT,
    config          JSONB NOT NULL,
    config_hash     TEXT NOT NULL,
    managed_by      TEXT NOT NULL DEFAULT 'registry'
                    CHECK (managed_by IN ('registry', 'admin')),
    registry_path   TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (collector_id, version)
);

CREATE TABLE sources (
    id                      TEXT PRIMARY KEY,
    vendor_id               TEXT NOT NULL REFERENCES vendors (id) ON DELETE RESTRICT,
    product_id              TEXT REFERENCES products (id) ON DELETE CASCADE,
    product_family_id       TEXT REFERENCES product_families (id) ON DELETE CASCADE,
    slug                    TEXT NOT NULL,
    source_type             TEXT NOT NULL
                            CHECK (source_type IN ('rest_api', 'graphql_api', 'rss_atom', 'xml_feed',
                                                   'json_endpoint', 'html_page', 'pdf_release_notes',
                                                   'download_portal', 'github_releases', 'sitemap',
                                                   'authenticated_portal', 'manual')),
    source_url              TEXT NOT NULL,

    -- Authority and compliance (ADR-0018). These gate dispatch.
    official                BOOLEAN NOT NULL DEFAULT false,
    quality_class           TEXT NOT NULL DEFAULT 'unknown_third_party'
                            CHECK (quality_class IN ('official_manufacturer', 'authorized_portal',
                                                     'vendor_repository', 'trusted_community',
                                                     'unknown_third_party')),
    robots_policy_status    TEXT NOT NULL DEFAULT 'unknown'
                            CHECK (robots_policy_status IN ('allowed', 'disallowed', 'unknown', 'not_applicable')),
    robots_checked_at       TIMESTAMPTZ,
    terms_review_status     TEXT NOT NULL DEFAULT 'pending'
                            CHECK (terms_review_status IN ('pending', 'approved', 'restricted', 'prohibited')),
    terms_review_note       TEXT,
    authentication_type     TEXT NOT NULL DEFAULT 'none'
                            CHECK (authentication_type IN ('none', 'api_key', 'account_required', 'entitlement_required')),
    enabled                 BOOLEAN NOT NULL DEFAULT false,

    -- Collection configuration.
    collector_id            TEXT,
    parser_configuration    JSONB NOT NULL DEFAULT '{}'::jsonb,
    expected_content_type   TEXT,
    check_frequency_seconds INTEGER NOT NULL DEFAULT 86400
                            CHECK (check_frequency_seconds >= 60),
    min_frequency_seconds   INTEGER
                            CHECK (min_frequency_seconds IS NULL OR min_frequency_seconds >= 60),

    -- Change-detection state, updated by CheckSource.
    etag                    TEXT,
    last_modified_value     TEXT,
    normalized_content_hash TEXT,
    last_checked_at         TIMESTAMPTZ,
    last_changed_at         TIMESTAMPTZ,
    last_success_at         TIMESTAMPTZ,
    next_check_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    consecutive_failures    INTEGER NOT NULL DEFAULT 0,
    consecutive_unchanged   INTEGER NOT NULL DEFAULT 0,
    retry_after_until       TIMESTAMPTZ,

    -- Health state machine.
    health_status           TEXT NOT NULL DEFAULT 'discovered'
                            CHECK (health_status IN ('discovered', 'pending_review', 'active', 'degraded',
                                                     'rate_limited', 'authentication_required', 'broken',
                                                     'relocated', 'disabled', 'retired')),
    relocated_to_source_id  TEXT REFERENCES sources (id) ON DELETE SET NULL,
    confidence_score        NUMERIC(3, 2) NOT NULL DEFAULT 0.50
                            CHECK (confidence_score >= 0 AND confidence_score <= 1),

    created_by              TEXT,
    approved_by             TEXT,
    approved_at             TIMESTAMPTZ,
    managed_by              TEXT NOT NULL DEFAULT 'registry'
                            CHECK (managed_by IN ('registry', 'admin')),
    registry_path           TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (vendor_id, slug),
    -- A source covers a specific product, a whole family, or (both null) a vendor-wide
    -- catalogue whose applicability is resolved at extraction time.
    CONSTRAINT sources_scope CHECK (NOT (product_id IS NOT NULL AND product_family_id IS NOT NULL))
);

-- The dispatch predicate lives in an index so the scheduler cannot accidentally omit it.
CREATE INDEX sources_dispatchable_idx ON sources (next_check_at)
    WHERE enabled = true
      AND health_status IN ('active', 'degraded')
      AND robots_policy_status IN ('allowed', 'not_applicable')
      AND terms_review_status IN ('approved', 'restricted');

CREATE INDEX sources_vendor_idx ON sources (vendor_id);
CREATE INDEX sources_product_idx ON sources (product_id);
CREATE INDEX sources_health_idx ON sources (health_status);

-- A source may cover many products explicitly (one catalogue page, forty devices).
CREATE TABLE source_products (
    source_id       TEXT NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    product_id      TEXT NOT NULL REFERENCES products (id) ON DELETE CASCADE,
    PRIMARY KEY (source_id, product_id)
);

-- Content-addressed artifact store. An unchanged source stores nothing new; a new
-- source_checks row simply points at the existing artifact.
CREATE TABLE source_artifacts (
    id                  TEXT PRIMARY KEY,
    content_hash        TEXT NOT NULL UNIQUE,
    content_type        TEXT,
    byte_size           BIGINT NOT NULL CHECK (byte_size >= 0),
    storage_backend     TEXT NOT NULL DEFAULT 'filesystem'
                        CHECK (storage_backend IN ('filesystem', 's3', 'inline')),
    storage_key         TEXT,
    inline_content      BYTEA,
    compression         TEXT NOT NULL DEFAULT 'none'
                        CHECK (compression IN ('none', 'gzip', 'zstd')),
    retention_class     TEXT NOT NULL DEFAULT 'temporarily_required'
                        CHECK (retention_class IN ('permanent', 'audit_required', 'temporarily_required',
                                                   'reconstructable', 'disposable')),
    expires_at          TIMESTAMPTZ,
    first_seen_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    reference_count     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX source_artifacts_expiry_idx ON source_artifacts (expires_at)
    WHERE expires_at IS NOT NULL;

CREATE TABLE source_checks (
    id                      TEXT PRIMARY KEY,
    source_id               TEXT NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    started_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at             TIMESTAMPTZ,
    duration_ms             INTEGER,
    outcome                 TEXT NOT NULL
                            CHECK (outcome IN ('unchanged', 'changed', 'unavailable', 'unauthorized',
                                               'rate_limited', 'redirected', 'parser_failed',
                                               'suspicious_content', 'manual_review_required')),
    change_signal           TEXT
                            CHECK (change_signal IS NULL OR change_signal IN ('webhook', 'feed', 'api_cursor',
                                   'etag', 'last_modified', 'conditional_get', 'sitemap_lastmod',
                                   'section_hash', 'content_hash', 'full_compare')),
    http_status             INTEGER,
    response_etag           TEXT,
    response_last_modified  TEXT,
    redirect_location       TEXT,
    normalized_content_hash TEXT,
    artifact_id             TEXT REFERENCES source_artifacts (id) ON DELETE SET NULL,
    bytes_fetched           BIGINT,
    error_message           TEXT,
    trace_id                TEXT,
    request_id              TEXT
);

CREATE INDEX source_checks_source_time_idx ON source_checks (source_id, started_at DESC);
CREATE INDEX source_checks_outcome_idx ON source_checks (outcome, started_at DESC);

CREATE TABLE collector_runs (
    id                      TEXT PRIMARY KEY,
    source_id               TEXT NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    source_check_id         TEXT REFERENCES source_checks (id) ON DELETE SET NULL,
    collector_id            TEXT NOT NULL,
    collector_version       TEXT NOT NULL,
    artifact_id             TEXT REFERENCES source_artifacts (id) ON DELETE SET NULL,
    started_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at             TIMESTAMPTZ,
    duration_ms             INTEGER,
    status                  TEXT NOT NULL DEFAULT 'running'
                            CHECK (status IN ('running', 'succeeded', 'failed', 'timed_out', 'limit_exceeded')),
    candidates_extracted    INTEGER NOT NULL DEFAULT 0,
    error_message           TEXT,
    trace_id                TEXT
);

CREATE INDEX collector_runs_source_idx ON collector_runs (source_id, started_at DESC);

-- ---------------------------------------------------------------------------
-- Ingestion context: turning observations into published facts.
-- ---------------------------------------------------------------------------

-- Evidence is the provenance record for every fact FirmScout publishes or considers.
CREATE TABLE evidence (
    id                  TEXT PRIMARY KEY,
    source_id           TEXT REFERENCES sources (id) ON DELETE SET NULL,
    source_url          TEXT NOT NULL,
    source_type         TEXT,
    official            BOOLEAN NOT NULL DEFAULT false,
    retrieved_at        TIMESTAMPTZ NOT NULL,
    artifact_id         TEXT REFERENCES source_artifacts (id) ON DELETE SET NULL,
    content_hash        TEXT,
    excerpt             TEXT NOT NULL,
    raw_value           TEXT,
    normalized_value    TEXT,
    collector_id        TEXT,
    collector_version   TEXT,
    discovery_method    TEXT NOT NULL DEFAULT 'deterministic'
                        CHECK (discovery_method IN ('deterministic', 'ai_assisted', 'manual', 'community')),
    ai_model_id         TEXT,
    ai_prompt_version   TEXT,
    confidence_score    NUMERIC(3, 2) CHECK (confidence_score IS NULL OR (confidence_score >= 0 AND confidence_score <= 1)),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- An AI-assisted fact must record which model and prompt produced it.
    CONSTRAINT evidence_ai_provenance CHECK (
        discovery_method <> 'ai_assisted' OR (ai_model_id IS NOT NULL AND ai_prompt_version IS NOT NULL)
    ),
    CONSTRAINT evidence_excerpt_not_blank CHECK (length(btrim(excerpt)) > 0)
);

CREATE INDEX evidence_source_idx ON evidence (source_id);

CREATE TABLE candidate_releases (
    id                      TEXT PRIMARY KEY,
    source_id               TEXT NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    collector_run_id        TEXT REFERENCES collector_runs (id) ON DELETE SET NULL,
    evidence_id             TEXT REFERENCES evidence (id) ON DELETE SET NULL,

    -- Product resolution. Null product_id means the match is unresolved or ambiguous.
    product_id              TEXT REFERENCES products (id) ON DELETE CASCADE,
    product_family_id       TEXT REFERENCES product_families (id) ON DELETE CASCADE,
    product_match_hint      TEXT,
    product_match_status    TEXT NOT NULL DEFAULT 'unresolved'
                            CHECK (product_match_status IN ('unresolved', 'unique', 'ambiguous', 'no_match')),

    raw_version             TEXT NOT NULL,
    normalized_version      TEXT NOT NULL,
    release_type            TEXT REFERENCES release_types (id) ON DELETE RESTRICT,
    proposed_release_type   TEXT REFERENCES release_types (id) ON DELETE RESTRICT,
    channel                 TEXT,
    region                  TEXT,
    hardware_revision       TEXT,
    deployment_mode         TEXT,
    applicability_note      TEXT,

    release_date            DATE,
    release_date_precision  TEXT NOT NULL DEFAULT 'unknown'
                            CHECK (release_date_precision IN ('exact_day', 'month_only', 'year_only', 'unknown')),
    publication_date        DATE,
    publication_date_precision TEXT NOT NULL DEFAULT 'unknown'
                            CHECK (publication_date_precision IN ('exact_day', 'month_only', 'year_only', 'unknown')),
    release_notes_url       TEXT,

    confidence_score        NUMERIC(3, 2) NOT NULL DEFAULT 0.50
                            CHECK (confidence_score >= 0 AND confidence_score <= 1),
    dedupe_key              TEXT NOT NULL,
    state                   TEXT NOT NULL DEFAULT 'discovered'
                            CHECK (state IN ('discovered', 'extracted', 'normalized', 'validation_pending',
                                             'validated', 'human_review_required', 'rejected', 'published',
                                             'superseded')),
    rejection_reason        TEXT,
    published_release_id    TEXT,
    discovered_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT candidate_version_not_blank CHECK (length(btrim(normalized_version)) > 0),
    -- Date precision discipline: the stored DATE is a canonical anchor, never an invented day.
    CONSTRAINT candidate_date_precision CHECK (
        (release_date_precision = 'unknown'    AND release_date IS NULL)
     OR (release_date_precision = 'exact_day'  AND release_date IS NOT NULL)
     OR (release_date_precision = 'month_only' AND release_date IS NOT NULL
             AND EXTRACT(DAY FROM release_date) = 1)
     OR (release_date_precision = 'year_only'  AND release_date IS NOT NULL
             AND EXTRACT(DAY FROM release_date) = 1 AND EXTRACT(MONTH FROM release_date) = 1)
    ),
    CONSTRAINT candidate_publication_date_precision CHECK (
        (publication_date_precision = 'unknown'    AND publication_date IS NULL)
     OR (publication_date_precision = 'exact_day'  AND publication_date IS NOT NULL)
     OR (publication_date_precision = 'month_only' AND publication_date IS NOT NULL
             AND EXTRACT(DAY FROM publication_date) = 1)
     OR (publication_date_precision = 'year_only'  AND publication_date IS NOT NULL
             AND EXTRACT(DAY FROM publication_date) = 1 AND EXTRACT(MONTH FROM publication_date) = 1)
    )
);

CREATE UNIQUE INDEX candidate_releases_dedupe_idx ON candidate_releases (source_id, dedupe_key);
CREATE INDEX candidate_releases_state_idx ON candidate_releases (state, discovered_at);
CREATE INDEX candidate_releases_product_idx ON candidate_releases (product_id);

CREATE TABLE validation_results (
    id                  TEXT PRIMARY KEY,
    candidate_id        TEXT NOT NULL REFERENCES candidate_releases (id) ON DELETE CASCADE,
    gate                TEXT NOT NULL,
    gate_order          INTEGER NOT NULL,
    outcome             TEXT NOT NULL
                        CHECK (outcome IN ('passed', 'rejected', 'review_required', 'skipped')),
    detail              TEXT,
    evaluated_by        TEXT NOT NULL DEFAULT 'deterministic'
                        CHECK (evaluated_by IN ('deterministic', 'ai', 'human')),
    evaluated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX validation_results_candidate_idx ON validation_results (candidate_id, gate_order);

-- APPEND-ONLY. No UPDATE statement targets this table.
CREATE TABLE releases (
    id                      TEXT PRIMARY KEY,
    vendor_id               TEXT NOT NULL REFERENCES vendors (id) ON DELETE RESTRICT,
    raw_version             TEXT NOT NULL,
    normalized_version      TEXT NOT NULL,
    release_type            TEXT NOT NULL REFERENCES release_types (id) ON DELETE RESTRICT,
    channel                 TEXT,

    release_date            DATE,
    release_date_precision  TEXT NOT NULL
                            CHECK (release_date_precision IN ('exact_day', 'month_only', 'year_only', 'unknown')),
    publication_date        DATE,
    publication_date_precision TEXT NOT NULL DEFAULT 'unknown'
                            CHECK (publication_date_precision IN ('exact_day', 'month_only', 'year_only', 'unknown')),

    first_observed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_verified_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    release_notes_url       TEXT,
    stable                  BOOLEAN,
    recommended             BOOLEAN,          -- NULL unless the vendor explicitly says so.
    withdrawn               BOOLEAN NOT NULL DEFAULT false,
    withdrawn_at            TIMESTAMPTZ,
    withdrawn_reason        TEXT,

    -- Correction and supersession chains. History is immutable; these are new rows.
    corrects_release_id     TEXT REFERENCES releases (id) ON DELETE RESTRICT,
    superseded_by_release_id TEXT REFERENCES releases (id) ON DELETE RESTRICT,

    candidate_id            TEXT REFERENCES candidate_releases (id) ON DELETE SET NULL,
    collector_run_id        TEXT REFERENCES collector_runs (id) ON DELETE SET NULL,
    evidence_id             TEXT NOT NULL REFERENCES evidence (id) ON DELETE RESTRICT,
    source_confidence       NUMERIC(3, 2) NOT NULL DEFAULT 0.50
                            CHECK (source_confidence >= 0 AND source_confidence <= 1),
    approved_by             TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT releases_version_not_blank CHECK (length(btrim(normalized_version)) > 0),
    CONSTRAINT releases_date_precision CHECK (
        (release_date_precision = 'unknown'    AND release_date IS NULL)
     OR (release_date_precision = 'exact_day'  AND release_date IS NOT NULL)
     OR (release_date_precision = 'month_only' AND release_date IS NOT NULL
             AND EXTRACT(DAY FROM release_date) = 1)
     OR (release_date_precision = 'year_only'  AND release_date IS NOT NULL
             AND EXTRACT(DAY FROM release_date) = 1 AND EXTRACT(MONTH FROM release_date) = 1)
    ),
    CONSTRAINT releases_publication_date_precision CHECK (
        (publication_date_precision = 'unknown'    AND publication_date IS NULL)
     OR (publication_date_precision = 'exact_day'  AND publication_date IS NOT NULL)
     OR (publication_date_precision = 'month_only' AND publication_date IS NOT NULL
             AND EXTRACT(DAY FROM publication_date) = 1)
     OR (publication_date_precision = 'year_only'  AND publication_date IS NOT NULL
             AND EXTRACT(DAY FROM publication_date) = 1 AND EXTRACT(MONTH FROM publication_date) = 1)
    ),
    CONSTRAINT releases_withdrawn_consistency CHECK (
        (withdrawn = false AND withdrawn_at IS NULL)
     OR (withdrawn = true  AND withdrawn_at IS NOT NULL)
    )
);

CREATE INDEX releases_vendor_idx ON releases (vendor_id);
CREATE INDEX releases_date_idx ON releases (release_date DESC NULLS LAST);
CREATE INDEX releases_first_observed_idx ON releases (first_observed_at DESC);

-- Applicability. One release may map to one model, several models, a family, a hardware
-- revision, a region, a channel or a deployment mode.
CREATE TABLE release_product_mappings (
    id                  TEXT PRIMARY KEY,
    release_id          TEXT NOT NULL REFERENCES releases (id) ON DELETE RESTRICT,
    product_id          TEXT REFERENCES products (id) ON DELETE CASCADE,
    product_family_id   TEXT REFERENCES product_families (id) ON DELETE CASCADE,
    hardware_revision   TEXT,
    region              TEXT,
    channel             TEXT,
    deployment_mode     TEXT,
    applicability_note  TEXT,
    -- Derived flag, not a fact: which release is currently latest for this product+channel.
    is_latest_observed  BOOLEAN NOT NULL DEFAULT false,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT release_mapping_target CHECK (
        (product_id IS NOT NULL AND product_family_id IS NULL)
     OR (product_id IS NULL     AND product_family_id IS NOT NULL)
    )
);

CREATE INDEX release_mappings_release_idx ON release_product_mappings (release_id);
CREATE INDEX release_mappings_product_idx ON release_product_mappings (product_id);

-- At most one latest-observed release per product and channel.
CREATE UNIQUE INDEX release_mappings_latest_idx
    ON release_product_mappings (product_id, COALESCE(channel, ''))
    WHERE is_latest_observed = true AND product_id IS NOT NULL;

CREATE TABLE release_notes (
    id              TEXT PRIMARY KEY,
    release_id      TEXT NOT NULL REFERENCES releases (id) ON DELETE RESTRICT,
    language        TEXT NOT NULL DEFAULT 'en',
    title           TEXT,
    -- A concise excerpt for verification. Full documents are linked, never reproduced.
    excerpt         TEXT,
    canonical_url   TEXT NOT NULL,
    content_type    TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT release_notes_excerpt_bounded CHECK (excerpt IS NULL OR length(excerpt) <= 4000)
);

CREATE TABLE review_items (
    id                  TEXT PRIMARY KEY,
    kind                TEXT NOT NULL
                        CHECK (kind IN ('candidate_low_confidence', 'candidate_implausible_transition',
                                        'product_match_ambiguous', 'multi_source_conflict',
                                        'ai_proposal', 'source_broken', 'source_relocated',
                                        'community_correction', 'new_source_proposal',
                                        'terms_review_required')),
    subject_type        TEXT NOT NULL,
    subject_id          TEXT NOT NULL,
    vendor_id           TEXT REFERENCES vendors (id) ON DELETE SET NULL,
    product_id          TEXT REFERENCES products (id) ON DELETE SET NULL,
    title               TEXT NOT NULL,
    detail              TEXT,
    payload             JSONB NOT NULL DEFAULT '{}'::jsonb,
    priority_score      INTEGER NOT NULL DEFAULT 0,
    sla_class           TEXT NOT NULL DEFAULT 'standard'
                        CHECK (sla_class IN ('urgent', 'high', 'standard', 'low')),
    state               TEXT NOT NULL DEFAULT 'open'
                        CHECK (state IN ('open', 'in_progress', 'resolved', 'dismissed')),
    resolution          TEXT,
    assigned_to         TEXT,
    resolved_by         TEXT,
    resolved_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX review_items_queue_idx ON review_items (state, priority_score DESC, created_at)
    WHERE state IN ('open', 'in_progress');

-- ---------------------------------------------------------------------------
-- Distribution context: serving the public.
-- ---------------------------------------------------------------------------

-- Precomputed per-product summary. The public site and search read this, not the joins.
CREATE TABLE product_summaries (
    product_id              TEXT PRIMARY KEY REFERENCES products (id) ON DELETE CASCADE,
    vendor_slug             TEXT NOT NULL,
    vendor_name             TEXT NOT NULL,
    product_slug            TEXT NOT NULL,
    product_name            TEXT NOT NULL,
    family_name             TEXT,
    aliases                 TEXT[] NOT NULL DEFAULT '{}',
    category_slugs          TEXT[] NOT NULL DEFAULT '{}',
    latest_release_id       TEXT REFERENCES releases (id) ON DELETE SET NULL,
    latest_raw_version      TEXT,
    latest_release_type     TEXT,
    latest_channel          TEXT,
    latest_release_date     DATE,
    latest_release_date_precision TEXT
                            CHECK (latest_release_date_precision IS NULL OR
                                   latest_release_date_precision IN ('exact_day', 'month_only', 'year_only', 'unknown')),
    recommended_release_id  TEXT REFERENCES releases (id) ON DELETE SET NULL,
    release_count           INTEGER NOT NULL DEFAULT 0,
    lifecycle_status        TEXT NOT NULL DEFAULT 'unknown',
    has_source_conflict     BOOLEAN NOT NULL DEFAULT false,
    advisory_count          INTEGER NOT NULL DEFAULT 0,
    last_verified_at        TIMESTAMPTZ,
    refreshed_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- aliases_text is the flattened form of `aliases`, maintained by the same use case
    -- that writes this row. It exists because a generated column's expression must be
    -- IMMUTABLE, and array_to_string is only STABLE -- PostgreSQL rejects it here
    -- (SQLSTATE 42P17). Flattening in the application keeps the generated column
    -- immutable and keeps alias text searchable.
    aliases_text            TEXT NOT NULL DEFAULT '',
    search_vector           tsvector GENERATED ALWAYS AS (
                                setweight(to_tsvector('simple', coalesce(product_name, '')), 'A') ||
                                setweight(to_tsvector('simple', coalesce(vendor_name, '')), 'B') ||
                                setweight(to_tsvector('simple', coalesce(aliases_text, '')), 'B') ||
                                setweight(to_tsvector('simple', coalesce(family_name, '')), 'C')
                            ) STORED
);

CREATE INDEX product_summaries_search_idx ON product_summaries USING gin (search_vector);
CREATE INDEX product_summaries_name_trgm_idx ON product_summaries USING gin (product_name gin_trgm_ops);
CREATE INDEX product_summaries_vendor_idx ON product_summaries (vendor_slug);

CREATE TABLE api_consumers (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    contact_email       TEXT,
    plan                TEXT NOT NULL DEFAULT 'free'
                        CHECK (plan IN ('anonymous', 'free', 'professional', 'enterprise', 'internal')),
    status              TEXT NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'suspended', 'closed')),
    monthly_quota       BIGINT NOT NULL DEFAULT 1000,
    rate_limit_per_min  INTEGER NOT NULL DEFAULT 60,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id                  TEXT PRIMARY KEY,
    consumer_id         TEXT NOT NULL REFERENCES api_consumers (id) ON DELETE CASCADE,
    -- SHA-256 of the secret. The plaintext is displayed once at creation and never stored.
    key_hash            TEXT NOT NULL UNIQUE,
    key_prefix          TEXT NOT NULL,
    label               TEXT,
    scopes              TEXT[] NOT NULL DEFAULT '{}',
    status              TEXT NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'rotating', 'revoked', 'expired')),
    last_used_at        TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ,
    revoked_at          TIMESTAMPTZ,
    revoked_reason      TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT api_keys_prefix_shape CHECK (length(key_prefix) BETWEEN 4 AND 16)
);

CREATE INDEX api_keys_consumer_idx ON api_keys (consumer_id);
CREATE INDEX api_keys_prefix_idx ON api_keys (key_prefix);

CREATE TABLE usage_records (
    id                  TEXT PRIMARY KEY,
    idempotency_key     TEXT NOT NULL UNIQUE,
    consumer_id         TEXT REFERENCES api_consumers (id) ON DELETE SET NULL,
    api_key_id          TEXT REFERENCES api_keys (id) ON DELETE SET NULL,
    endpoint            TEXT NOT NULL,
    method              TEXT NOT NULL,
    status_code         INTEGER NOT NULL,
    quota_weight        INTEGER NOT NULL DEFAULT 1,
    duration_ms         INTEGER,
    bytes_out           BIGINT,
    vendor_slug         TEXT,
    product_slug        TEXT,
    rate_limited        BOOLEAN NOT NULL DEFAULT false,
    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX usage_records_consumer_time_idx ON usage_records (consumer_id, occurred_at DESC);
CREATE INDEX usage_records_time_idx ON usage_records (occurred_at DESC);

CREATE TABLE usage_aggregates (
    consumer_id         TEXT NOT NULL REFERENCES api_consumers (id) ON DELETE CASCADE,
    period_start        DATE NOT NULL,
    period_kind         TEXT NOT NULL CHECK (period_kind IN ('day', 'month')),
    request_count       BIGINT NOT NULL DEFAULT 0,
    quota_consumed      BIGINT NOT NULL DEFAULT 0,
    error_count         BIGINT NOT NULL DEFAULT 0,
    rate_limited_count  BIGINT NOT NULL DEFAULT 0,
    bytes_out           BIGINT NOT NULL DEFAULT 0,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer_id, period_kind, period_start)
);

-- ---------------------------------------------------------------------------
-- Platform: jobs, analytics, audit, AI runs.
-- ---------------------------------------------------------------------------

CREATE TABLE jobs (
    id                  TEXT PRIMARY KEY,
    kind                TEXT NOT NULL,
    idempotency_key     TEXT NOT NULL UNIQUE,
    payload             JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- W3C trace context, so a trace survives the queue hop.
    trace_context       JSONB NOT NULL DEFAULT '{}'::jsonb,
    status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'dead')),
    priority            INTEGER NOT NULL DEFAULT 100,
    attempts            INTEGER NOT NULL DEFAULT 0,
    max_attempts        INTEGER NOT NULL DEFAULT 5,
    run_after           TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until        TIMESTAMPTZ,
    locked_by           TEXT,
    last_error          TEXT,
    dead_lettered_at    TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The dequeue index: pending or expired-lock jobs whose run_after has passed.
CREATE INDEX jobs_dequeue_idx ON jobs (priority, run_after)
    WHERE status IN ('pending', 'running');
CREATE INDEX jobs_dead_idx ON jobs (dead_lettered_at) WHERE status = 'dead';

CREATE TABLE analytics_events (
    id                  TEXT PRIMARY KEY,
    event_name          TEXT NOT NULL,
    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Privacy-filtered before insertion. Never contains keys, tokens, credentials,
    -- customer inventory contents, or raw IP addresses.
    properties          JSONB NOT NULL DEFAULT '{}'::jsonb,
    vendor_slug         TEXT,
    product_slug        TEXT,
    country_code        TEXT,
    consumer_plan       TEXT,
    request_id          TEXT
);

CREATE INDEX analytics_events_name_time_idx ON analytics_events (event_name, occurred_at DESC);
CREATE INDEX analytics_events_time_idx ON analytics_events (occurred_at DESC);

CREATE TABLE audit_events (
    id                  TEXT PRIMARY KEY,
    actor_type          TEXT NOT NULL CHECK (actor_type IN ('human', 'system', 'ai', 'contributor')),
    actor_id            TEXT,
    action              TEXT NOT NULL,
    subject_type        TEXT NOT NULL,
    subject_id          TEXT NOT NULL,
    before_state        JSONB,
    after_state         JSONB,
    reason              TEXT,
    request_id          TEXT,
    trace_id            TEXT,
    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_subject_idx ON audit_events (subject_type, subject_id, occurred_at DESC);
CREATE INDEX audit_events_time_idx ON audit_events (occurred_at DESC);

CREATE TABLE ai_runs (
    id                      TEXT PRIMARY KEY,
    agent                   TEXT NOT NULL
                            CHECK (agent IN ('discovery', 'repair', 'validation', 'classification', 'source_quality')),
    trigger_reason          TEXT NOT NULL,
    subject_type            TEXT,
    subject_id              TEXT,
    vendor_id               TEXT REFERENCES vendors (id) ON DELETE SET NULL,
    input_artifact_ids      TEXT[] NOT NULL DEFAULT '{}',
    prompt_version          TEXT NOT NULL,
    model_id                TEXT NOT NULL,
    input_tokens            INTEGER NOT NULL DEFAULT 0,
    output_tokens           INTEGER NOT NULL DEFAULT 0,
    estimated_cost_usd      NUMERIC(10, 6) NOT NULL DEFAULT 0,
    budget_cap_usd          NUMERIC(10, 6),
    output                  JSONB,
    schema_valid            BOOLEAN NOT NULL DEFAULT false,
    outcome                 TEXT NOT NULL DEFAULT 'pending'
                            CHECK (outcome IN ('pending', 'succeeded', 'schema_invalid', 'budget_exceeded',
                                               'failed', 'rejected_by_human', 'accepted_by_human')),
    human_decision          TEXT,
    human_decided_by        TEXT,
    human_decided_at        TIMESTAMPTZ,
    review_item_id          TEXT REFERENCES review_items (id) ON DELETE SET NULL,
    started_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at             TIMESTAMPTZ,
    trace_id                TEXT
);

CREATE INDEX ai_runs_agent_time_idx ON ai_runs (agent, started_at DESC);
CREATE INDEX ai_runs_vendor_idx ON ai_runs (vendor_id, started_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS ai_runs;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS analytics_events;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS usage_aggregates;
DROP TABLE IF EXISTS usage_records;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS api_consumers;
DROP TABLE IF EXISTS product_summaries;
DROP TABLE IF EXISTS review_items;
DROP TABLE IF EXISTS release_notes;
DROP TABLE IF EXISTS release_product_mappings;
DROP TABLE IF EXISTS releases;
DROP TABLE IF EXISTS validation_results;
DROP TABLE IF EXISTS candidate_releases;
DROP TABLE IF EXISTS evidence;
DROP TABLE IF EXISTS collector_runs;
DROP TABLE IF EXISTS source_checks;
DROP TABLE IF EXISTS source_artifacts;
DROP TABLE IF EXISTS source_products;
DROP TABLE IF EXISTS sources;
DROP TABLE IF EXISTS collector_definitions;
DROP TABLE IF EXISTS product_aliases;
DROP TABLE IF EXISTS product_categories;
DROP TABLE IF EXISTS products;
DROP TABLE IF EXISTS release_types;
DROP TABLE IF EXISTS product_families;
DROP TABLE IF EXISTS categories;
DROP TABLE IF EXISTS vendors;
-- +goose StatementEnd
