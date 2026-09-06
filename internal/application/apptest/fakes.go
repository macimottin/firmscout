// Package apptest provides in-memory implementations of the application ports.
//
// They exist so use cases can be tested without a database, a network, a queue or a
// clock. A test that needs PostgreSQL to prove that a candidate below the confidence
// threshold routes to review is testing the wrong thing.
//
// These fakes are deliberately simple and in-memory. They are not a second
// implementation of the system; where behaviour is subtle (transaction semantics,
// SKIP LOCKED contention) the PostgreSQL adapter's own integration tests cover it.
package apptest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// Clock is a controllable clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock fixed at t.
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// IDGen produces deterministic identifiers so test assertions can name them.
type IDGen struct {
	mu sync.Mutex
	n  map[string]int
}

// NewIDGen returns a deterministic identifier generator.
func NewIDGen() *IDGen { return &IDGen{n: map[string]int{}} }

// NewID returns the next identifier for a prefix, such as "rel_1".
func (g *IDGen) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n[prefix]++
	return prefix + "_" + strconv.Itoa(g.n[prefix])
}

// UnitOfWork runs fn directly. It records how many transactions were opened, which is
// how a test asserts that publication happens in exactly one.
type UnitOfWork struct {
	Transactions int
	// FailAfter, when positive, makes the Nth transaction return an error after fn
	// has run, simulating a commit failure.
	FailAfter int
}

// Within runs fn.
func (u *UnitOfWork) Within(ctx context.Context, fn func(context.Context) error) error {
	u.Transactions++
	if err := fn(ctx); err != nil {
		return err
	}
	if u.FailAfter > 0 && u.Transactions == u.FailAfter {
		return fmt.Errorf("apptest: simulated commit failure")
	}
	return nil
}

// Events collects published events.
type Events struct {
	mu        sync.Mutex
	Published []domain.Event
}

// Publish records events.
func (e *Events) Publish(ctx context.Context, events ...domain.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Published = append(e.Published, events...)
	return nil
}

// Names returns the names of every published event, in order.
func (e *Events) Names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.Published))
	for _, ev := range e.Published {
		out = append(out, string(ev.Name))
	}
	return out
}

// Count returns how many times an event was published.
func (e *Events) Count(name domain.EventName) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.Published {
		if ev.Name == name {
			n++
		}
	}
	return n
}

// Sources is an in-memory SourceRepository.
type Sources struct {
	mu       sync.Mutex
	items    map[string]domain.Source
	products map[string][]domain.Product
	Checks   []application.SourceCheck
}

// NewSources returns an empty source repository.
func NewSources() *Sources {
	return &Sources{items: map[string]domain.Source{}, products: map[string][]domain.Product{}}
}

// Add stores a source.
func (s *Sources) Add(src domain.Source) { s.mu.Lock(); defer s.mu.Unlock(); s.items[src.ID] = src }

// SetProducts declares which products a source covers.
func (s *Sources) SetProducts(sourceID string, ps []domain.Product) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.products[sourceID] = ps
}

func (s *Sources) GetByID(ctx context.Context, id string) (domain.Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.items[id]
	if !ok {
		return domain.Source{}, fmt.Errorf("source %s: %w", id, domain.ErrNotFound)
	}
	return src, nil
}

func (s *Sources) GetBySlug(ctx context.Context, vendorID, slug string) (domain.Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, src := range s.items {
		if src.VendorID == vendorID && src.Slug == slug {
			return src, nil
		}
	}
	return domain.Source{}, domain.ErrNotFound
}

func (s *Sources) Upsert(ctx context.Context, src domain.Source) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[src.ID] = src
	return nil
}

