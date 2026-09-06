package application_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/application/apptest"
	"github.com/macimottin/firmscout/internal/domain"
)

// publish runs the publication use case for one candidate, which the conflict tests need
// in order to reach the state where both sources' current claims are already published --
// the state in which gate 4 short-circuits every later check.
func (f *fixture) publish(t *testing.T, candidateID string) application.PublishResult {
	t.Helper()
	res, err := application.NewPublishRelease(f.ingestDeps()).Execute(context.Background(), candidateID)
	if err != nil {
		t.Fatalf("publish %s: %v", candidateID, err)
	}
	if !res.Published {
		t.Fatalf("publish %s did not publish: %s", candidateID, res.Reason)
	}
	return res
}

// openItems returns the items a human would actually see.
func (f *fixture) openItems() []application.ReviewItem {
	var out []application.ReviewItem
	for _, item := range f.reviews.Items {
		if item.State == application.ReviewStateOpen || item.State == application.ReviewStateInProgress {
			out = append(out, item)
		}
	}
	return out
}

// assertQueueCoversPendingCandidates is the invariant every fix in this file exists to
// protect: a candidate parked in human_review_required is waiting for a decision, and a
// decision nobody can reach is not a decision. Either an open item names it, or an open
// item lists it among the candidates in dispute.
func assertQueueCoversPendingCandidates(t *testing.T, f *fixture) {
	t.Helper()
	pending, err := f.candidates.ListByState(context.Background(), domain.CandidateHumanReviewRequired, 0)
	if err != nil {
		t.Fatalf("list candidates awaiting review: %v", err)
	}
	for _, c := range pending {
		covered := false
		for _, item := range f.openItems() {
			if item.SubjectID == c.ID {
				covered = true
				break
			}
			for _, id := range strings.Split(item.Payload[application.PayloadKeyConflictCandidates], ",") {
				if id == c.ID {
					covered = true
					break
				}
			}
		}
		if !covered {
			t.Errorf("candidate %s (%s) is stranded in human_review_required: no open review item references it",
				c.ID, c.Version.Raw())
		}
	}
}

// ---------------------------------------------------------------------------
// D2: the queue item has to describe the disagreement as it stands now
// ---------------------------------------------------------------------------

// The revert detector for the reuse guard's missing subject check.
//
// Reverting it makes the item go on naming the candidate that opened the conflict, so
// accepting it publishes a version no source claims any more -- here 7.24.1, which is
// older than both live claims -- and leaves the candidate that is actually in dispute
// parked in human_review_required with nothing in the queue pointing at it.
func TestRefreshedConflictRetargetsItsReviewItem(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addSecondSource(t)
	ctx := context.Background()

	first := f.validateFrom(t, sourceID, "h1", "7.24.3", "2026-09-02")
	if first.Decision != domain.GatePassed {
		t.Fatalf("first source decision = %q, want passed (%s)", first.Decision, first.Reason)
	}

	second := f.validateFrom(t, secondSourceID, "h2", "7.24.1", "2026-09-01")
	if second.ReviewItemID == "" || second.ConflictID == "" {
		t.Fatalf("the disagreement was not queued: %+v", second)
	}

	// The next day the second source moves on. The conflict is the same disagreement --
	// same product, same channel, still unresolved -- so it is refreshed rather than
	// reopened, and the one item covering it has to follow.
	third := f.validateFrom(t, secondSourceID, "h3", "7.24.5", "2026-09-03")
	if third.Decision != domain.GateReviewRequired {
		t.Fatalf("third decision = %q, want review_required (%s)", third.Decision, third.Reason)
	}
	if third.ConflictID != second.ConflictID {
		t.Fatalf("conflict id = %q, want the already-open %q", third.ConflictID, second.ConflictID)
	}
	if open := f.conflicts.OpenConflicts(); len(open) != 1 {
		t.Fatalf("%d open conflicts, want exactly 1", len(open))
	}
	if items := f.openItems(); len(items) != 1 {
		t.Fatalf("review queue has %d open items, want exactly 1 for one disagreement", len(items))
	}

	item := f.openItems()[0]
	if item.SubjectID != third.CandidateID {
		t.Errorf("queue item names candidate %q; the candidate in dispute is %q",
			item.SubjectID, third.CandidateID)
	}
	if !strings.Contains(item.Title, "7.24.5") {
		t.Errorf("queue item title = %q, want it to name the version now in dispute", item.Title)
	}
	if got := item.Payload[application.PayloadKeyConflictVersions]; got != "7.24.3,7.24.5" {
		t.Errorf("payload versions = %q, want the versions the sources report today", got)
	}
	if got := item.Payload[application.PayloadKeyConflictID]; got != third.ConflictID {
		t.Errorf("payload conflict id = %q, want %q", got, third.ConflictID)
	}

	// The superseded claim is not left parked: its own source stopped reporting it, so
	// there is nothing about it for a human to decide.
	superseded, err := f.candidates.GetByID(ctx, second.CandidateID)
	if err != nil {
		t.Fatalf("load the superseded candidate: %v", err)
	}
	if superseded.State == domain.CandidateHumanReviewRequired {
		t.Errorf("candidate %s (7.24.1) is still awaiting review with nothing in the queue naming it",
			second.CandidateID)
	}
	assertQueueCoversPendingCandidates(t, f)

	// The decision a human makes must be about a version somebody still claims.
	res, err := application.NewDecideReviewItem(f.reviewDeps()).Accept(ctx, application.ReviewDecisionInput{
		ItemID: item.ID,
		Actor:  "alex",
		Reason: "the release notes page is the authoritative one",
	})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	published, err := f.releases.GetByID(ctx, res.ReleaseID)
	if err != nil {
		t.Fatalf("load the published release: %v", err)
	}
	if published.Version.Raw() != "7.24.5" {
		t.Fatalf("accepting the queue item published %q; no source claims that version any more",
			published.Version.Raw())
	}
	latest, err := f.releases.LatestForProduct(ctx, productID, "stable")
	if err != nil {
		t.Fatalf("latest for product: %v", err)
	}
	if latest.Version.Raw() != "7.24.5" {
		t.Errorf("latest observed = %q, want 7.24.5", latest.Version.Raw())
	}
}

