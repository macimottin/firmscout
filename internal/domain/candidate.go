package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// CandidateState is the candidate release state machine.
//
// A candidate is an observation that may be wrong. It becomes a release only by
// passing validation, which is why these are separate states of a separate table
// rather than a status column on releases.
type CandidateState string

const (
	CandidateDiscovered          CandidateState = "discovered"
	CandidateExtracted           CandidateState = "extracted"
	CandidateNormalized          CandidateState = "normalized"
	CandidateValidationPending   CandidateState = "validation_pending"
	CandidateValidated           CandidateState = "validated"
	CandidateHumanReviewRequired CandidateState = "human_review_required"
	CandidateRejected            CandidateState = "rejected"
	CandidatePublished           CandidateState = "published"
	CandidateSuperseded          CandidateState = "superseded"
)

// candidateTransitions declares every legal edge. Notably absent: any edge back from
// published, and any edge from rejected other than to review.
var candidateTransitions = map[CandidateState][]CandidateState{
	CandidateDiscovered:          {CandidateExtracted, CandidateRejected},
	CandidateExtracted:           {CandidateNormalized, CandidateRejected, CandidateHumanReviewRequired},
	CandidateNormalized:          {CandidateValidationPending, CandidateRejected, CandidateHumanReviewRequired},
	CandidateValidationPending:   {CandidateValidated, CandidateRejected, CandidateHumanReviewRequired},
	CandidateValidated:           {CandidatePublished, CandidateRejected, CandidateHumanReviewRequired},
	CandidateHumanReviewRequired: {CandidateValidated, CandidateRejected, CandidatePublished},
	CandidateRejected:            {CandidateHumanReviewRequired},
	CandidatePublished:           {CandidateSuperseded},
	CandidateSuperseded:          {},
}

// ValidCandidateState reports whether s is a declared candidate state.
func ValidCandidateState(s CandidateState) bool {
	_, ok := candidateTransitions[s]
	return ok
}

// CanTransitionTo reports whether the edge from s to next exists.
func (s CandidateState) CanTransitionTo(next CandidateState) bool {
	for _, allowed := range candidateTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Terminal reports whether no further transitions are possible.
func (s CandidateState) Terminal() bool { return len(candidateTransitions[s]) == 0 }

// ProductMatchStatus records how confidently a candidate was attached to a product.
// An ambiguous match is never guessed; it goes to a human.
type ProductMatchStatus string

const (
	MatchUnresolved ProductMatchStatus = "unresolved"
	MatchUnique     ProductMatchStatus = "unique"
	MatchAmbiguous  ProductMatchStatus = "ambiguous"
	MatchNoMatch    ProductMatchStatus = "no_match"
)

// Applicability constrains which devices a release applies to. The zero value means
// "applies to the matched product without further constraint".
type Applicability struct {
	HardwareRevision string
	Region           string
	Channel          string
	DeploymentMode   string
	Note             string
}

// CandidateRelease is an extracted, not-yet-trusted observation of a release.
//
// Collectors produce these and nothing else. A collector is given no repository
// handle, so it is structurally incapable of publishing; see ADR-0005.
type CandidateRelease struct {
	ID             string
	SourceID       string
	CollectorRunID string
	EvidenceID     string

	ProductID          string
	ProductFamilyID    string
	ProductMatchHint   string
	ProductMatchStatus ProductMatchStatus

	Version             VersionString
	ReleaseType         ReleaseType
	ProposedReleaseType ReleaseType
	Applicability       Applicability

	ReleaseDate     PartialDate
	PublicationDate PartialDate
	ReleaseNotesURL string

	Confidence float64
	DedupeKey  string
	State      CandidateState

	RejectionReason    string
	PublishedReleaseID string
	DiscoveredAt       time.Time
	UpdatedAt          time.Time
}

// Validate checks the invariants a candidate must satisfy before validation gates run.
func (c CandidateRelease) Validate() error {
	if strings.TrimSpace(c.SourceID) == "" {
		return invalid("candidate.source_id", "must be set")
	}
	if c.Version.IsZero() {
		return invalid("candidate.version", "must not be empty")
	}
	if c.ReleaseType != "" && !ValidReleaseType(c.ReleaseType) {
		return invalid("candidate.release_type", string(c.ReleaseType)+" is not a known release type")
	}
	if !ValidDatePrecision(c.ReleaseDate.Precision()) {
		return invalid("candidate.release_date_precision", "unknown precision")
	}
	if c.Confidence < 0 || c.Confidence > 1 {
		return invalid("candidate.confidence", "must be between 0 and 1")
	}
	if !ValidCandidateState(c.State) {
		return invalid("candidate.state", string(c.State)+" is not a known candidate state")
	}
	if strings.TrimSpace(c.DedupeKey) == "" {
		return invalid("candidate.dedupe_key", "must be set")
	}
	return nil
}

// TransitionTo moves the candidate along the state machine, rejecting edges that do
// not exist.
func (c *CandidateRelease) TransitionTo(next CandidateState) error {
	if !ValidCandidateState(next) {
		return invalid("candidate.state", string(next)+" is not a known candidate state")
	}
	if c.State == next {
		return nil
	}
	if !c.State.CanTransitionTo(next) {
		return &TransitionError{Entity: "candidate_release", From: string(c.State), To: string(next)}
	}
	c.State = next
	return nil
}

// ComputeDedupeKey derives the per-source uniqueness key for a candidate.
//
// This is the single most dangerous value a collector author can get wrong. Computed
// inconsistently between runs it produces duplicate candidates on every check;
// computed too coarsely it silently swallows genuine releases. The SDK uses this
// implementation so that collectors do not each invent their own.
//
// The key covers the identity of the release as the source describes it: the product
// hint, the normalised version, the channel, and the applicability constraints that
// distinguish otherwise identical rows. It deliberately excludes the release date,
// because vendors sometimes correct a date after publication and that must update the
// existing candidate rather than create a second one.
func ComputeDedupeKey(productHint string, version VersionString, app Applicability) string {
	parts := []string{
		strings.ToLower(strings.TrimSpace(productHint)),
		version.Normalized(),
		strings.ToLower(strings.TrimSpace(app.Channel)),
		strings.ToLower(strings.TrimSpace(app.HardwareRevision)),
		strings.ToLower(strings.TrimSpace(app.Region)),
		strings.ToLower(strings.TrimSpace(app.DeploymentMode)),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:16])
}
