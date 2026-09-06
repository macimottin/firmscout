package application_test

import (
	"context"
	"errors"
	"testing"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/application/apptest"
	"github.com/macimottin/firmscout/internal/domain"
)

// decisionFixture builds a world in which one candidate has already been routed to human
// review, which is the only state a decision can act on.
type decisionFixture struct {
	*fixture
	item        application.ReviewItem
	candidateID string
}

func (f *fixture) reviewDeps() application.ReviewDeps {
	return application.ReviewDeps{
		Reviews:    f.reviews,
		Candidates: f.candidates,
		Conflicts:  f.conflicts,
		Audit:      f.audit,
		Publisher:  application.NewPublishRelease(f.ingestDeps()),
		Releases:   f.releases,
		Events:     f.events,
		UoW:        f.uow,
		Clock:      f.clock,
		IDs:        f.ids,
	}
}

// newDecisionFixture routes a candidate to review the way the pipeline does -- by failing
// a gate -- rather than by writing the review row directly. A test that hand-built the
// queue row would prove nothing about what a real queued item looks like.
func newDecisionFixture(t *testing.T) *decisionFixture {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()

	candidate := f.candidateFor(t, "7.24.3", "2026-09-02")
	// Below the automatic-publication threshold: gate 8 sends it to a human.
	candidate.Confidence = 0.40
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
	res, err := application.NewValidateCandidate(deps).Execute(ctx, ex.CandidateIDs[0])
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if res.Decision != domain.GateReviewRequired {
		t.Fatalf("decision = %q, want review_required; the fixture needs a queued item", res.Decision)
	}
	item, err := f.reviews.GetByID(ctx, res.ReviewItemID)
	if err != nil {
		t.Fatalf("load review item: %v", err)
	}
	return &decisionFixture{fixture: f, item: item, candidateID: res.CandidateID}
}

// The revert detector for the review queue's whole point: a human can override a gate,
// and the override is recorded rather than silent.
func TestAcceptPublishesCandidateThatFailedAGate(t *testing.T) {
	t.Parallel()
	f := newDecisionFixture(t)
	ctx := context.Background()

	// The gate that routed it is still on record; accepting does not erase it.
	gates, err := f.candidates.ListValidationResults(ctx, f.candidateID)
	if err != nil {
		t.Fatalf("list validation results: %v", err)
	}
	var failing bool
	for _, g := range gates {
		if g.Gate == domain.GateConfidenceThreshold && g.Outcome == domain.GateReviewRequired {
			failing = true
		}
	}
	if !failing {
		t.Fatal("the fixture's candidate did not record a failing gate")
	}

	res, err := application.NewDecideReviewItem(f.reviewDeps()).Accept(ctx, application.ReviewDecisionInput{
		ItemID:    f.item.ID,
		Actor:     "alex",
		Reason:    "confirmed against the vendor's changelog page",
		RequestID: "req_1",
	})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !res.Published || res.ReleaseID == "" {
		t.Fatalf("accepting did not publish: %+v", res)
	}
	if f.releases.Count() != 1 {
		t.Fatalf("stored %d releases, want 1", f.releases.Count())
	}

	stored, err := f.reviews.GetByID(ctx, f.item.ID)
	if err != nil {
		t.Fatalf("reload review item: %v", err)
	}
	if stored.State != application.ReviewStateResolved {
		t.Errorf("review item state = %q, want resolved", stored.State)
	}
	if stored.ResolvedBy != "alex" {
		t.Errorf("resolved by = %q, want alex", stored.ResolvedBy)
	}
	if stored.Resolution == "" {
		t.Error("the resolution recorded no reason; that is a timestamp, not an audit trail")
	}

	candidate, err := f.candidates.GetByID(ctx, f.candidateID)
	if err != nil {
		t.Fatalf("reload candidate: %v", err)
	}
	if candidate.State != domain.CandidatePublished {
		t.Errorf("candidate state = %q, want published", candidate.State)
	}

	events := f.audit.Events()
	if len(events) != 1 {
		t.Fatalf("recorded %d audit events, want exactly 1", len(events))
	}
	audit := events[0]
	if audit.ActorAuthenticated {
		t.Error("the audit row claims the actor was authenticated; FirmScout has no login (ADR-0021)")
	}
	if audit.ActorType != application.ActorTypeHuman || audit.ActorID != "alex" {
		t.Errorf("audit actor = %s/%s, want human/alex", audit.ActorType, audit.ActorID)
	}
	if audit.Action != application.AuditActionReviewAccepted {
		t.Errorf("audit action = %q, want %q", audit.Action, application.AuditActionReviewAccepted)
	}
	if audit.Reason == "" || audit.RequestID != "req_1" {
		t.Errorf("audit row lost the reason or the request id: %+v", audit)
	}
	if audit.AfterState["release_id"] != res.ReleaseID {
		t.Errorf("audit after-state release id = %q, want %q", audit.AfterState["release_id"], res.ReleaseID)
	}
	if audit.BeforeState["candidate_state"] != string(domain.CandidateHumanReviewRequired) {
		t.Errorf("audit before-state candidate = %q, want human_review_required",
			audit.BeforeState["candidate_state"])
	}
	if n := f.events.Count(domain.EventReviewItemResolved); n != 1 {
		t.Errorf("published %d ReviewItemResolved events, want 1", n)
	}
}

