package application

import (
	"testing"

	"github.com/macimottin/firmscout/internal/domain"
)

// TestReviewPayloadListsEveryDisputingCandidate is the revert detector for
// disputingCandidateIDs.
//
// PayloadKeyConflictCandidates (ports.go) documents that a candidate parked in
// human_review_required stays reachable from the queue even when it is not the item's
// subject. Nothing enforces that promise end to end: the only other reader is
// assertQueueCoversPendingCandidates in conflict_routing_test.go, and its check on this
// key is a fallback that never fires in this repository's scenarios, because every
// candidate it inspects also happens to be the item's own SubjectID -- the primary
// check the helper tries first. A disputingCandidateIDs that always returned nil would
// leave that helper, and the rest of the suite, green.
//
// This test is a whitebox test (package application, not application_test) for exactly
// that reason: it calls reviewPayload directly, with a second, non-subject candidate
// in the assessment, so the fallback path is the only one that can make it pass.
func TestReviewPayloadListsEveryDisputingCandidate(t *testing.T) {
	assessment := conflictAssessment{
		Subject: domain.SourceObservation{SourceID: "src_a", CandidateID: "cand_subject"},
		Observations: []domain.SourceObservation{
			{SourceID: "src_a", CandidateID: "cand_subject", Eligible: true},
			// The one thing item.SubjectID cannot name: a second candidate, from a
			// different source, that the same conflict is about. Reachable only
			// through PayloadKeyConflictCandidates.
			{SourceID: "src_b", CandidateID: "cand_other", Eligible: true},
			// An ineligible source's candidate is not "in dispute" and must not
			// appear alongside the ones that are.
			{SourceID: "src_c", CandidateID: "cand_ineligible", Eligible: false},
		},
		Verdict: domain.ConflictVerdict{
			ConflictingVersions:  []string{"2.0.0"},
			ConflictingSourceIDs: []string{"src_b"},
		},
	}
	outcome := conflictOutcome{ConflictID: "conf_1"}

	payload := reviewPayload(domain.ValidationVerdict{}, outcome, assessment)

	got := payload[PayloadKeyConflictCandidates]
	want := "cand_other,cand_subject" // sorted, deduplicated, the ineligible source excluded
	if got != want {
		t.Fatalf("payload[%s] = %q, want %q -- the non-subject candidate is not reachable from the queue",
			PayloadKeyConflictCandidates, got, want)
	}
}