// A subject may hold only one open item -- review_items_open_subject_idx says so -- and
// the routing code has to respect that even when the conflict's own item has gone stale.
// Retargeting onto a subject that already has an item would attempt exactly the row the
// index forbids, turning the validation into a job that can never succeed; the existing
// item is used instead.
func TestRetargetYieldsToAnItemTheSubjectAlreadyHas(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addSecondSource(t)
	ctx := context.Background()

	f.validateFrom(t, sourceID, "h1", "7.24.3", "2026-09-02")
	second := f.validateFrom(t, secondSourceID, "h2", "7.24.1", "2026-09-01")
	linked := second.ReviewItemID
	if linked == "" {
		t.Fatal("the fixture needs a conflict with a queue item")
	}

	// Extract the second source's next claim without validating it, so the test can seed
	// an item against the candidate the retarget would otherwise move onto. Seeding is
	// how this state is reached at all: it is what an earlier review of the same
	// candidate, filed before the conflict existed, leaves behind.
	f.collector = &apptest.Collector{CollectorID: "c", Ver: "1",
		Candidates: []domain.CandidateRelease{f.candidateFor(t, "7.24.5", "2026-09-03")}}
	deps := f.ingestDeps()
	artID, _, err := f.artifacts.Put(ctx, "h3", "text/html", []byte("7.24.5"))
	if err != nil {
		t.Fatalf("store artifact: %v", err)
	}
	ex, err := application.NewExtractCandidates(deps).Execute(ctx, secondSourceID, artID)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	nextCandidate := ex.CandidateIDs[0]

	f.reviews.Add(application.ReviewItem{
		ID:          "rev_seeded",
		Kind:        "candidate_low_confidence",
		SubjectType: application.SubjectTypeCandidateRelease,
		SubjectID:   nextCandidate,
		VendorID:    vendorID,
		ProductID:   productID,
		Title:       "Candidate 7.24.5 needs review",
		State:       application.ReviewStateOpen,
		CreatedAt:   f.clock.Now(),
	})

	res, err := application.NewValidateCandidate(deps).Execute(ctx, nextCandidate)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if res.ReviewItemID != "rev_seeded" {
		t.Fatalf("routing reported item %q, want the one the subject already had (rev_seeded); "+
			"repointing %s onto an occupied subject is the write the unique index forbids",
			res.ReviewItemID, linked)
	}

	seeded, err := f.reviews.GetByID(ctx, "rev_seeded")
	if err != nil {
		t.Fatalf("reload the seeded item: %v", err)
	}
	if seeded.Payload[application.PayloadKeyConflictID] != res.ConflictID {
		t.Errorf("the reused item carries conflict id %q, want %q",
			seeded.Payload[application.PayloadKeyConflictID], res.ConflictID)
	}
	conflict, err := f.conflicts.GetConflict(ctx, res.ConflictID)
	if err != nil {
		t.Fatalf("load conflict: %v", err)
	}
	if conflict.ReviewItemID != "rev_seeded" {
		t.Errorf("conflict links item %q, want rev_seeded", conflict.ReviewItemID)
	}
	assertQueueCoversPendingCandidates(t, f)
}

