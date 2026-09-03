// Package application holds FirmScout's use cases and the ports they depend on.
//
// Ports are interfaces defined here because the application decides what it needs;
// adapters in internal/adapters implement them. This package imports internal/domain
// and the standard library, and nothing else. No SQL driver, no HTTP framework, no AWS
// SDK, no AI SDK. The rule is enforced by internal/archtest.
//
// Ports exist where substitution is genuinely needed: persistence, network, queue,
// clock, identifiers and AI. There is no interface for a value object, and no
// abstraction over PostgreSQL "in case we switch" -- the persistence port exists so
// use cases can be tested in microseconds, and the PostgreSQL adapter is free to use
// PostgreSQL-specific features.
package application

import (
	"context"
	"io"
	"time"

	"github.com/macimottin/firmscout/internal/domain"
)

// ---------------------------------------------------------------------------
// Infrastructure primitives
// ---------------------------------------------------------------------------

// Clock supplies the current time. Injecting it is what makes scheduling and
// validation tests deterministic.
type Clock interface {
	Now() time.Time
}

// IDGenerator produces prefixed identifiers such as "src_01J...".
type IDGenerator interface {
	NewID(prefix string) string
}

// UnitOfWork runs fn inside a single transaction. The application owns transaction
// boundaries because a repository cannot know whether it is the whole operation or
// part of one.
//
// The context passed to fn carries the transaction; repositories resolve it from
// there, so a use case never handles a driver-specific transaction type.
type UnitOfWork interface {
	Within(ctx context.Context, fn func(ctx context.Context) error) error
}

// EventPublisher dispatches domain and analytics events. In the MVP this writes to
// PostgreSQL and the in-process job queue; on AWS the same events can be forwarded to
// EventBridge without the domain changing.
type EventPublisher interface {
	Publish(ctx context.Context, events ...domain.Event) error
}

// ---------------------------------------------------------------------------
// Catalog repositories
// ---------------------------------------------------------------------------

// VendorRepository persists vendors.
type VendorRepository interface {
	GetByID(ctx context.Context, id string) (domain.Vendor, error)
	GetBySlug(ctx context.Context, slug string) (domain.Vendor, error)
	List(ctx context.Context, limit int, cursor string) ([]domain.Vendor, string, error)
	Upsert(ctx context.Context, v domain.Vendor) error
}

// CategoryRepository persists the product category vocabulary.
type CategoryRepository interface {
	GetBySlug(ctx context.Context, slug string) (domain.Category, error)
	List(ctx context.Context) ([]domain.Category, error)
	Upsert(ctx context.Context, c domain.Category) error
}

// ProductRepository persists products, families and aliases.
type ProductRepository interface {
	GetByID(ctx context.Context, id string) (domain.Product, error)
	GetBySlug(ctx context.Context, slug string) (domain.Product, error)
	ListByVendor(ctx context.Context, vendorID string, limit int, cursor string) ([]domain.Product, string, error)
	Upsert(ctx context.Context, p domain.Product) error

	UpsertFamily(ctx context.Context, f domain.ProductFamily) error
	GetFamilyBySlug(ctx context.Context, vendorID, slug string) (domain.ProductFamily, error)

	ReplaceAliases(ctx context.Context, productID string, aliases []domain.ProductAlias) error
	ListAliases(ctx context.Context, productID string) ([]domain.ProductAlias, error)

	// ResolveByAlias returns every product a source's product hint could refer to.
	//
	// The hint is the raw text as the source wrote it -- a slug, a marketing name, a
	// model number, whatever the page said. Implementations compare it against the
	// product slug directly and against alias entries in their normalised form, so
	// the caller never has to guess which shape to pass.
	//
	// More than one result means the match is ambiguous, and ambiguity is never
	// resolved by picking one: the candidate goes to human review.
	ResolveByAlias(ctx context.Context, vendorID, hint string) ([]domain.Product, error)
}

// ---------------------------------------------------------------------------
// Sourcing repositories
// ---------------------------------------------------------------------------