func (s *Sources) ListDispatchable(ctx context.Context, now time.Time, limit int) ([]domain.Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Source
	for _, src := range s.items {
		if src.Dispatchable(now) {
			out = append(out, src)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextCheckAt.Before(out[j].NextCheckAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Sources) UpdateCheckState(ctx context.Context, src domain.Source) error {
	return s.Upsert(ctx, src)
}

func (s *Sources) ProductsForSource(ctx context.Context, sourceID string) ([]domain.Product, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.products[sourceID], nil
}

func (s *Sources) RecordCheck(ctx context.Context, c application.SourceCheck) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Checks = append(s.Checks, c)
	return nil
}

// Products is an in-memory ProductRepository.
type Products struct {
	mu       sync.Mutex
	items    map[string]domain.Product
	families map[string]domain.ProductFamily
	aliases  map[string][]domain.ProductAlias
	// relationships holds outgoing edges by from-product id, mirroring aliases. The
	// real table's UNIQUE (from_product_id, relation_kind, to_product_id) is not
	// simulated: a caller that wants to assert on a duplicate is asserting on a
	// database constraint, and that belongs in the postgres suite.
	relationships map[string][]domain.ProductRelationship
}

// NewProducts returns an empty product repository.
func NewProducts() *Products {
	return &Products{
		items:         map[string]domain.Product{},
		families:      map[string]domain.ProductFamily{},
		aliases:       map[string][]domain.ProductAlias{},
		relationships: map[string][]domain.ProductRelationship{},
	}
}

// Add stores a product.
func (p *Products) Add(prod domain.Product) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items[prod.ID] = prod
}

// AddAlias stores an alias for a product.
func (p *Products) AddAlias(productID, alias string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, _ := domain.NewProductAlias("ali_"+alias, productID, alias, domain.AliasMarketingName)
	p.aliases[productID] = append(p.aliases[productID], a)
}

func (p *Products) GetByID(ctx context.Context, id string) (domain.Product, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.items[id]
	if !ok {
		return domain.Product{}, fmt.Errorf("product %s: %w", id, domain.ErrNotFound)
	}
	return v, nil
}

func (p *Products) GetBySlug(ctx context.Context, slug string) (domain.Product, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, v := range p.items {
		if v.Slug == slug {
			return v, nil
		}
	}
	return domain.Product{}, domain.ErrNotFound
}

func (p *Products) ListByVendor(ctx context.Context, vendorID string, limit int, cursor string) ([]domain.Product, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []domain.Product
	for _, v := range p.items {
		if v.VendorID == vendorID {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, "", nil
}

func (p *Products) Upsert(ctx context.Context, prod domain.Product) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items[prod.ID] = prod
	return nil
}

func (p *Products) UpsertFamily(ctx context.Context, f domain.ProductFamily) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.families[f.ID] = f
	return nil
}

func (p *Products) GetFamilyBySlug(ctx context.Context, vendorID, slug string) (domain.ProductFamily, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.families {
		if f.VendorID == vendorID && f.Slug == slug {
			return f, nil
		}
	}
	return domain.ProductFamily{}, domain.ErrNotFound
}

func (p *Products) ReplaceAliases(ctx context.Context, productID string, aliases []domain.ProductAlias) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.aliases[productID] = aliases
	return nil
}

func (p *Products) ListAliases(ctx context.Context, productID string) ([]domain.ProductAlias, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.aliases[productID], nil
}

// ReplaceRelationships overwrites a product's outgoing edges, as the real repository
// does: the registry file is the source of truth, so an edge removed from it must stop
// being asserted rather than survive as a leftover row.
func (p *Products) ReplaceRelationships(ctx context.Context, fromProductID string, rels []domain.ProductRelationship) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.relationships[fromProductID] = rels
	return nil
}

// ListRelationships returns a product's outgoing edges ordered by target id. The real
// repository orders by the target's slug; this fake has no join to reach one, and the
// property a caller actually depends on -- a stable order -- is preserved either way.
func (p *Products) ListRelationships(ctx context.Context, fromProductID string) ([]domain.ProductRelationship, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]domain.ProductRelationship(nil), p.relationships[fromProductID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].ToProductID < out[j].ToProductID })
	return out, nil
}

