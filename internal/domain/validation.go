package domain

import (
	"strings"
	"time"
)

// ValidationGate names one of the deterministic checks a candidate must pass before
// publication. The order is the order they run in; an earlier gate's rejection short
// circuits the rest.
type ValidationGate string

const (
	GateProductIdentity     ValidationGate = "product_identity"
	GateVersionPresent      ValidationGate = "version_present"
	GateSourceEligible      ValidationGate = "source_eligible"
	GateDuplicate           ValidationGate = "duplicate"
	GateDateValid           ValidationGate = "date_valid"
	GateVersionTransition   ValidationGate = "version_transition"
	GateEvidenceRetained    ValidationGate = "evidence_retained"
	GateConfidenceThreshold ValidationGate = "confidence_threshold"
	GateApplicability       ValidationGate = "applicability"
	GateMultiSourceAgree    ValidationGate = "multi_source_agreement"
)

// gateOrder is the fixed running order of the validation gates.
var gateOrder = []ValidationGate{
	GateProductIdentity,
	GateVersionPresent,
	GateSourceEligible,
	GateDuplicate,
	GateDateValid,
	GateVersionTransition,
	GateEvidenceRetained,
	GateConfidenceThreshold,
	GateApplicability,
	GateMultiSourceAgree,
}

// GateOrder returns the gates in running order.
func GateOrder() []ValidationGate {
	out := make([]ValidationGate, len(gateOrder))
	copy(out, gateOrder)
	return out
}

// GateIndex returns the 1-based position of a gate in the running order.
func GateIndex(g ValidationGate) int {
	for i, x := range gateOrder {
		if x == g {
			return i + 1
		}
	}
	return 0
}

// GateOutcome is the verdict of a single gate.
type GateOutcome string

const (
	GatePassed         GateOutcome = "passed"
	GateRejected       GateOutcome = "rejected"
	GateReviewRequired GateOutcome = "review_required"
	GateSkipped        GateOutcome = "skipped"
)

// GateResult records one gate's verdict with the reasoning, so a reviewer sees why
// rather than only what.
type GateResult struct {
	Gate        ValidationGate
	Order       int
	Outcome     GateOutcome
	Detail      string
	EvaluatedBy string
	EvaluatedAt time.Time
}

// ValidationVerdict is the aggregate result of running the gates.
type ValidationVerdict struct {
	Results []GateResult
	// Decision is the overall outcome: publish, review, or reject.
	Decision GateOutcome
	// Reason summarises the deciding gate.
	Reason string
}

// Publishable reports whether every gate passed and the candidate may be published
// without human involvement.
func (v ValidationVerdict) Publishable() bool { return v.Decision == GatePassed }

// ValidationContext is everything the deterministic gates need. It is assembled by the
// application from repositories and passed in, so that RunValidationGates stays a pure
// function testable without a database.
type ValidationContext struct {
	Now time.Time

	// Product resolution, established before the gates run.
	ProductResolved  bool
	ProductAmbiguous bool
	// ResolvedProductID carries the match back to the caller when the candidate
	// arrived without one, so publication does not resolve it a second time.
	ResolvedProductID string

	// Source eligibility.
	SourceHealth   SourceHealth
	SourceEligible bool
	SourceQuality  QualityClass
	SourceOfficial bool

	// Duplicate detection: the already-published release with the same product,
	// normalised version and channel, if one exists.
	DuplicateReleaseID string

	// The latest observed version for this product and channel, used for the
	// plausibility check. Zero when there is none.
	PreviousVersion VersionString

	// EvidencePresent reports whether an artifact was stored and a non-empty excerpt
	// retained.
	EvidencePresent bool

	// ConfidenceThreshold is the minimum confidence for automatic publication.
	// Community sources always route to review regardless of confidence.
	ConfidenceThreshold float64

	// ApplicabilityKnown reports whether the hardware revision and region named by
	// the candidate are recognised for the product.
	ApplicabilityKnown bool

	// ConflictingSourceVersions holds versions reported by other eligible sources for
	// the same product and channel that this candidate's source does not outrank.
	// A non-empty value routes to review and is never auto-resolved (ADR-0020).
	ConflictingSourceVersions []string

	// OutrankedSourceVersions holds versions reported by eligible sources this
	// candidate's source outranks on the authority ladder. They do not block
	// publication, but they are named in the gate's detail so a reviewer reading
	// validation_results sees that a disagreement existed and why it did not stop the
	// release.
	OutrankedSourceVersions []string

	// FutureDateTolerance bounds how far ahead of Now a release date may be before
	// it is treated as implausible. Vendors do occasionally post-date a release.
	FutureDateTolerance time.Duration

	// EarliestPlausibleDate rejects dates before a vendor could plausibly have
	// published anything.
	EarliestPlausibleDate time.Time
}

// DefaultConfidenceThreshold is the automatic-publication threshold for official
// sources. Community sources always route to human review.
const DefaultConfidenceThreshold = 0.85