// ---------------------------------------------------------------------------
// D3: a disagreement reaches a human even when the candidate that found it did not
// ---------------------------------------------------------------------------

// The revert detector for the rejected branch's missing routing.
//
// Once both sources' current claims are already-published versions, every later check
// hits gate 4, short circuits with decision=rejected, and the old code opened the conflict
// and filed nothing. The state was permanent: no queue item, no route, no command, and
// ADR-0020's stated purpose unmet.
func TestDuplicateRejectionStillQueuesTheDisagreement(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addSecondSource(t)
	ctx := context.Background()

	// Both of the first source's claims become published releases.
	f.publish(t, f.validateFrom(t, sourceID, "h1", "7.24.1", "2026-09-01").CandidateID)
	f.publish(t, f.validateFrom(t, sourceID, "h2", "7.24.9", "2026-09-03").CandidateID)

	// The second source still reports the older one. That is a real disagreement about
	// what is current, and it is also an exact duplicate of a published release, so gate
	// 4 rejects the candidate before gate 10 ever runs.
	second := f.validateFrom(t, secondSourceID, "h3", "7.24.1", "2026-09-01")
	if second.Decision != domain.GateRejected {
		t.Fatalf("second source decision = %q, want rejected as a duplicate (%s)", second.Decision, second.Reason)
	}
	if second.ConflictID == "" {
		t.Fatal("two sources report different current versions and no conflict was recorded")
	}
	if second.ReviewItemID == "" {
		t.Fatal("the conflict was opened with no review item: the disagreement reaches nobody")
	}

	items := f.openItems()
	if len(items) != 1 {
		t.Fatalf("review queue has %d open items, want exactly 1", len(items))
	}
	item := items[0]
	if item.SubjectType != application.SubjectTypeSourceConflict || item.SubjectID != second.ConflictID {
		t.Errorf("queue item subject = %s/%s, want the conflict %s: there is no candidate left to accept",
			item.SubjectType, item.SubjectID, second.ConflictID)
	}
	if item.Kind != "multi_source_conflict" {
		t.Errorf("queue item kind = %q, want multi_source_conflict", item.Kind)
	}
	if got := item.Payload[application.PayloadKeyConflictVersions]; got != "7.24.1,7.24.9" {
		t.Errorf("payload versions = %q, want both disputed versions", got)
	}

	// Three further scheduler cycles. The conflict stays open and the queue stays at one
	// item; neither source can produce anything but a duplicate rejection now.
	for i := 0; i < 3; i++ {
		f.validateFrom(t, sourceID, "cycle-a", "7.24.9", "2026-09-03")
		f.validateFrom(t, secondSourceID, "cycle-b", "7.24.1", "2026-09-01")
	}
	if items := f.openItems(); len(items) != 1 || items[0].ID != item.ID {
		t.Fatalf("after three cycles the queue holds %d items, want the same single item %s", len(items), item.ID)
	}
	if open := f.conflicts.OpenConflicts(); len(open) != 1 {
		t.Fatalf("%d open conflicts after three cycles, want 1", len(open))
	}

	// And the conflict is reachable through a use case rather than only through the
	// table, which is what "wired to no route and no command" meant.
	entries, err := application.NewListOpenConflicts(f.conflictQueryDeps()).Execute(ctx, 0)
	if err != nil {
		t.Fatalf("ListOpenConflicts: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListOpenConflicts returned %d entries, want 1", len(entries))
	}
	entry := entries[0]
	if !entry.Queued || entry.ReviewItem.ID != item.ID {
		t.Errorf("open conflict reports queued=%v item=%q, want the queue item %s",
			entry.Queued, entry.ReviewItem.ID, item.ID)
	}
	if entry.ProductSlug != "mikrotik-routeros" {
		t.Errorf("product slug = %q, want mikrotik-routeros", entry.ProductSlug)
	}
	if len(entry.Observations) != 2 {
		t.Errorf("open conflict carries %d source observations, want both sides of the disagreement",
			len(entry.Observations))
	}

	// A human resolves the disagreement itself, not a candidate: this item's subject
	// is the conflict (application.SubjectTypeSourceConflict), so DecideReviewItem's
	// candidate-publish branch never runs, and nothing else on this path used to
	// refresh product_summaries. ProductSummary.Conflict's own doc comment promises
	// the detail is "derived fresh... on every refresh", which was false for exactly
	// this path: resolving here left HasSourceConflict and its channel/versions/
	// detected-at showing the now-resolved disagreement as if it were still open,
	// indefinitely, because nothing else was ever going to touch this product's
	// summary again on its own.
	// f.publish (called twice above, once per genuinely published version) already
	// refreshed this product's summary on its own path, so the count before this
	// decision is nonzero -- asserting the LAST entry is productID would prove
	// nothing, since it already was. What resolving the conflict must add is one MORE
	// refresh that would not otherwise happen.
	before := len(f.releases.SummaryRefresh)
	if _, err := application.NewDecideReviewItem(f.reviewDeps()).Reject(ctx, application.ReviewDecisionInput{
		ItemID: item.ID,
		Actor:  "alex",
		Reason: "7.24.9 is the current release; the older claim is stale",
	}); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	after := f.releases.SummaryRefresh
	if len(after) != before+1 || after[len(after)-1] != productID {
		t.Fatalf("resolving a source_conflict review item did not refresh product %s's summary once more; "+
			"refreshes before=%d after=%v", productID, before, after)
	}
}

