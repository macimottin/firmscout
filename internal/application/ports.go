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

// ProductRepository persists products, families, aliases and the edges between
// products.
type ProductRepository interface {
	GetByID(ctx context.Context, id string) (domain.Product, error)
	GetBySlug(ctx context.Context, slug string) (domain.Product, error)
	ListByVendor(ctx context.Context, vendorID string, limit int, cursor string) ([]domain.Product, string, error)
	Upsert(ctx context.Context, p domain.Product) error

	UpsertFamily(ctx context.Context, f domain.ProductFamily) error
	GetFamilyBySlug(ctx context.Context, vendorID, slug string) (domain.ProductFamily, error)

	ReplaceAliases(ctx context.Context, productID string, aliases []domain.ProductAlias) error
	ListAliases(ctx context.Context, productID string) ([]domain.ProductAlias, error)

	// ReplaceRelationships makes a product's outgoing relationship set exactly the
	// given list, in one transaction. Replacement rather than merge, for the same
	// reason ReplaceAliases replaces: the registry file is the source of truth, and an
	// edge deleted from that file must stop being asserted rather than linger.
	//
	// An empty rels is a supported call and the one that matters most: it is how a
	// product that has stopped declaring any edge gets the edges it used to declare
	// retracted. A caller that guards this on len(rels) > 0 has turned a replace into
	// an append, which is the bug this port's name promises it does not have.
	ReplaceRelationships(ctx context.Context, fromProductID string, rels []domain.ProductRelationship) error

	// ListRelationships returns a product's outgoing relationships, ordered by target
	// product slug so the output is stable for diffing against a registry file. It
	// exists alongside ReplaceRelationships for the same reason ListAliases exists
	// alongside ReplaceAliases: a destructive replace whose result cannot be read back
	// is a write nothing can assert on.
	ListRelationships(ctx context.Context, fromProductID string) ([]domain.ProductRelationship, error)

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

	// ListValidationResults returns the recorded verdict of every gate that ran for a
	// candidate, in gate order.
	//
	// RecordValidation has always written these rows and nothing has ever read them
	// back, which meant a reviewer could see that a candidate needed review but not
	// which gate said so. A review queue without the failing gate is a queue nobody
	// can act on.
	ListValidationResults(ctx context.Context, candidateID string) ([]domain.GateResult, error)
}

// ReleaseRepository persists published releases. It has no Update method by design:
// releases are append-only, and withdrawal and correction are new rows.
type ReleaseRepository interface {
	GetByID(ctx context.Context, id string) (domain.Release, error)
	Insert(ctx context.Context, r domain.Release, mappings []domain.ReleaseProductMapping) error

	// FindDuplicate returns the id of an already-published release with the same
	// product, normalised version and channel, or "" when there is none.
	FindDuplicate(ctx context.Context, productID, normalizedVersion, channel string) (string, error)

	// ProductRefForRelease resolves the {slug, name} of the product a release is
	// mapped to, for a caller that has only the release id and no product context of
	// its own -- GET /releases/{id} is the one release read on that path; every other
	// one (a product's history, its latest release) already knows the product it
	// asked for. A release mapped to more than one product returns the
	// lexicographically first by slug, deterministic rather than arbitrary; every
	// release in the pilot dataset maps to exactly one. domain.ErrNotFound here means
	// a release with no mapping at all, which Insert's own invariant ("a release with
	// no mapping is unreachable") should make impossible outside a corrupted database.
	ProductRefForRelease(ctx context.Context, releaseID string) (ReleaseProductRef, error)

	// LatestForProduct returns the current latest-observed release for a product and
	// channel. It returns domain.ErrNotFound when the product has no releases.
	//
	// An empty channel means ANY channel -- never "the channel whose name is the empty
	// string". The two readings are indistinguishable over HTTP, where an omitted and an
	// empty ?channel= arrive identically, and "any" subsumes the channel-less mapping
	// anyway. This sentence is here because its absence was the whole defect: the
	// PostgreSQL adapter implemented the other reading, so GET /products/{slug}/latest
	// answered 404 to exactly the call the OpenAPI document names as its default, for
	// every product whose releases all carry a channel.
	//
	// A withdrawn release is never returned, even when it holds the latest-observed
	// flag (domain.Release.Serveable). The vendor pulled it; "what version should I be
	// on?" is not answered with an image nobody should install.
	//
	// When more than one channel holds a latest-observed flag -- which is normal, the
	// flag being per channel -- the answer is the one domain.LatestComparison would
	// pick: release date first, then first-observed time, and never the version string
	// (ADR-0017). That is the same rule the product summary's headline latestRelease
	// uses, so the page and this call cannot name different releases for the same
	// product at the same moment.
	//
	// An implementation that reads the flag but not this contract is what the two
	// in-memory fakes did until they were corrected; a fake that answers a narrower
	// question than the adapter is a test suite that certifies the bug.
	LatestForProduct(ctx context.Context, productID, channel string) (domain.Release, error)

	// ListForProduct returns a page of a product's releases, newest observed first.
	ListForProduct(ctx context.Context, productID string, opts ReleaseListOptions) ([]domain.Release, string, error)

	// ClearLatestFlag and SetLatestFlag maintain the derived latest-observed marker
	// under the partial unique index.
	ClearLatestFlag(ctx context.Context, productID, channel string) error

	// TouchVerified refreshes last_verified_at when a check re-confirms an existing
	// release. This is the correct response to a duplicate candidate.
	TouchVerified(ctx context.Context, releaseID string, at time.Time) error

	// RefreshProductSummary rebuilds the precomputed row the public site reads.
	RefreshProductSummary(ctx context.Context, productID string) error
}