// RunValidationGates applies the ten deterministic gates in order and returns the
// verdict.
//
// The function is pure: it reads only the candidate and the context it is given, and
// it performs no I/O. That is what allows the entire publication policy to be tested
// in microseconds with no database.
//
// A gate may reject (the candidate is wrong and will not be published), require review
// (the candidate may be right but a human must decide), or pass. Rejection short
// circuits; a review requirement does not, because a reviewer benefits from seeing
// every gate's verdict rather than only the first problem.
func RunValidationGates(c CandidateRelease, ctx ValidationContext) ValidationVerdict {
	var results []GateResult
	decision := GatePassed
	reason := "all gates passed"

	record := func(g ValidationGate, outcome GateOutcome, detail string) bool {
		results = append(results, GateResult{
			Gate:        g,
			Order:       GateIndex(g),
			Outcome:     outcome,
			Detail:      detail,
			EvaluatedBy: "deterministic",
			EvaluatedAt: ctx.Now,
		})
		switch outcome {
		case GateRejected:
			decision = GateRejected
			reason = string(g) + ": " + detail
			return false // short circuit
		case GateReviewRequired:
			if decision != GateRejected {
				decision = GateReviewRequired
				reason = string(g) + ": " + detail
			}
		}
		return true
	}

	// 1. Product identity.
	switch {
	case ctx.ProductAmbiguous:
		if !record(GateProductIdentity, GateReviewRequired, "the product name matches more than one catalogued product") {
			return ValidationVerdict{results, decision, reason}
		}
	case !ctx.ProductResolved:
		if !record(GateProductIdentity, GateRejected, "no catalogued product matches this release") {
			return ValidationVerdict{results, decision, reason}
		}
	default:
		record(GateProductIdentity, GatePassed, "resolved to a single product")
	}

	// 2. Version present.
	if c.Version.IsZero() || strings.TrimSpace(c.Version.Normalized()) == "" {
		if !record(GateVersionPresent, GateRejected, "version is empty after normalisation") {
			return ValidationVerdict{results, decision, reason}
		}
	} else {
		record(GateVersionPresent, GatePassed, "version present: "+c.Version.Raw())
	}

	// 3. Source eligibility, including compliance.
	if !ctx.SourceEligible {
		if !record(GateSourceEligible, GateRejected, "source is not eligible for collection or is not active") {
			return ValidationVerdict{results, decision, reason}
		}
	} else {
		record(GateSourceEligible, GatePassed, "source is active and permitted")
	}

	// 4. Exact duplicate. Not an error: the correct response is to refresh the
	// existing release's last-verified timestamp rather than publish a second row.
	if ctx.DuplicateReleaseID != "" {
		if !record(GateDuplicate, GateRejected, "already published as "+ctx.DuplicateReleaseID+"; refreshing verification instead") {
			return ValidationVerdict{results, decision, reason}
		}
	} else {
		record(GateDuplicate, GatePassed, "no existing release with this product, version and channel")
	}

	// 5. Date validity.
	switch {
	case !c.ReleaseDate.Known():
		record(GateDateValid, GatePassed, "no release date published; stored as unknown precision")
	case !ctx.EarliestPlausibleDate.IsZero() && c.ReleaseDate.Anchor().Before(ctx.EarliestPlausibleDate):
		if !record(GateDateValid, GateRejected, "release date precedes the earliest plausible date for this vendor") {
			return ValidationVerdict{results, decision, reason}
		}
	case c.ReleaseDate.AfterInstant(ctx.Now.Add(ctx.FutureDateTolerance)):
		record(GateDateValid, GateReviewRequired, "release date is further in the future than the tolerance allows")
	default:
		record(GateDateValid, GatePassed, "release date "+c.ReleaseDate.String()+" at "+string(c.ReleaseDate.Precision())+" precision")
	}

	// 6. Version transition plausibility.
	pr := AssessTransition(ctx.PreviousVersion, c.Version)
	switch pr.Verdict {
	case TransitionSuspicious, TransitionUncomparable:
		record(GateVersionTransition, GateReviewRequired, pr.Reason)
	default:
		record(GateVersionTransition, GatePassed, pr.Reason)
	}

	// 7. Evidence retained.
	if !ctx.EvidencePresent || strings.TrimSpace(c.EvidenceID) == "" {
		if !record(GateEvidenceRetained, GateRejected, "no evidence artifact or excerpt was retained") {
			return ValidationVerdict{results, decision, reason}
		}
	} else {
		record(GateEvidenceRetained, GatePassed, "evidence retained")
	}

	// 8. Confidence threshold. Non-official sources never publish automatically.
	threshold := ctx.ConfidenceThreshold
	if threshold <= 0 {
		threshold = DefaultConfidenceThreshold
	}
	switch {
	case !ctx.SourceOfficial:
		record(GateConfidenceThreshold, GateReviewRequired, "source is not an official manufacturer source; human review required")
	case c.Confidence < threshold:
		record(GateConfidenceThreshold, GateReviewRequired, "confidence below the automatic publication threshold")
	default:
		record(GateConfidenceThreshold, GatePassed, "confidence meets the threshold")
	}

	// 9. Applicability constraints.
	if !ctx.ApplicabilityKnown {
		record(GateApplicability, GateReviewRequired, "hardware revision or region is not recognised for this product")
	} else {
		record(GateApplicability, GatePassed, "applicability constraints are consistent with the product")
	}

	// 10. Multi-source agreement. A disagreement between sources of equal authority is
	// never auto-resolved; one this source outranks is recorded and passed.
	switch {
	case len(ctx.ConflictingSourceVersions) > 0:
		record(GateMultiSourceAgree, GateReviewRequired,
			"other active sources report a different version: "+strings.Join(ctx.ConflictingSourceVersions, ", "))
	case len(ctx.OutrankedSourceVersions) > 0:
		record(GateMultiSourceAgree, GatePassed,
			"a lower-authority source reports a different version ("+
				strings.Join(ctx.OutrankedSourceVersions, ", ")+"); the higher-authority value stands")
	default:
		record(GateMultiSourceAgree, GatePassed, "no disagreement among active sources")
	}

	return ValidationVerdict{Results: results, Decision: decision, Reason: reason}
}