// SourceRepository persists sources and their check history.
type SourceRepository interface {
	GetByID(ctx context.Context, id string) (domain.Source, error)
	GetBySlug(ctx context.Context, vendorID, slug string) (domain.Source, error)
	Upsert(ctx context.Context, s domain.Source) error

	// ListDispatchable returns sources due for a check. The implementation must
	// apply the same predicate as domain.Source.Dispatchable; the PostgreSQL adapter
	// does so through the sources_dispatchable_idx partial index.
	ListDispatchable(ctx context.Context, now time.Time, limit int) ([]domain.Source, error)

	// UpdateCheckState writes back the change-detection fields after a check.
	UpdateCheckState(ctx context.Context, s domain.Source) error

	// ProductsForSource returns every product a source covers, which is what allows
	// one catalogue fetch to serve many products.
	ProductsForSource(ctx context.Context, sourceID string) ([]domain.Product, error)

	RecordCheck(ctx context.Context, c SourceCheck) error
}

// SourceCheck is one watcher run's record.
type SourceCheck struct {
	ID                    string
	SourceID              string
	StartedAt             time.Time
	FinishedAt            time.Time
	Outcome               domain.CheckOutcome
	ChangeSignal          domain.ChangeSignal
	HTTPStatus            int
	ResponseETag          string
	ResponseLastModified  string
	RedirectLocation      string
	NormalizedContentHash string
	ArtifactID            string
	BytesFetched          int64
	ErrorMessage          string
	TraceID               string
	RequestID             string
}

// Duration reports how long the check took.
func (c SourceCheck) Duration() time.Duration {
	if c.StartedAt.IsZero() || c.FinishedAt.IsZero() {
		return 0
	}
	return c.FinishedAt.Sub(c.StartedAt)
}

// ---------------------------------------------------------------------------
// Ingestion repositories
// ---------------------------------------------------------------------------

// CandidateRepository persists candidate releases and their validation results.
type CandidateRepository interface {
	GetByID(ctx context.Context, id string) (domain.CandidateRelease, error)
	// UpsertByDedupeKey inserts a candidate, or returns the existing one when the
	// (source, dedupe key) pair is already present. The bool reports whether a new
	// row was created, which is what makes re-running a check idempotent.
	UpsertByDedupeKey(ctx context.Context, c domain.CandidateRelease) (domain.CandidateRelease, bool, error)
	UpdateState(ctx context.Context, id string, state domain.CandidateState, rejectionReason string) error
	SetPublishedRelease(ctx context.Context, candidateID, releaseID string) error
	// SetResolvedProduct records the product a candidate was matched to, after the
	// match was made from its product hint.
	SetResolvedProduct(ctx context.Context, candidateID, productID string) error
	ListByState(ctx context.Context, state domain.CandidateState, limit int) ([]domain.CandidateRelease, error)
	RecordValidation(ctx context.Context, candidateID string, results []domain.GateResult) error
}

// ReleaseRepository persists published releases. It has no Update method by design:
// releases are append-only, and withdrawal and correction are new rows.
type ReleaseRepository interface {
	GetByID(ctx context.Context, id string) (domain.Release, error)
	Insert(ctx context.Context, r domain.Release, mappings []domain.ReleaseProductMapping) error

	// FindDuplicate returns the id of an already-published release with the same
	// product, normalised version and channel, or "" when there is none.
	FindDuplicate(ctx context.Context, productID, normalizedVersion, channel string) (string, error)

	// LatestForProduct returns the current latest-observed release for a product and
	// channel. It returns domain.ErrNotFound when the product has no releases.
	LatestForProduct(ctx context.Context, productID, channel string) (domain.Release, error)

	ListForProduct(ctx context.Context, productID string, limit int, cursor string) ([]domain.Release, string, error)

	// ClearLatestFlag and SetLatestFlag maintain the derived latest-observed marker
	// under the partial unique index.
	ClearLatestFlag(ctx context.Context, productID, channel string) error

	// TouchVerified refreshes last_verified_at when a check re-confirms an existing
	// release. This is the correct response to a duplicate candidate.
	TouchVerified(ctx context.Context, releaseID string, at time.Time) error

	// RefreshProductSummary rebuilds the precomputed row the public site reads.
	RefreshProductSummary(ctx context.Context, productID string) error
}

// EvidenceRepository persists provenance records.
type EvidenceRepository interface {
	Insert(ctx context.Context, e domain.Evidence) error
	GetByID(ctx context.Context, id string) (domain.Evidence, error)
}