// An ineligible source's claim is not evidence, so it must not manufacture work for a
// human. Without this the compliance gate would be the only thing standing between a
// prohibited source and the review queue, and gate 3's rejection does not stop the
// conflict projection.
func TestIneligibleSourceNeitherOpensNorQueuesAConflict(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	blocked := f.addSecondSource(t)
	blocked.TermsReviewStatus = domain.TermsProhibited
	f.sources.Add(blocked)

	f.validateFrom(t, sourceID, "h1", "7.24.3", "2026-09-02")
	res := f.validateFrom(t, secondSourceID, "h2", "7.24.1", "2026-09-01")

	if res.Decision != domain.GateRejected {
		t.Fatalf("decision = %q, want rejected: the source is not eligible (%s)", res.Decision, res.Reason)
	}
	if res.ConflictID != "" {
		t.Errorf("a source FirmScout may not collect from opened conflict %s", res.ConflictID)
	}
	if items := f.openItems(); len(items) != 0 {
		t.Errorf("a prohibited source filed %d review items", len(items))
	}
}

// ---------------------------------------------------------------------------
// D4: at-least-once delivery must not mean one queue item per delivery
// ---------------------------------------------------------------------------

// The revert detector for validation's idempotency. Reviews.Create mints a fresh id on
// every call, so without the "is there already an open item for this subject" lookup each
// redelivery of the same job filed another copy.
func TestValidateCandidateIsIdempotentAcrossRedelivery(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	candidate := f.candidateFor(t, "7.24.3", "2026-09-02")
	candidate.Confidence = 0.40 // below the automatic-publication threshold: gate 8 queues it
	f.collector = &apptest.Collector{CollectorID: "c", Ver: "1", Candidates: []domain.CandidateRelease{candidate}}
	deps := f.ingestDeps()
	artID, _, err := f.artifacts.Put(ctx, "h", "text/html", []byte(changedBody))
	if err != nil {
		t.Fatalf("store artifact: %v", err)
	}
	ex, err := application.NewExtractCandidates(deps).Execute(ctx, sourceID, artID)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	candidateID := ex.CandidateIDs[0]

	var itemIDs []string
	for delivery := 1; delivery <= 3; delivery++ {
		before := f.uow.Transactions
		res, err := application.NewValidateCandidate(deps).Execute(ctx, candidateID)
		if err != nil {
			t.Fatalf("delivery %d: %v", delivery, err)
		}
		if opened := f.uow.Transactions - before; opened != 1 {
			t.Errorf("delivery %d opened %d transactions, want exactly 1 unit of work", delivery, opened)
		}
		itemIDs = append(itemIDs, res.ReviewItemID)
	}

	if len(f.reviews.Items) != 1 {
		t.Fatalf("three deliveries filed %d review items, want 1", len(f.reviews.Items))
	}
	if itemIDs[0] != itemIDs[1] || itemIDs[1] != itemIDs[2] {
		t.Errorf("deliveries reported items %v, want the same item each time", itemIDs)
	}
	if f.reviews.Items[0].SubjectID != candidateID {
		t.Errorf("item subject = %q, want %q", f.reviews.Items[0].SubjectID, candidateID)
	}
	// One request, not three: the candidate moved into human_review_required once, and an
	// event stream that announced it on every redelivery would describe work that never
	// happened.
	if n := f.events.Count(domain.EventHumanReviewRequested); n != 1 {
		t.Errorf("published %d HumanReviewRequested events across three deliveries, want 1", n)
	}
	assertQueueCoversPendingCandidates(t, f)
}