// ResolveByAlias matches on the product slug or any normalised alias. Returning more
// than one product is how the caller detects ambiguity, so this deliberately does not
// pick a winner.
func (p *Products) ResolveByAlias(ctx context.Context, vendorID, hint string) ([]domain.Product, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw := strings.TrimSpace(hint)
	normalized := domain.NormalizeAlias(hint)
	var out []domain.Product
	for id, prod := range p.items {
		if prod.VendorID != vendorID {
			continue
		}
		if prod.Slug == raw || domain.NormalizeAlias(prod.Name) == normalized {
			out = append(out, prod)
			continue
		}
		for _, a := range p.aliases[id] {
			if a.NormalizedAlias == normalized {
				out = append(out, prod)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Candidates is an in-memory CandidateRepository.
type Candidates struct {
	mu          sync.Mutex
	items       map[string]domain.CandidateRelease
	byDedupe    map[string]string
	Validations map[string][]domain.GateResult
}

// NewCandidates returns an empty candidate repository.
func NewCandidates() *Candidates {
	return &Candidates{
		items:       map[string]domain.CandidateRelease{},
		byDedupe:    map[string]string{},
		Validations: map[string][]domain.GateResult{},
	}
}

func (c *Candidates) GetByID(ctx context.Context, id string) (domain.CandidateRelease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[id]
	if !ok {
		return domain.CandidateRelease{}, fmt.Errorf("candidate %s: %w", id, domain.ErrNotFound)
	}
	return v, nil
}

func (c *Candidates) UpsertByDedupeKey(ctx context.Context, cand domain.CandidateRelease) (domain.CandidateRelease, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cand.SourceID + "\x1f" + cand.DedupeKey
	if existingID, ok := c.byDedupe[key]; ok {
		existing := c.items[existingID]
		existing.UpdatedAt = cand.UpdatedAt
		c.items[existingID] = existing
		return existing, false, nil
	}
	c.items[cand.ID] = cand
	c.byDedupe[key] = cand.ID
	return cand, true, nil
}

func (c *Candidates) UpdateState(ctx context.Context, id string, state domain.CandidateState, rejectionReason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[id]
	if !ok {
		return fmt.Errorf("candidate %s: %w", id, domain.ErrNotFound)
	}
	v.State = state
	v.RejectionReason = rejectionReason
	c.items[id] = v
	return nil
}

func (c *Candidates) SetPublishedRelease(ctx context.Context, candidateID, releaseID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[candidateID]
	if !ok {
		return fmt.Errorf("candidate %s: %w", candidateID, domain.ErrNotFound)
	}
	v.PublishedReleaseID = releaseID
	c.items[candidateID] = v
	return nil
}

func (c *Candidates) SetResolvedProduct(ctx context.Context, candidateID, productID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[candidateID]
	if !ok {
		return fmt.Errorf("candidate %s: %w", candidateID, domain.ErrNotFound)
	}
	v.ProductID = productID
	v.ProductMatchStatus = domain.MatchUnique
	c.items[candidateID] = v
	return nil
}

func (c *Candidates) ListByState(ctx context.Context, state domain.CandidateState, limit int) ([]domain.CandidateRelease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []domain.CandidateRelease
	for _, v := range c.items {
		if v.State == state {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (c *Candidates) RecordValidation(ctx context.Context, candidateID string, results []domain.GateResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Validations[candidateID] = results
	return nil
}

func (c *Candidates) ListValidationResults(ctx context.Context, candidateID string) ([]domain.GateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := c.Validations[candidateID]
	out := make([]domain.GateResult, len(stored))
	copy(out, stored)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out, nil
}

// Releases is an in-memory ReleaseRepository. It enforces the append-only rule: there
// is no method that mutates a stored release except TouchVerified.
type Releases struct {
	mu             sync.Mutex
	items          map[string]domain.Release
	mappings       []domain.ReleaseProductMapping
	SummaryRefresh []string
	Verified       map[string]time.Time
}

// NewReleases returns an empty release repository.
func NewReleases() *Releases {
	return &Releases{items: map[string]domain.Release{}, Verified: map[string]time.Time{}}
}

func (r *Releases) GetByID(ctx context.Context, id string) (domain.Release, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.items[id]
	if !ok {
		return domain.Release{}, fmt.Errorf("release %s: %w", id, domain.ErrNotFound)
	}
	return v, nil
}

func (r *Releases) Insert(ctx context.Context, rel domain.Release, mappings []domain.ReleaseProductMapping) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.items[rel.ID]; exists {
		return fmt.Errorf("release %s: %w", rel.ID, domain.ErrConflict)
	}
	r.items[rel.ID] = rel
	r.mappings = append(r.mappings, mappings...)
	return nil
}

// ProductRefForRelease returns the release's first mapped ProductID (sorted for
// determinism when there is more than one) as both Slug and Name -- this fake has no
// product registry to resolve a real name from, and nothing that constructs it needs
// one: what a caller can observe here is that the mapped id round-trips.
func (r *Releases) ProductRefForRelease(ctx context.Context, releaseID string) (application.ReleaseProductRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var productIDs []string
	for _, m := range r.mappings {
		if m.ReleaseID == releaseID {
			productIDs = append(productIDs, m.ProductID)
		}
	}
	if len(productIDs) == 0 {
		return application.ReleaseProductRef{}, fmt.Errorf("release %s: %w", releaseID, domain.ErrNotFound)
	}
	sort.Strings(productIDs)
	return application.ReleaseProductRef{Slug: productIDs[0], Name: productIDs[0]}, nil
}

func (r *Releases) FindDuplicate(ctx context.Context, productID, normalizedVersion, channel string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.mappings {
		if m.ProductID != productID || m.Applicability.Channel != channel {
			continue
		}
		rel := r.items[m.ReleaseID]
		if rel.Version.Normalized() == normalizedVersion {
			return rel.ID, nil
		}
	}
	return "", nil
}

// LatestForProduct answers the port's question, not a narrower one. See the contract on
// application.ReleaseRepository.LatestForProduct: an empty channel means ANY channel, a
// withdrawn release is never the answer, and several flagged channels are folded with
// domain.LatestComparison rather than settled by whichever mapping this map happens to
// yield first.
//
// This fake used to test `m.Applicability.Channel == channel` and return the first hit.
// That is the same mistake the PostgreSQL adapter made -- an omitted channel became a
// demand for the channel whose name is empty -- and because the fake made it too, no
// test in this package could see it. A fake that answers a narrower question than the
// adapter does not stand in for it; it certifies the bug.
func (r *Releases) LatestForProduct(ctx context.Context, productID, channel string) (domain.Release, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best domain.Release
	found := false
	for _, m := range r.mappings {
		if m.ProductID != productID || !m.IsLatestObserved {
			continue
		}
		if channel != "" && m.Applicability.Channel != channel {
			continue
		}
		rel := r.items[m.ReleaseID]
		if !rel.Serveable() {
			continue
		}
		if !found || domain.LatestComparison(rel, best) > 0 {
			best, found = rel, true
		}
	}
	if !found {
		return domain.Release{}, domain.ErrNotFound
	}
	return best, nil
}

func (r *Releases) ListForProduct(ctx context.Context, productID string, opts application.ReleaseListOptions) ([]domain.Release, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Release
	for _, m := range r.mappings {
		if m.ProductID != productID {
			continue
		}
		rel := r.items[m.ReleaseID]
		if !insideWindow(rel, opts.Since) {
			continue
		}
		out = append(out, rel)
	}
	// Ordered by release date then first observed. Never by version string.
	sort.SliceStable(out, func(i, j int) bool {
		return domain.LatestComparison(out[i], out[j]) > 0
	})
	return out, "", nil
}

// insideWindow applies the same rule the SQL adapter applies: the vendor's own release
// date decides, and first_observed_at is consulted only for a release the vendor never
// dated. Filtering on observation time alone would make the window a no-op in a young
// catalogue; filtering on release date alone would silently drop every undated release.
//
// It delegates to application.WithinHistoryWindow rather than restating the rule. This
// double previously restated it and drifted at the boundary -- it compared the period
// end against the raw instant while the adapter compared it against the instant's UTC
// day, so the two disagreed for any release dated on the boundary day itself. A double
// that reimplements the thing under test can only agree with it by luck.
func insideWindow(rel domain.Release, since time.Time) bool {
	return application.WithinHistoryWindow(rel.ReleaseDate, rel.FirstObservedAt, since)
}

func (r *Releases) ClearLatestFlag(ctx context.Context, productID, channel string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.mappings {
		if r.mappings[i].ProductID == productID && r.mappings[i].Applicability.Channel == channel {
			r.mappings[i].IsLatestObserved = false
		}
	}
	return nil
}

func (r *Releases) TouchVerified(ctx context.Context, releaseID string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Verified[releaseID] = at
	return nil
}

func (r *Releases) RefreshProductSummary(ctx context.Context, productID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.SummaryRefresh = append(r.SummaryRefresh, productID)
	return nil
}

// Count returns how many releases were published.
func (r *Releases) Count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.items) }

// Mappings returns a copy of the stored mappings.
func (r *Releases) Mappings() []domain.ReleaseProductMapping {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.ReleaseProductMapping, len(r.mappings))
	copy(out, r.mappings)
	return out
}

// EvidenceStore is an in-memory EvidenceRepository.
type EvidenceStore struct {
	mu    sync.Mutex
	items map[string]domain.Evidence
}

// NewEvidenceStore returns an empty evidence repository.
func NewEvidenceStore() *EvidenceStore {
	return &EvidenceStore{items: map[string]domain.Evidence{}}
}

func (e *EvidenceStore) Insert(ctx context.Context, ev domain.Evidence) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.items[ev.ID] = ev
	return nil
}

func (e *EvidenceStore) GetByID(ctx context.Context, id string) (domain.Evidence, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.items[id]
	if !ok {
		return domain.Evidence{}, fmt.Errorf("evidence %s: %w", id, domain.ErrNotFound)
	}
	return v, nil
}

// Count returns how many evidence rows were stored.
func (e *EvidenceStore) Count() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.items) }