// SummaryRefresher rebuilds one product's precomputed summary row.
//
// It is a one-method port rather than the whole ReleaseRepository because the registry
// sync has no business with releases: it needs a product to become readable, and
// "readable" in this system means "has a product_summaries row". Narrowing the
// dependency to the single method keeps SyncRegistry's surface honest and lets a test
// substitute a counter. postgres.ReleaseRepo satisfies it structurally, with no change.
type SummaryRefresher interface {
	RefreshProductSummary(ctx context.Context, productID string) error
}

// ReleaseProductRef is the {slug, name} ProductRefForRelease resolves.
type ReleaseProductRef struct {
	Slug string
	Name string
}

// ReleaseListOptions bounds a page of release history.
//
// Since exists because history depth is a tier boundary (api.md §2) and a window has to
// be applied by the query, not by filtering a page the query already returned:
// filtering afterwards would consume a cursor for rows the caller never sees, so a
// windowed consumer would page through short, unexplained results.
type ReleaseListOptions struct {
	Limit  int
	Cursor string
	// Since windows the history returned. The zero value means the complete archive.
	//
	// A release is inside the window when the vendor's own release date could fall on
	// or after Since, and -- only when the vendor published no date at all -- when
	// FirmScout first observed it on or after Since. Windowing purely on observation
	// time would make the window meaningless in a young catalogue; windowing purely on
	// release date would silently drop every undated release.
	//
	// "Could fall" is the whole of the date rule. A reduced-precision date denotes a
	// period, not an instant, so the comparison is against the last day that period
	// could mean -- domain.PartialDate.PeriodEnd -- and not against its canonical
	// anchor. A release the vendor dated only "2025" anchors at 1 January and would
	// otherwise disappear from a window opening in September, even though the vendor
	// may well have shipped it in December; a release dated "2025-09" would disappear
	// from a window opening on 5 September for the same reason. Both are hidden
	// releases, which is the one failure this window may not produce.
	//
	// Comparing at PeriodEnd instead can return a release whose true date turns out to
	// sit just before the boundary, and that direction is the correct one to err in.
	// Showing a caller one extra release discloses a fact the tier already entitles
	// them to see at a coarser precision -- the release is in the catalogue, its date
	// is published as a month or a year, and nothing about it is gated. Hiding a
	// release they paid to see is a silent correctness failure: the page looks
	// complete, the window member says where it starts, and neither reveals that a
	// row inside it was dropped by an arithmetic convenience.
	Since time.Time
}

// EvidenceRepository persists provenance records.
type EvidenceRepository interface {
	Insert(ctx context.Context, e domain.Evidence) error
	GetByID(ctx context.Context, id string) (domain.Evidence, error)
}

// ReviewRepository persists items needing a human decision and serves the queue a human
// reads.
type ReviewRepository interface {
	Create(ctx context.Context, item ReviewItem) error
	GetByID(ctx context.Context, id string) (ReviewItem, error)
	// List returns a filtered, cursor-paginated page of the queue, ordered the way
	// review_items_queue_idx is: highest priority first, oldest first within a
	// priority.
	List(ctx context.Context, f ReviewQueueFilter) ([]ReviewItem, string, error)
	// Resolve closes an item. It refuses to re-resolve one that is already closed, so
	// two reviewers racing produce one decision and one visible failure.
	Resolve(ctx context.Context, id, resolution, resolvedBy string, at time.Time) error

	// FindOpenBySubject returns the item already waiting on a decision about a subject,
	// or domain.ErrNotFound when there is none.
	//
	// It exists because the job queue delivers at least once. Without a way to ask "is
	// there already an item for this candidate", a redelivered validation filed a second
	// copy, and a queue whose value depends on staying short accumulated one row per
	// redelivery. review_items_open_subject_idx makes the answer unique rather than
	// merely usually-unique.
	FindOpenBySubject(ctx context.Context, subjectType, subjectID string) (ReviewItem, error)

	// Retarget rewrites an open item in place so that it describes the decision as it
	// stands now: its subject, kind, title, detail, payload and priority.
	//
	// A disagreement outlives the candidate that first exposed it. When the source that
	// reported the disputed version moves on to another one, the queued item stops
	// describing anything real -- and accepting it published a version no source still
	// claimed. Rewriting the one item is preferred to closing it and filing another,
	// because the item's age and position in the queue belong to the disagreement rather
	// than to whichever candidate happened to surface it.
	//
	// It refuses an item that is already closed: a decision a human has made is not
	// something the pipeline may quietly reword.
	Retarget(ctx context.Context, item ReviewItem) error
}