// The exact failure the unit of work was added for: the review item is written, linking it
// to the conflict fails, the worker retries. Before the guard the retry filed a second
// item for the same disagreement -- the outcome the reuse rule exists to prevent.
//
// The in-memory unit of work runs its function without rolling anything back, so what
// this proves is the application-side half: the retry finds the item that already exists
// instead of minting another. That real transactions actually roll the item back is
// proved against PostgreSQL in TestReviewItemCreationRollsBackWithItsUnitOfWork.
func TestValidationRetryAfterALinkFailureFilesOneItem(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addSecondSource(t)

	f.validateFrom(t, sourceID, "h1", "7.24.3", "2026-09-02")

	f.conflicts.LinkErr = errors.New("connection reset while linking the review item")
	failing := f.candidateFor(t, "7.24.1", "2026-09-01")
	f.collector = &apptest.Collector{CollectorID: "c", Ver: "1", Candidates: []domain.CandidateRelease{failing}}
	deps := f.ingestDeps()
	ctx := context.Background()
	artID, _, err := f.artifacts.Put(ctx, "h2", "text/html", []byte("7.24.1"))
	if err != nil {
		t.Fatalf("store artifact: %v", err)
	}
	ex, err := application.NewExtractCandidates(deps).Execute(ctx, secondSourceID, artID)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := application.NewValidateCandidate(deps).Execute(ctx, ex.CandidateIDs[0]); err == nil {
		t.Fatal("validation reported success while the review item could not be linked")
	}

	f.conflicts.LinkErr = nil
	res, err := application.NewValidateCandidate(deps).Execute(ctx, ex.CandidateIDs[0])
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	if len(f.reviews.Items) != 1 {
		t.Fatalf("a retry after a failed link filed %d review items, want 1", len(f.reviews.Items))
	}
	if res.ReviewItemID != f.reviews.Items[0].ID {
		t.Errorf("retry reported item %q, want the one already filed (%q)", res.ReviewItemID, f.reviews.Items[0].ID)
	}
	conflict, err := f.conflicts.GetConflict(ctx, res.ConflictID)
	if err != nil {
		t.Fatalf("load conflict: %v", err)
	}
	if conflict.ReviewItemID != res.ReviewItemID {
		t.Errorf("conflict %s links review item %q, want %q", conflict.ID, conflict.ReviewItemID, res.ReviewItemID)
	}
	assertQueueCoversPendingCandidates(t, f)
}

// ---------------------------------------------------------------------------
// D13: compliance is re-checked at publication, not only at dispatch
// ---------------------------------------------------------------------------

// The revert detector for publishing after a takedown. ADR-0018's gate covers dispatch and
// fetching; a candidate already sitting in the review queue when an operator records
// terms_review_status = 'prohibited' used to publish anyway, because PublishRelease loaded
// the source only for its vendor id.
func TestPublicationRefusesASourceUnderATakedown(t *testing.T) {
	t.Parallel()
	f := newDecisionFixture(t)
	ctx := context.Background()

	// The vendor sends a takedown while the item waits in the queue.
	prohibited := f.source
	prohibited.TermsReviewStatus = domain.TermsProhibited
	prohibited.TermsReviewNote = "takedown notice received 2026-09-04"
	f.sources.Add(prohibited)

	_, err := application.NewDecideReviewItem(f.reviewDeps()).Accept(ctx, application.ReviewDecisionInput{
		ItemID: f.item.ID,
		Actor:  "alex",
		Reason: "looks right to me",
	})
	if err == nil {
		t.Fatal("accepting the queued item published data from a source FirmScout was told not to use")
	}
	if !errors.Is(err, domain.ErrNotPermitted) {
		t.Errorf("error = %v, want domain.ErrNotPermitted so the API can render the refusal", err)
	}
	// The refusal has to say what is wrong, or a reviewer cannot tell it from an outage.
	for _, want := range []string{sourceID, "terms review", string(domain.TermsProhibited)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}

	if f.releases.Count() != 0 {
		t.Errorf("%d releases published from a prohibited source", f.releases.Count())
	}
	// That the refusal also leaves the item in the queue is a property of the real
	// transaction rather than of this use case -- the in-memory unit of work runs its
	// function and rolls nothing back -- so it is proved against PostgreSQL in
	// TestAcceptingAQueuedItemRefusesAfterATakedown.
}