// Reviews is an in-memory ReviewRepository.
//
// Create enforces the same uniqueness review_items_open_subject_idx does -- at most one
// open or in-progress item per subject -- for the reason Conflicts keys observations the
// way source_observations_current_idx does: a fake that allowed the second row would let
// a use-case test pass while the real adapter rejected the same call.
type Reviews struct {
	mu    sync.Mutex
	Items []application.ReviewItem
}

func (r *Reviews) Create(ctx context.Context, item application.ReviewItem) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if item.State == "" {
		item.State = application.ReviewStateOpen
	}
	for _, existing := range r.Items {
		if !openReviewState(existing.State) {
			continue
		}
		if existing.SubjectType == item.SubjectType && existing.SubjectID == item.SubjectID {
			return fmt.Errorf("review item for %s %s is already open: %w",
				item.SubjectType, item.SubjectID, domain.ErrConflict)
		}
	}
	r.Items = append(r.Items, item)
	return nil
}

// openReviewState reports whether a state counts as "still in the queue". It is the same
// pair review_items_queue_idx and review_items_open_subject_idx are partial on.
func openReviewState(state string) bool {
	return state == application.ReviewStateOpen || state == application.ReviewStateInProgress
}

// FindOpenBySubject returns the open item for a subject, or domain.ErrNotFound.
func (r *Reviews) FindOpenBySubject(ctx context.Context, subjectType, subjectID string) (application.ReviewItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.Items {
		if openReviewState(item.State) && item.SubjectType == subjectType && item.SubjectID == subjectID {
			return item, nil
		}
	}
	return application.ReviewItem{}, fmt.Errorf("open review item for %s %s: %w",
		subjectType, subjectID, domain.ErrNotFound)
}