// ReviewRepository persists items needing a human decision.
type ReviewRepository interface {
	Create(ctx context.Context, item ReviewItem) error
	ListOpen(ctx context.Context, limit int) ([]ReviewItem, error)
	Resolve(ctx context.Context, id, resolution, resolvedBy string, at time.Time) error
}

// ReviewItem is a queued human decision.
type ReviewItem struct {
	ID            string
	Kind          string
	SubjectType   string
	SubjectID     string
	VendorID      string
	ProductID     string
	Title         string
	Detail        string
	Payload       map[string]string
	PriorityScore int
	SLAClass      string
	CreatedAt     time.Time
}

// ---------------------------------------------------------------------------
// Collection ports
// ---------------------------------------------------------------------------

// FetchRequest describes a conditional retrieval.
type FetchRequest struct {
	URL                 string
	ETag                string
	LastModified        string
	ExpectedContentType string
	MaxBytes            int64
	Timeout             time.Duration
	UserAgent           string
	// RespectRobots is always true in production. It exists as a field so that
	// fixture-driven tests can fetch from a local test server that serves no
	// robots.txt without the fetcher treating that as a policy failure.
	RespectRobots bool
}

// FetchResult is the outcome of one retrieval.
type FetchResult struct {
	Outcome          domain.CheckOutcome
	ChangeSignal     domain.ChangeSignal
	StatusCode       int
	ETag             string
	LastModified     string
	ContentType      string
	Body             []byte
	BytesRead        int64
	RedirectLocation string
	// OffRegisteredHost reports that a redirect left the source's registered host,
	// which means the source may have moved and needs a human decision.
	OffRegisteredHost bool
	RetryAfter        time.Duration
	Err               error
}

// Fetcher retrieves a source's content under FirmScout's safety and politeness rules:
// SSRF guard, size limit, MIME validation, timeout, robots policy and Retry-After.
type Fetcher interface {
	Fetch(ctx context.Context, req FetchRequest) (FetchResult, error)
}

// Normalizer strips volatile content and computes a stable hash.
//
// Normalisation is what makes change detection meaningful: hashing a whole page that
// carries analytics identifiers, session tokens and rotating advertisement blocks
// reports a change on every check.
type Normalizer interface {
	// Normalize returns the canonical text of the content and the hash of the
	// section that matters. sectionSelector may be empty, in which case the whole
	// normalised document is hashed.
	Normalize(contentType string, body []byte, sectionSelector string, strip []string) (normalized []byte, hash string, err error)
}

// ArtifactStore holds fetched content, addressed by hash. Storing the same hash twice
// is a no-op, which is where most of FirmScout's storage saving comes from.
type ArtifactStore interface {
	// Put stores content and returns the artifact id. It reports created=false when
	// the hash was already present.
	Put(ctx context.Context, hash, contentType string, body []byte) (id string, created bool, err error)
	Get(ctx context.Context, id string) (io.ReadCloser, error)
	Touch(ctx context.Context, id string, at time.Time) error
}

// BlobStore holds artifact bytes, addressed by content hash. It is separate from
// ArtifactStore because the two halves have different lifetimes and costs: metadata is
// small, referenced by foreign keys, and drives retention; bytes are large, disposable,
// and belong wherever storage is cheapest -- a directory in development, S3 in AWS.
type BlobStore interface {
	// PutBlob stores content and returns the key to read it back by, reporting
	// created=false when the same hash was already held.
	PutBlob(ctx context.Context, hash string, body []byte) (key string, created bool, err error)
	GetBlob(ctx context.Context, key string) (io.ReadCloser, error)
	// Backend names this implementation for the storage_backend column:
	// "filesystem", "s3" or "inline".
	Backend() string
}

// Artifact is the input to extraction.
type Artifact struct {
	ID          string
	SourceID    string
	ContentType string
	Body        []byte
	Hash        string
	RetrievedAt time.Time
	URL         string
}

// Collector fetches and extracts structured release information from one source.
//
// The contract's most important property is that Extract is a pure function of
// (Source, Artifact): it is given no clock, no repository and no network, so the same
// artifact produces the same candidates forever. That is what makes fixture tests
// meaningful, and it is why collectors are structurally incapable of publishing.
type Collector interface {
	ID() string
	Version() string
	Vendor() string
	Supports(src domain.Source) bool
	Extract(ctx context.Context, src domain.Source, art Artifact) ([]domain.CandidateRelease, error)
}