// Review item subject types, written to review_items.subject_type.
//
// A queued decision is about a candidate whenever there is one a reviewer can accept.
// When the disagreement outlived every candidate that could be published -- both sources
// now report versions that are already published, so every later check is rejected as a
// duplicate -- the subject is the conflict itself. An item pointing at a rejected
// candidate would offer a reviewer an accept button that cannot work.
const (
	SubjectTypeCandidateRelease = "candidate_release"
	SubjectTypeSourceConflict   = "source_conflict"
)

// ReviewQueueFilter selects part of the queue. Every field is optional; the zero value
// means "open and in-progress items, newest priority first, default page size".
type ReviewQueueFilter struct {
	// States defaults to {"open", "in_progress"}. An item somebody started and did not
	// finish is still an open decision.
	States     []string
	Kinds      []string
	SLAClasses []string
	VendorID   string
	ProductID  string
	Limit      int
	Cursor     string
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
	State         string
	Resolution    string
	AssignedTo    string
	ResolvedBy    string
	ResolvedAt    time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Review item states, matching the review_items.state CHECK constraint.
const (
	ReviewStateOpen       = "open"
	ReviewStateInProgress = "in_progress"
	ReviewStateResolved   = "resolved"
	ReviewStateDismissed  = "dismissed"
)

// Review item payload keys written by this phase. They are constants because a reviewer
// UI reads them by name, and a typo in a map key is a field that silently never appears.
const (
	PayloadKeyConflictID       = "conflict_id"
	PayloadKeyConflictVersions = "conflict_versions"
	PayloadKeyConflictSources  = "conflict_sources"
	PayloadKeyFailingGate      = "failing_gate"
	// PayloadKeyConflictCandidates lists every candidate currently in dispute, sorted.
	// The subject names the one a reviewer would act on; this names the others, so a
	// candidate parked in human_review_required is always reachable from the queue even
	// when it is not the item's subject.
	PayloadKeyConflictCandidates = "conflict_candidates"
)

// ConflictRepository persists what each source currently reports and the disagreements
// that follow from it.
//
// It is one port rather than two because the two halves are never used apart: a
// disagreement is only meaningful against the observations that produced it, and an
// observation is only recorded in order to detect one.
type ConflictRepository interface {
	// RecordObservation writes what a source currently reports for a product and
	// channel, replacing that source's previous row. The caller decides whether the
	// new observation is the later one; see domain.LaterObservation.
	RecordObservation(ctx context.Context, obs domain.SourceObservation) error

	// ObservationsForProduct returns every source's current observation for a product
	// and channel, with the source's authority and eligibility resolved from the
	// registry at read time so a reclassified or disabled source stops counting
	// immediately.
	ObservationsForProduct(ctx context.Context, productID, channel string) ([]domain.SourceObservation, error)

	// UpsertOpenConflict opens the conflict for a product and channel, or refreshes the
	// one already open with the current participants. created reports whether this call
	// opened it, which is what stops one disagreement producing one review item per
	// check.
	UpsertOpenConflict(ctx context.Context, c domain.SourceConflict) (stored domain.SourceConflict, created bool, err error)

	// LinkReviewItem attaches the review item a human will act on to an open conflict.
	LinkReviewItem(ctx context.Context, conflictID, reviewItemID string) error

	// CloseOpenConflict resolves whatever conflict is open for a product and channel.
	// closed reports whether there was one; a product with no open conflict is the
	// normal case, not an error.
	CloseOpenConflict(ctx context.Context, productID, channel, resolution, resolvedBy string, at time.Time) (closed bool, err error)

	// ResolveConflict closes one conflict by id, for a human decision that names it.
	ResolveConflict(ctx context.Context, id, resolution, resolvedBy string, at time.Time) error

	GetConflict(ctx context.Context, id string) (domain.SourceConflict, error)
	// OpenConflictFor returns the open conflict for a product and channel, or
	// domain.ErrNotFound.
	OpenConflictFor(ctx context.Context, productID, channel string) (domain.SourceConflict, error)
	ListOpenConflicts(ctx context.Context, limit int) ([]domain.SourceConflict, error)
}

// AuditRepository persists who decided what.
//
// Blueprint §16 requires "the audit trail of who approved it" for every correction and
// publication decision. The audit_events table has existed since the initial migration
// with exactly the right columns and nothing has ever written to it.
type AuditRepository interface {
	Record(ctx context.Context, e AuditEvent) error
	ListForSubject(ctx context.Context, subjectType, subjectID string, limit int) ([]AuditEvent, error)
}

// AuditEvent is one recorded decision.
type AuditEvent struct {
	ID        string
	ActorType string
	ActorID   string
	// ActorAuthenticated reports whether the platform verified this actor's identity.
	// Every row this phase writes sets it false, because FirmScout has no login yet and
	// the actor is a string the caller asserted. Recording that honestly is the point:
	// when authentication arrives, rows written before it must not be mistaken for
	// verified ones. See ADR-0021.
	ActorAuthenticated bool
	Action             string
	SubjectType        string
	SubjectID          string
	BeforeState        map[string]string
	AfterState         map[string]string
	Reason             string
	RequestID          string
	TraceID            string
	OccurredAt         time.Time
}

// Actor types, matching the audit_events.actor_type CHECK constraint.
const (
	ActorTypeHuman       = "human"
	ActorTypeSystem      = "system"
	ActorTypeAI          = "ai"
	ActorTypeContributor = "contributor"
)

// Audit actions written by this phase.
const (
	AuditActionReviewAccepted   = "review.accepted"
	AuditActionReviewRejected   = "review.rejected"
	AuditActionConflictResolved = "conflict.resolved"
)

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

// ProductSourceRef is one entry of ProductSummary.OfficialSources: a source that has
// actually contributed a currently-mapped, non-withdrawn release to the product, not
// merely a source registered for it. Kind already went through domain.PublicSourceKind
// -- the postgres adapter applies that mapping when it scans the raw sources.source_type
// value the summary's official_sources column stores, the same function a release's
// Source.Kind goes through, so the two can never disagree about what "kind" means for
// the same underlying source. A caller renders Kind as-is; it does not map it again.
type ProductSourceRef struct {
	Slug     string
	URL      string
	Kind     string
	Official bool
}

// ProductConflictSummary is the detail behind ProductSummary.HasSourceConflict when it
// is true: which channel and versions are disputed, how many sources, and when it was
// first detected. See RefreshProductSummary for why this can surface only one open
// conflict when a product has more than one channel independently disputed at once.
type ProductConflictSummary struct {
	Channel     string
	Versions    []string
	SourceCount int
	DetectedAt  time.Time
}

// ProductRunsRef is the {slug, name, kind} of a product another product runs.
//
// It carries the relation kind rather than assuming runs_os so that a second kind, when
// one is ever evidenced, does not silently render as the first.
type ProductRunsRef struct {
	Slug string
	Name string
	Kind domain.RelationKind
}

// ProductSummary is the precomputed row the public site and search read, so that a
// product page is one indexed read rather than a six-way join.
type ProductSummary struct {
	ProductID   string
	VendorSlug  string
	VendorName  string
	ProductSlug string
	ProductName string
	FamilyName  string
	// ModelIdentifier is the vendor's published product code for a hardware model, and
	// empty for a software product. It is carried here for display only: model-number
	// search runs through the model_number alias, which is already in aliases_text and
	// therefore already in the generated search vector.
	ModelIdentifier string
	// Runs lists the products this product runs -- for a device, the operating system
	// whose releases are the ones a fleet manager is actually looking for. Never nil in
	// practice (RefreshProductSummary defaults the column to an empty JSON array), but
	// a caller should treat a nil slice as "none" rather than assume non-nil.
	Runs                 []ProductRunsRef
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
	// OfficialSources lists the product's real contributing sources -- see
	// ProductSourceRef. Never nil in practice (RefreshProductSummary defaults the
	// column to an empty JSON array), but a caller should treat a nil slice as "none"
	// rather than assume non-nil.
	OfficialSources []ProductSourceRef
	// Conflict is non-nil exactly when HasSourceConflict is true. Both are derived
	// fresh by RefreshProductSummary on every refresh -- resolving the conflict clears
	// this the same way it flips HasSourceConflict back to false, never leaving the
	// two disagreeing about whether a conflict is currently open.
	Conflict       *ProductConflictSummary
	AdvisoryCount  int
	LastVerifiedAt time.Time
	RefreshedAt    time.Time
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