// Retarget rewrites an open item in place, leaving created_at and assigned_to alone the
// way the real UPDATE does.
func (r *Reviews) Retarget(ctx context.Context, item application.ReviewItem) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.Items {
		if r.Items[i].ID != item.ID {
			continue
		}
		if !openReviewState(r.Items[i].State) {
			return fmt.Errorf("review item %s is not open: %w", item.ID, domain.ErrNotFound)
		}
		stored := &r.Items[i]
		stored.Kind = item.Kind
		stored.SubjectType = item.SubjectType
		stored.SubjectID = item.SubjectID
		stored.VendorID = item.VendorID
		stored.ProductID = item.ProductID
		stored.Title = item.Title
		stored.Detail = item.Detail
		stored.Payload = item.Payload
		stored.PriorityScore = item.PriorityScore
		stored.SLAClass = item.SLAClass
		return nil
	}
	return fmt.Errorf("review item %s: %w", item.ID, domain.ErrNotFound)
}

// Add seeds an item without going through Create, for tests that need a queue that
// already has history.
func (r *Reviews) Add(item application.ReviewItem) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if item.State == "" {
		item.State = application.ReviewStateOpen
	}
	r.Items = append(r.Items, item)
}

func (r *Reviews) GetByID(ctx context.Context, id string) (application.ReviewItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.Items {
		if item.ID == id {
			return item, nil
		}
	}
	return application.ReviewItem{}, fmt.Errorf("review item %s: %w", id, domain.ErrNotFound)
}

