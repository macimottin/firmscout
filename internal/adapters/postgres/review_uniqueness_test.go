package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// discardEvents satisfies the event port for the tests below. Nothing here asserts on
// events; what these tests are about is what survives a commit and what does not.
type discardEvents struct{}

func (discardEvents) Publish(context.Context, ...domain.Event) error { return nil }

// fixedClock keeps a decision's timestamps deterministic.
type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

// subjectItem builds an open review item for one subject.
func subjectItem(id, subjectType, subjectID, vendorID, productID string) application.ReviewItem {
	return application.ReviewItem{
		ID:            id,
		Kind:          "multi_source_conflict",
		SubjectType:   subjectType,
		SubjectID:     subjectID,
		VendorID:      vendorID,
		ProductID:     productID,
		Title:         "needs a decision",
		Detail:        "two sources disagree",
		Payload:       map[string]string{application.PayloadKeyConflictVersions: "7.24.1,7.24.9"},
		PriorityScore: 200,
		SLAClass:      "high",
	}
}

// TestOpenReviewItemIsUniquePerSubject is the revert detector for migration 00003's half
// of the idempotency fix.
//
// review_items_subject_idx (00002) is a plain index: it made "is there already an item for
// this candidate" fast and left the answer allowed to be "three". The job queue delivers
// at least once, so without the unique index a redelivered validation filed another copy
// of the same decision and nothing in the schema objected.
func TestOpenReviewItemIsUniquePerSubject(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	reviews := NewReviewRepo(db)

	first := subjectItem("rev_1", application.SubjectTypeCandidateRelease, "cand_1", v.ID, p.ID)
	if err := reviews.Create(ctx, first); err != nil {
		t.Fatalf("Create: %v", err)
	}

	second := subjectItem("rev_2", application.SubjectTypeCandidateRelease, "cand_1", v.ID, p.ID)
	if err := reviews.Create(ctx, second); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a second open item for the same candidate = %v, want domain.ErrConflict", err)
	}

	// A different subject type against the same id is a different decision and must not
	// collide: an item about the conflict itself and one about a candidate are answers to
	// different questions.
	conflictItem := subjectItem("rev_3", application.SubjectTypeSourceConflict, "cand_1", v.ID, p.ID)
	if err := reviews.Create(ctx, conflictItem); err != nil {
		t.Fatalf("an item with a different subject type was refused: %v", err)
	}

	found, err := reviews.FindOpenBySubject(ctx, application.SubjectTypeCandidateRelease, "cand_1")
	if err != nil {
		t.Fatalf("FindOpenBySubject: %v", err)
	}
	if found.ID != "rev_1" {
		t.Errorf("FindOpenBySubject = %q, want rev_1", found.ID)
	}
	if _, err := reviews.FindOpenBySubject(ctx, application.SubjectTypeCandidateRelease, "cand_absent"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("FindOpenBySubject on an unqueued subject = %v, want domain.ErrNotFound", err)
	}

	// The index is partial on the open states, because resolved items are history and a
	// subject legitimately accumulates several of them over its life.
	if err := reviews.Resolve(ctx, "rev_1", "accepted: confirmed", "alex", time.Now().UTC()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := reviews.Create(ctx, second); err != nil {
		t.Fatalf("a new item after the previous one was resolved: %v", err)
	}
	if _, err := reviews.FindOpenBySubject(ctx, application.SubjectTypeCandidateRelease, "cand_1"); err != nil {
		t.Fatalf("FindOpenBySubject after re-filing: %v", err)
	}
}