func TestRejectMovesTheCandidateThroughTheStateMachine(t *testing.T) {
	t.Parallel()
	f := newDecisionFixture(t)
	ctx := context.Background()

	res, err := application.NewDecideReviewItem(f.reviewDeps()).Reject(ctx, application.ReviewDecisionInput{
		ItemID: f.item.ID,
		Actor:  "alex",
		Reason: "the page was showing a beta build",
	})
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if res.Published || res.ReleaseID != "" {
		t.Fatalf("rejecting published something: %+v", res)
	}
	if f.releases.Count() != 0 {
		t.Errorf("%d releases exist after a rejection", f.releases.Count())
	}

	candidate, err := f.candidates.GetByID(ctx, f.candidateID)
	if err != nil {
		t.Fatalf("reload candidate: %v", err)
	}
	if candidate.State != domain.CandidateRejected {
		t.Errorf("candidate state = %q, want rejected", candidate.State)
	}
	if candidate.RejectionReason == "" {
		t.Error("the rejection recorded no reason")
	}
	if events := f.audit.Events(); len(events) != 1 || events[0].Action != application.AuditActionReviewRejected {
		t.Errorf("audit trail does not record the rejection: %+v", events)
	}
}

func TestAcceptRefusesWithoutActorOrReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   application.ReviewDecisionInput
	}{
		{"no actor", application.ReviewDecisionInput{Actor: "   ", Reason: "looks right"}},
		{"no reason", application.ReviewDecisionInput{Actor: "alex", Reason: ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newDecisionFixture(t)
			in := tc.in
			in.ItemID = f.item.ID

			if _, err := application.NewDecideReviewItem(f.reviewDeps()).Accept(context.Background(), in); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Accept error = %v, want a validation error", err)
			}
			if f.releases.Count() != 0 {
				t.Error("a decision with no audit trail published a release anyway")
			}
			if len(f.audit.Events()) != 0 {
				t.Error("an audit row was written for a refused decision")
			}
		})
	}
}

func TestAcceptRefusesAnAlreadyResolvedItem(t *testing.T) {
	t.Parallel()
	f := newDecisionFixture(t)
	ctx := context.Background()
	uc := application.NewDecideReviewItem(f.reviewDeps())
	in := application.ReviewDecisionInput{ItemID: f.item.ID, Actor: "alex", Reason: "confirmed"}

	if _, err := uc.Accept(ctx, in); err != nil {
		t.Fatalf("first Accept: %v", err)
	}

	// Two reviewers racing produce one decision and one visible failure, never two
	// publications of the same candidate.
	_, err := uc.Accept(ctx, application.ReviewDecisionInput{ItemID: f.item.ID, Actor: "sam", Reason: "also confirmed"})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second Accept error = %v, want a conflict error", err)
	}
	if f.releases.Count() != 1 {
		t.Errorf("stored %d releases, want 1", f.releases.Count())
	}
	if len(f.audit.Events()) != 1 {
		t.Errorf("recorded %d audit events, want 1", len(f.audit.Events()))
	}
}

func TestDecidingAMissingItemIsNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := application.NewDecideReviewItem(f.reviewDeps()).Accept(context.Background(),
		application.ReviewDecisionInput{ItemID: "rev_nope", Actor: "alex", Reason: "why not"})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Accept error = %v, want not found", err)
	}
}

// A decision on a conflict item closes the conflict too: leaving it open would keep the
// warning on the product page after the disagreement was settled.
func TestDecidingAConflictItemResolvesTheConflict(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addSecondSource(t)
	ctx := context.Background()

	f.validateFrom(t, sourceID, "h1", "7.24.3", "2026-09-02")
	conflicted := f.validateFrom(t, secondSourceID, "h2", "7.24.1", "2026-09-01")
	if conflicted.ConflictID == "" || conflicted.ReviewItemID == "" {
		t.Fatalf("the fixture did not produce a conflict with a review item: %+v", conflicted)
	}

	res, err := application.NewDecideReviewItem(f.reviewDeps()).Reject(ctx, application.ReviewDecisionInput{
		ItemID: conflicted.ReviewItemID,
		Actor:  "alex",
		Reason: "the release-notes page lags the changelog; 7.24.3 is correct",
	})
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if res.ConflictID != conflicted.ConflictID {
		t.Errorf("decision conflict id = %q, want %q", res.ConflictID, conflicted.ConflictID)
	}
	if open := f.conflicts.OpenConflicts(); len(open) != 0 {
		t.Fatalf("%d conflicts still open after the decision", len(open))
	}
	stored, err := f.conflicts.GetConflict(ctx, conflicted.ConflictID)
	if err != nil {
		t.Fatalf("load conflict: %v", err)
	}
	if stored.ResolvedBy != "alex" || stored.Resolution == "" {
		t.Errorf("the conflict does not record who resolved it and why: %+v", stored)
	}
}

// The decision runs inside a unit of work, with the publication nested in it. The fake
// counts every Within call, including the nested one; the real adapter's Within joins an
// already-open transaction rather than starting a second, which is what makes "published
// but still queued" impossible. What this test proves is the nesting -- that Accept does
// not publish outside its own transaction.
func TestDecisionRunsInsideAUnitOfWork(t *testing.T) {
	t.Parallel()
	f := newDecisionFixture(t)
	before := f.uow.Transactions

	if _, err := application.NewDecideReviewItem(f.reviewDeps()).Accept(context.Background(),
		application.ReviewDecisionInput{ItemID: f.item.ID, Actor: "alex", Reason: "confirmed"}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if opened := f.uow.Transactions - before; opened != 2 {
		t.Errorf("the decision entered Within %d times, want 2: its own and the publication nested in it", opened)
	}
}