// List applies the filter and orders the way review_items_queue_idx does: highest
// priority first, oldest first within a priority. It does not paginate -- a fake that
// reimplemented keyset cursors would be testing the fake.
func (r *Reviews) List(ctx context.Context, f application.ReviewQueueFilter) ([]application.ReviewItem, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	states := f.States
	if len(states) == 0 {
		states = []string{application.ReviewStateOpen, application.ReviewStateInProgress}
	}
	var out []application.ReviewItem
	for _, item := range r.Items {
		if !containsString(states, item.State) {
			continue
		}
		if len(f.Kinds) > 0 && !containsString(f.Kinds, item.Kind) {
			continue
		}
		if len(f.SLAClasses) > 0 && !containsString(f.SLAClasses, item.SLAClass) {
			continue
		}
		if f.VendorID != "" && item.VendorID != f.VendorID {
			continue
		}
		if f.ProductID != "" && item.ProductID != f.ProductID {
			continue
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PriorityScore != out[j].PriorityScore {
			return out[i].PriorityScore > out[j].PriorityScore
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, "", nil
}

// Resolve refuses an item that is already closed, which is the behaviour the real
// adapter gets from its WHERE state IN ('open','in_progress') clause. Two reviewers
// racing must produce one decision and one visible failure, not two decisions.
func (r *Reviews) Resolve(ctx context.Context, id, resolution, resolvedBy string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.Items {
		if r.Items[i].ID != id {
			continue
		}
		if r.Items[i].State != application.ReviewStateOpen && r.Items[i].State != application.ReviewStateInProgress {
			return fmt.Errorf("review item %s is not open: %w", id, domain.ErrNotFound)
		}
		r.Items[i].State = application.ReviewStateResolved
		r.Items[i].Resolution = resolution
		r.Items[i].ResolvedBy = resolvedBy
		r.Items[i].ResolvedAt = at
		r.Items[i].UpdatedAt = at
		return nil
	}
	return fmt.Errorf("review item %s: %w", id, domain.ErrNotFound)
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// Conflicts is an in-memory ConflictRepository.
//
// It keeps observations keyed exactly the way source_observations_current_idx does --
// one row per source, product and channel -- because that uniqueness is the property
// that makes the table a projection rather than a second append-log, and a fake that
// allowed two current rows would hide the bug the index exists to prevent.
type Conflicts struct {
	mu           sync.Mutex
	observations map[string]domain.SourceObservation
	conflicts    map[string]domain.SourceConflict
	order        []string

	// LinkErr, when set, makes LinkReviewItem fail. It exists because the failure that
	// motivated wrapping validation in a unit of work is precisely this one: the review
	// item was committed, the link was not, the worker retried, and a second item was
	// filed for the same disagreement.
	LinkErr error
}

// NewConflicts returns an empty conflict repository.
func NewConflicts() *Conflicts {
	return &Conflicts{
		observations: map[string]domain.SourceObservation{},
		conflicts:    map[string]domain.SourceConflict{},
	}
}

func observationKey(sourceID, productID, channel string) string {
	return sourceID + "\x1f" + productID + "\x1f" + channel
}

func (c *Conflicts) RecordObservation(ctx context.Context, obs domain.SourceObservation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := observationKey(obs.SourceID, obs.ProductID, obs.Channel)
	if existing, ok := c.observations[key]; ok {
		// first_observed_at is left alone by the real ON CONFLICT DO UPDATE, because
		// it records when the source first made this claim, not when it last repeated
		// it.
		obs.FirstObservedAt = existing.FirstObservedAt
	}
	c.observations[key] = obs
	return nil
}

// SetObservation seeds what a source claims, including the authority and eligibility
// fields. The fake stores them exactly as given and never re-derives them: in
// production they come from a SQL join against the source registry, and a fake that
// computed them would be testing the fake.
func (c *Conflicts) SetObservation(obs domain.SourceObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observations[observationKey(obs.SourceID, obs.ProductID, obs.Channel)] = obs
}

func (c *Conflicts) ObservationsForProduct(ctx context.Context, productID, channel string) ([]domain.SourceObservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []domain.SourceObservation
	for _, obs := range c.observations {
		if obs.ProductID == productID && obs.Channel == channel {
			out = append(out, obs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceID < out[j].SourceID })
	return out, nil
}

func (c *Conflicts) UpsertOpenConflict(ctx context.Context, sc domain.SourceConflict) (domain.SourceConflict, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range c.order {
		existing := c.conflicts[id]
		if existing.State != domain.ConflictOpen || existing.ProductID != sc.ProductID || existing.Channel != sc.Channel {
			continue
		}
		existing.Versions = sc.Versions
		existing.SourceIDs = sc.SourceIDs
		existing.AuthorityRank = sc.AuthorityRank
		existing.LastSeenAt = sc.LastSeenAt
		c.conflicts[id] = existing
		return existing, false, nil
	}
	c.conflicts[sc.ID] = sc
	c.order = append(c.order, sc.ID)
	return sc, true, nil
}

func (c *Conflicts) LinkReviewItem(ctx context.Context, conflictID, reviewItemID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.LinkErr != nil {
		return c.LinkErr
	}
	stored, ok := c.conflicts[conflictID]
	if !ok {
		return fmt.Errorf("conflict %s: %w", conflictID, domain.ErrNotFound)
	}
	stored.ReviewItemID = reviewItemID
	c.conflicts[conflictID] = stored
	return nil
}

func (c *Conflicts) CloseOpenConflict(ctx context.Context, productID, channel, resolution, resolvedBy string, at time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range c.order {
		stored := c.conflicts[id]
		if stored.State != domain.ConflictOpen || stored.ProductID != productID || stored.Channel != channel {
			continue
		}
		resolved, err := stored.Resolve(at, resolvedBy, resolution)
		if err != nil {
			return false, err
		}
		c.conflicts[id] = resolved
		return true, nil
	}
	return false, nil
}

func (c *Conflicts) ResolveConflict(ctx context.Context, id, resolution, resolvedBy string, at time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	stored, ok := c.conflicts[id]
	if !ok {
		return fmt.Errorf("conflict %s: %w", id, domain.ErrNotFound)
	}
	resolved, err := stored.Resolve(at, resolvedBy, resolution)
	if err != nil {
		return err
	}
	c.conflicts[id] = resolved
	return nil
}

func (c *Conflicts) GetConflict(ctx context.Context, id string) (domain.SourceConflict, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stored, ok := c.conflicts[id]
	if !ok {
		return domain.SourceConflict{}, fmt.Errorf("conflict %s: %w", id, domain.ErrNotFound)
	}
	return stored, nil
}

func (c *Conflicts) OpenConflictFor(ctx context.Context, productID, channel string) (domain.SourceConflict, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range c.order {
		stored := c.conflicts[id]
		if stored.State == domain.ConflictOpen && stored.ProductID == productID && stored.Channel == channel {
			return stored, nil
		}
	}
	return domain.SourceConflict{}, domain.ErrNotFound
}

func (c *Conflicts) ListOpenConflicts(ctx context.Context, limit int) ([]domain.SourceConflict, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []domain.SourceConflict
	for _, id := range c.order {
		if stored := c.conflicts[id]; stored.State == domain.ConflictOpen {
			out = append(out, stored)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// OpenConflicts returns the open conflicts in the order they were opened.
func (c *Conflicts) OpenConflicts() []domain.SourceConflict {
	out, _ := c.ListOpenConflicts(context.Background(), 0)
	return out
}

// Audit is an in-memory AuditRepository.
type Audit struct {
	mu     sync.Mutex
	events []application.AuditEvent
}

// NewAudit returns an empty audit repository.
func NewAudit() *Audit { return &Audit{} }

func (a *Audit) Record(ctx context.Context, e application.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
	return nil
}

func (a *Audit) ListForSubject(ctx context.Context, subjectType, subjectID string, limit int) ([]application.AuditEvent, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []application.AuditEvent
	for _, e := range a.events {
		if e.SubjectType == subjectType && e.SubjectID == subjectID {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Events returns every recorded audit row, in the order it was written.
func (a *Audit) Events() []application.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]application.AuditEvent, len(a.events))
	copy(out, a.events)
	return out
}

// Queue is an in-memory JobQueue that honours idempotency keys.
type Queue struct {
	mu       sync.Mutex
	jobs     []application.Job
	seenKeys map[string]bool
	Failures []string
}

// NewQueue returns an empty queue.
func NewQueue() *Queue { return &Queue{seenKeys: map[string]bool{}} }

func (q *Queue) Enqueue(ctx context.Context, j application.Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if j.IdempotencyKey != "" && q.seenKeys[j.IdempotencyKey] {
		return nil // already enqueued; not an error
	}
	q.seenKeys[j.IdempotencyKey] = true
	q.jobs = append(q.jobs, j)
	return nil
}

func (q *Queue) Dequeue(ctx context.Context, kinds []string, limit int, lease time.Duration, workerID string) ([]application.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	want := map[string]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	var out, rest []application.Job
	for _, j := range q.jobs {
		if len(out) < limit && (len(want) == 0 || want[j.Kind]) {
			out = append(out, j)
			continue
		}
		rest = append(rest, j)
	}
	q.jobs = rest
	return out, nil
}

func (q *Queue) Complete(ctx context.Context, jobID string) error { return nil }

func (q *Queue) Fail(ctx context.Context, jobID string, cause error, retryAfter time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.Failures = append(q.Failures, jobID)
	return nil
}

// Jobs returns the pending jobs.
func (q *Queue) Jobs() []application.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]application.Job, len(q.jobs))
	copy(out, q.jobs)
	return out
}

// JobsOfKind returns pending jobs of one kind.
func (q *Queue) JobsOfKind(kind string) []application.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []application.Job
	for _, j := range q.jobs {
		if j.Kind == kind {
			out = append(out, j)
		}
	}
	return out
}

// Artifacts is an in-memory content-addressed ArtifactStore.
type Artifacts struct {
	mu     sync.Mutex
	byHash map[string]string
	bodies map[string][]byte
	Puts   int
}

// NewArtifacts returns an empty artifact store.
func NewArtifacts() *Artifacts {
	return &Artifacts{byHash: map[string]string{}, bodies: map[string][]byte{}}
}

func (a *Artifacts) Put(ctx context.Context, hash, contentType string, body []byte) (string, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Puts++
	if id, ok := a.byHash[hash]; ok {
		return id, false, nil
	}
	id := "art_" + hash[:min(8, len(hash))]
	a.byHash[hash] = id
	a.bodies[id] = append([]byte(nil), body...)
	return id, true, nil
}

func (a *Artifacts) Get(ctx context.Context, id string) (io.ReadCloser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.bodies[id]
	if !ok {
		return nil, fmt.Errorf("artifact %s: %w", id, domain.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (a *Artifacts) Touch(ctx context.Context, id string, at time.Time) error { return nil }

// StoredCount returns how many distinct artifacts are held.
func (a *Artifacts) StoredCount() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.bodies) }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