// TestRetargetRewritesAnOpenItemInPlace proves the operation the "one disagreement, one
// queue item" rule now depends on: the item follows the dispute instead of being closed
// and re-filed, so its age and position in the queue stay with the disagreement.
func TestRetargetRewritesAnOpenItemInPlace(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	reviews := NewReviewRepo(db)

	item := subjectItem("rev_1", application.SubjectTypeCandidateRelease, "cand_1", v.ID, p.ID)
	// Truncated to what PostgreSQL stores: timestamptz keeps microseconds, and a
	// nanosecond the column cannot hold would make this assertion about rounding.
	item.CreatedAt = time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	if err := reviews.Create(ctx, item); err != nil {
		t.Fatalf("Create: %v", err)
	}

	item.SubjectID = "cand_2"
	item.Title = "Candidate 7.24.5 needs review"
	item.Payload = map[string]string{application.PayloadKeyConflictVersions: "7.24.3,7.24.5"}
	item.PriorityScore = 275
	item.SLAClass = "urgent"
	if err := reviews.Retarget(ctx, item); err != nil {
		t.Fatalf("Retarget: %v", err)
	}

	got, err := reviews.GetByID(ctx, "rev_1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.SubjectID != "cand_2" || got.Title != "Candidate 7.24.5 needs review" {
		t.Errorf("retargeted item = %+v, want it pointed at cand_2", got)
	}
	if got.Payload[application.PayloadKeyConflictVersions] != "7.24.3,7.24.5" {
		t.Errorf("payload = %v, want the versions in dispute today", got.Payload)
	}
	if got.PriorityScore != 275 || got.SLAClass != "urgent" {
		t.Errorf("priority = %d/%s, want 275/urgent", got.PriorityScore, got.SLAClass)
	}
	// The item's age belongs to the disagreement, not to whichever candidate last
	// surfaced it, so a retarget must not reset it to the front of the queue.
	if !got.CreatedAt.Equal(item.CreatedAt.UTC()) {
		t.Errorf("created_at moved from %s to %s; retargeting must not restart the clock",
			item.CreatedAt.UTC(), got.CreatedAt)
	}
	// The retarget must also have kept it findable under its new subject and not under
	// the old one, or the idempotency lookup would file a duplicate on the next delivery.
	if found, err := reviews.FindOpenBySubject(ctx, application.SubjectTypeCandidateRelease, "cand_2"); err != nil || found.ID != "rev_1" {
		t.Errorf("FindOpenBySubject(cand_2) = (%+v, %v), want rev_1", found, err)
	}
	if _, err := reviews.FindOpenBySubject(ctx, application.SubjectTypeCandidateRelease, "cand_1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("the old subject is still queued (%v)", err)
	}

	// A decision a human has already made is not something the pipeline may reword.
	if err := reviews.Resolve(ctx, "rev_1", "accepted: confirmed", "alex", time.Now().UTC()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := reviews.Retarget(ctx, item); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("retargeting a resolved item = %v, want domain.ErrNotFound", err)
	}
}