// The same check must not refuse the ordinary case: a restricted-but-approved source, or
// one whose robots policy does not apply, still publishes.
func TestPublicationStillAllowsARestrictedSource(t *testing.T) {
	t.Parallel()
	f := newDecisionFixture(t)
	ctx := context.Background()

	restricted := f.source
	restricted.TermsReviewStatus = domain.TermsRestricted
	restricted.RobotsPolicyStatus = domain.RobotsNotApplicable
	f.sources.Add(restricted)

	res, err := application.NewDecideReviewItem(f.reviewDeps()).Accept(ctx, application.ReviewDecisionInput{
		ItemID: f.item.ID,
		Actor:  "alex",
		Reason: "confirmed against the vendor's changelog page",
	})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !res.Published {
		t.Fatal("a restricted-but-permitted source was refused publication")
	}
}

// conflictQueryDeps wires the open-conflict read side from the same fakes the pipeline
// tests use. The pipeline fixture keeps no vendor repository -- nothing in the ingest
// path reads one -- so this supplies the single vendor its product belongs to.
func (f *fixture) conflictQueryDeps() application.ConflictQueryDeps {
	vendors := newFakeVendors()
	vendors.add(domain.Vendor{ID: vendorID, Slug: "mikrotik", Name: "MikroTik"})
	return application.ConflictQueryDeps{
		Conflicts: f.conflicts,
		Products:  f.products,
		Vendors:   vendors,
		Reviews:   f.reviews,
		Clock:     f.clock,
	}
}

// TestListOpenConflictsEnforcesTheDocumentedPageBounds pins the two-branch clamp.
//
// The single-branch form it replaced collapsed an over-large ask to the *default*, so
// `firmscout conflicts list --limit 1000` returned fifty rows and said nothing about
// having truncated -- to an operator whose whole reason for running the command is to
// find out whether the conflict detector is finding anything at all. ListReleases and
// ListReviewQueue were corrected earlier; this surface kept the old shape.
func TestListOpenConflictsEnforcesTheDocumentedPageBounds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	seed := func(f *fixture, n int) {
		t.Helper()
		for i := range n {
			channel := "ch-" + strconv.Itoa(i)
			sc := domain.SourceConflict{
				ID:            "cfl_" + strconv.Itoa(i),
				ProductID:     productID,
				Channel:       channel,
				State:         domain.ConflictOpen,
				Versions:      []string{"7.24.1", "7.24.3"},
				SourceIDs:     []string{sourceID, secondSourceID},
				DetectedAt:    f.clock.Now(),
				LastSeenAt:    f.clock.Now(),
				AuthorityRank: 0,
			}
			if _, _, err := f.conflicts.UpsertOpenConflict(ctx, sc); err != nil {
				t.Fatalf("seed conflict %d: %v", i, err)
			}
		}
	}

	for _, tc := range []struct {
		name string
		ask  int
		want int
	}{
		{name: "absent means the documented default", ask: 0, want: application.DefaultOpenConflictPageSize},
		{name: "negative means the documented default", ask: -7, want: application.DefaultOpenConflictPageSize},
		{name: "in range is honoured", ask: 123, want: 123},
		{name: "over-large means the maximum, not the default", ask: 10000, want: application.MaxOpenConflictPageSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			seed(f, application.MaxOpenConflictPageSize+50)

			entries, err := application.NewListOpenConflicts(f.conflictQueryDeps()).Execute(ctx, tc.ask)
			if err != nil {
				t.Fatalf("Execute(%d): %v", tc.ask, err)
			}
			if len(entries) != tc.want {
				t.Errorf("Execute(%d) returned %d entries, want %d", tc.ask, len(entries), tc.want)
			}
		})
	}
}