// CollectorRegistry resolves a source to the collector that handles it.
type CollectorRegistry interface {
	For(src domain.Source) (Collector, error)
	All() []Collector
}

// ---------------------------------------------------------------------------
// Queue
// ---------------------------------------------------------------------------

// Job is a unit of background work.
type Job struct {
	ID             string
	Kind           string
	IdempotencyKey string
	Payload        map[string]string
	// TraceContext carries W3C trace context so a distributed trace survives the
	// queue hop, which is where it is normally lost.
	TraceContext map[string]string
	Attempts     int
	MaxAttempts  int
	RunAfter     time.Time
}

// Job kinds handled by the worker.
const (
	JobCheckSource       = "source.check.requested"
	JobExtractCandidates = "extraction.requested"
	JobValidateCandidate = "validation.requested"
	JobPublishRelease    = "publication.requested"
	JobRefreshSummary    = "summary.refresh.requested"
)

// JobQueue is the background work port. The MVP adapter is PostgreSQL using
// SELECT ... FOR UPDATE SKIP LOCKED; an SQS adapter satisfies the same contract on
// AWS. See ADR-0015.
type JobQueue interface {
	// Enqueue adds a job. A job whose idempotency key is already present is not
	// duplicated.
	Enqueue(ctx context.Context, j Job) error
	// Dequeue leases up to limit jobs for the given lease duration.
	Dequeue(ctx context.Context, kinds []string, limit int, lease time.Duration, workerID string) ([]Job, error)
	// Complete marks a leased job finished.
	Complete(ctx context.Context, jobID string) error
	// Fail records a failure, rescheduling with backoff or dead-lettering once
	// attempts are exhausted.
	Fail(ctx context.Context, jobID string, cause error, retryAfter time.Duration) error
}

// ---------------------------------------------------------------------------
// Entitlement and metering
// ---------------------------------------------------------------------------

// APIConsumer is an authenticated caller.
type APIConsumer struct {
	ID              string
	Name            string
	Plan            string
	Status          string
	MonthlyQuota    int64
	RateLimitPerMin int
}

// APIKeyRepository resolves and manages API keys. Keys are stored as SHA-256 hashes;
// the plaintext is displayed once at creation and never persisted.
type APIKeyRepository interface {
	// ResolveByHash returns the consumer for a key hash, or domain.ErrNotFound.
	ResolveByHash(ctx context.Context, keyHash string) (APIConsumer, string, error)
	Create(ctx context.Context, consumerID, keyHash, keyPrefix, label string) (string, error)
	Revoke(ctx context.Context, keyID, reason string, at time.Time) error
	TouchLastUsed(ctx context.Context, keyID string, at time.Time) error
}

// UsageRecord is one metered request.
type UsageRecord struct {
	ID             string
	IdempotencyKey string
	ConsumerID     string
	APIKeyID       string
	Endpoint       string
	Method         string
	StatusCode     int
	QuotaWeight    int
	DurationMS     int
	BytesOut       int64
	VendorSlug     string
	ProductSlug    string
	RateLimited    bool
	OccurredAt     time.Time
}

// UsageRecorder persists metered usage and maintains the aggregates quota enforcement
// reads. Recording is asynchronous relative to the response path so the customer's
// latency does not pay for the meter.
type UsageRecorder interface {
	Record(ctx context.Context, r UsageRecord) error
	QuotaConsumed(ctx context.Context, consumerID string, periodStart time.Time) (int64, error)
}

// ---------------------------------------------------------------------------
// Read models
// ---------------------------------------------------------------------------

// ProductSummary is the precomputed row the public site and search read, so that a
// product page is one indexed read rather than a six-way join.
type ProductSummary struct {
	ProductID            string
	VendorSlug           string
	VendorName           string
	ProductSlug          string
	ProductName          string
	FamilyName           string
	Aliases              []string
	CategorySlugs        []string
	LatestReleaseID      string
	LatestRawVersion     string
	LatestReleaseType    string
	LatestChannel        string
	LatestReleaseDate    domain.PartialDate
	RecommendedReleaseID string
	ReleaseCount         int
	LifecycleStatus      string
	HasSourceConflict    bool
	AdvisoryCount        int
	LastVerifiedAt       time.Time
	RefreshedAt          time.Time
}