// TestReviewItemCreationRollsBackWithItsUnitOfWork is the half the in-memory fakes cannot
// prove: ValidateCandidate now files its review item inside the same transaction as the
// link to the conflict, so a failure at the link leaves no item behind for the retry to
// trip over. Before that, the item was committed, the link was not, the worker retried,
// and a second item was filed for the same disagreement.
func TestReviewItemCreationRollsBackWithItsUnitOfWork(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)
	reviews := NewReviewRepo(db)

	linkFailed := errors.New("connection reset while linking the review item")
	err := db.Within(ctx, func(ctx context.Context) error {
		if err := reviews.Create(ctx, subjectItem("rev_1", application.SubjectTypeCandidateRelease, "cand_1", v.ID, p.ID)); err != nil {
			return err
		}
		return linkFailed
	})
	if !errors.Is(err, linkFailed) {
		t.Fatalf("Within returned %v, want the caller's own error", err)
	}
	if _, err := reviews.FindOpenBySubject(ctx, application.SubjectTypeCandidateRelease, "cand_1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a review item survived a rolled-back unit of work (%v)", err)
	}

	// The retry commits exactly one item.
	if err := db.Within(ctx, func(ctx context.Context) error {
		return reviews.Create(ctx, subjectItem("rev_2", application.SubjectTypeCandidateRelease, "cand_1", v.ID, p.ID))
	}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	open, _, err := reviews.List(ctx, application.ReviewQueueFilter{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(open) != 1 || open[0].ID != "rev_2" {
		t.Fatalf("queue = %d items %v, want exactly rev_2", len(open), idsOf(open))
	}
}

// TestAcceptingAQueuedItemRefusesAfterATakedown is the revert detector for the compliance
// check on the publication path, run against the real database and the real decision use
// case because the property it proves is transactional: the refusal must leave the item in
// the queue, not resolve it and then fail.
//
// ADR-0018's gate stops the next check of a prohibited source. It never covered promotion
// of what had already been collected, so a candidate queued for review before a takedown
// still published days after an operator recorded that FirmScout may not use the source.
func TestAcceptingAQueuedItemRefusesAfterATakedown(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	v, p := seedCatalog(t, db)

	sources := NewSourceRepo(db)
	src := newSource("src_changelog", v.ID, "changelog")
	if err := sources.Upsert(ctx, src); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	ev := newEvidence("ev_1", src.ID)
	if err := NewEvidenceRepo(db).Insert(ctx, ev); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	candidates := NewCandidateRepo(db)
	cand := newCandidate(t, src.ID, p.ID, "7.24.2")
	cand.ID = "cand_1"
	cand.EvidenceID = ev.ID
	stored, _, err := candidates.UpsertByDedupeKey(ctx, cand)
	if err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	for _, state := range []domain.CandidateState{
		domain.CandidateNormalized, domain.CandidateValidationPending, domain.CandidateHumanReviewRequired,
	} {
		if err := candidates.UpdateState(ctx, stored.ID, state, "confidence below the threshold"); err != nil {
			t.Fatalf("move candidate to %s: %v", state, err)
		}
	}

	reviews := NewReviewRepo(db)
	item := subjectItem("rev_1", application.SubjectTypeCandidateRelease, stored.ID, v.ID, p.ID)
	item.Kind = "candidate_low_confidence"
	item.Payload = map[string]string{}
	if err := reviews.Create(ctx, item); err != nil {
		t.Fatalf("seed review item: %v", err)
	}

	now := time.Now().UTC()
	ingest := application.IngestDeps{
		Sources: sources, Products: NewProductRepo(db), Candidates: candidates,
		Releases: NewReleaseRepo(db), Evidence: NewEvidenceRepo(db), Reviews: reviews,
		Conflicts: NewConflictRepo(db), Audit: NewAuditRepo(db), Queue: NewQueue(db),
		Events: discardEvents{}, UoW: db, Clock: fixedClock{at: now}, IDs: ulidGenerator{},
	}
	decide := application.NewDecideReviewItem(application.ReviewDeps{
		Reviews: reviews, Candidates: candidates, Conflicts: NewConflictRepo(db),
		Audit: NewAuditRepo(db), Publisher: application.NewPublishRelease(ingest),
		Events: discardEvents{}, UoW: db, Clock: fixedClock{at: now}, IDs: ulidGenerator{},
	})

	// The compliant case first, so this test cannot pass by refusing everything.
	if _, err := application.NewPublishRelease(ingest).Execute(ctx, stored.ID); err != nil {
		t.Fatalf("a compliant source was refused publication: %v", err)
	}

	// Now the takedown, against a fresh candidate in the same state.
	second := newCandidate(t, src.ID, p.ID, "7.24.3")
	second.ID = "cand_2"
	second.EvidenceID = ev.ID
	secondStored, _, err := candidates.UpsertByDedupeKey(ctx, second)
	if err != nil {
		t.Fatalf("seed second candidate: %v", err)
	}
	for _, state := range []domain.CandidateState{
		domain.CandidateNormalized, domain.CandidateValidationPending, domain.CandidateHumanReviewRequired,
	} {
		if err := candidates.UpdateState(ctx, secondStored.ID, state, "confidence below the threshold"); err != nil {
			t.Fatalf("move second candidate to %s: %v", state, err)
		}
	}
	secondItem := subjectItem("rev_2", application.SubjectTypeCandidateRelease, secondStored.ID, v.ID, p.ID)
	secondItem.Kind = "candidate_low_confidence"
	secondItem.Payload = map[string]string{}
	if err := reviews.Create(ctx, secondItem); err != nil {
		t.Fatalf("seed second review item: %v", err)
	}

	src.TermsReviewStatus = domain.TermsProhibited
	src.TermsReviewNote = "takedown notice received"
	if err := sources.Upsert(ctx, src); err != nil {
		t.Fatalf("record the takedown: %v", err)
	}

	releasesBefore := releaseCount(t, db)
	_, err = decide.Accept(ctx, application.ReviewDecisionInput{
		ItemID: "rev_2", Actor: "alex", Reason: "looks right to me",
	})
	if err == nil {
		t.Fatal("accepting the queued item published data from a source under a takedown")
	}
	if !errors.Is(err, domain.ErrNotPermitted) {
		t.Errorf("error = %v, want domain.ErrNotPermitted", err)
	}

	if got := releaseCount(t, db); got != releasesBefore {
		t.Errorf("%d releases exist, want %d: nothing may be published from a prohibited source",
			got, releasesBefore)
	}
	stillOpen, err := reviews.GetByID(ctx, "rev_2")
	if err != nil {
		t.Fatalf("reload review item: %v", err)
	}
	if stillOpen.State != application.ReviewStateOpen {
		t.Errorf("review item state = %q; a refused decision must roll back and leave the item queued",
			stillOpen.State)
	}
	refused, err := candidates.GetByID(ctx, secondStored.ID)
	if err != nil {
		t.Fatalf("reload candidate: %v", err)
	}
	if refused.State != domain.CandidateHumanReviewRequired {
		t.Errorf("candidate state = %q, want it left awaiting review", refused.State)
	}
}

func releaseCount(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := pool(db).QueryRow(context.Background(), `SELECT count(*) FROM releases`).Scan(&n); err != nil {
		t.Fatalf("count releases: %v", err)
	}
	return n
}