// SummaryRepository reads precomputed product summaries and serves search.
type SummaryRepository interface {
	Get(ctx context.Context, productSlug string) (ProductSummary, error)
	Search(ctx context.Context, query string, limit int) ([]ProductSummary, error)
	ListByVendor(ctx context.Context, vendorSlug string, limit int, cursor string) ([]ProductSummary, string, error)
}

// ---------------------------------------------------------------------------
// Observability
// ---------------------------------------------------------------------------

// Metrics is the instrument set the application and its adapters record against.
//
// It is a port rather than a direct dependency on an OpenTelemetry SDK for two
// reasons. The dependency rule forbids internal/domain and internal/application from
// importing a telemetry library at all, and adapters must not import each other, so an
// HTTP adapter cannot reach into the telemetry adapter to find its instruments. Both
// receive this interface from the composition root instead.
//
// The metric names are fixed by docs/architecture/observability.md; the implementation
// maps them onto real instruments. Attribute keys are deliberately restricted to
// low-cardinality dimensions -- route pattern, method, status class, outcome, vendor,
// source type. A product slug, source id, URL or API key must never become a metric
// attribute: cardinality is a budget, and per-entity detail belongs in logs and traces.
type Metrics interface {
	// Counter increments a named counter by n.
	Counter(ctx context.Context, name string, n int64, attrs ...Attr)
	// Histogram records an observation, typically a duration in seconds or a size
	// in bytes.
	Histogram(ctx context.Context, name string, value float64, attrs ...Attr)
	// Gauge records the current value of something that goes up and down, such as
	// queue depth.
	Gauge(ctx context.Context, name string, value int64, attrs ...Attr)
}

// Attr is one low-cardinality metric dimension.
type Attr struct {
	Key   string
	Value string
}

// A returns an attribute, spelled short because call sites carry several.
func A(key, value string) Attr { return Attr{Key: key, Value: value} }

// NopMetrics discards every measurement. It is the default wherever telemetry has not
// been configured, so a missing collector degrades observability rather than breaking
// the request path.
type NopMetrics struct{}

func (NopMetrics) Counter(context.Context, string, int64, ...Attr)     {}
func (NopMetrics) Histogram(context.Context, string, float64, ...Attr) {}
func (NopMetrics) Gauge(context.Context, string, int64, ...Attr)       {}

// Metric names. These are the contract between the code, the Prometheus alert rules in
// infrastructure/observability, and the Grafana dashboards. Changing one here without
// changing the dashboards silently blanks a panel, so they live in one place.
const (
	MetricAPIRequestsTotal        = "firmscout_api_requests_total"
	MetricAPIRequestDuration      = "firmscout_api_request_duration_seconds"
	MetricAPIPayloadSize          = "firmscout_api_payload_size_bytes"
	MetricAPIRateLimitEvents      = "firmscout_api_rate_limit_events_total"
	MetricAPIQuotaViolations      = "firmscout_api_quota_violations_total"
	MetricAPICacheResults         = "firmscout_api_cache_results_total"
	MetricCollectorChecksTotal    = "firmscout_collector_checks_total"
	MetricCollectorExtractionFail = "firmscout_collector_extraction_failures_total"
	MetricCollectorValidationFail = "firmscout_collector_validation_failures_total"
	MetricCollectorPublications   = "firmscout_collector_publications_total"
	MetricCollectorDuplicates     = "firmscout_collector_duplicate_candidates_total"
	MetricCollectorDuration       = "firmscout_collector_execution_duration_seconds"
	MetricQueueDepth              = "firmscout_queue_depth"
	MetricQueueOldestMessageAge   = "firmscout_queue_oldest_message_age_seconds"
	MetricQueueDeadLetterTotal    = "firmscout_queue_dead_letter_total"
	MetricAIAgentExecutions       = "firmscout_ai_agent_executions_total"
	MetricAITokensTotal           = "firmscout_ai_tokens_total"
	MetricAICostUSDTotal          = "firmscout_ai_cost_usd_total"
)
