package domain

import "strconv"

// SLAClass is the response-time band a review item is placed in. The values match the
// review_items.sla_class CHECK constraint.
type SLAClass string

const (
	SLAUrgent   SLAClass = "urgent"
	SLAHigh     SLAClass = "high"
	SLAStandard SLAClass = "standard"
	SLALow      SLAClass = "low"
)

// ValidSLAClass reports whether c is a declared SLA class.
func ValidSLAClass(c SLAClass) bool {
	switch c {
	case SLAUrgent, SLAHigh, SLAStandard, SLALow:
		return true
	}
	return false
}

// ReviewSignals are the facts scoring is allowed to use.
//
// It is a struct of already-gathered values rather than a set of repository handles
// because scoring is a policy, and a policy that can do I/O is a policy nobody can test
// in a microsecond. Two inputs the design diagram names are absent on purpose: paid
// customer dependency (no subscription-to-product data exists) and blast radius (a
// candidate affects exactly one product). Inventing numbers for either would make the
// score look better informed than it is.
type ReviewSignals struct {
	// Gate is the gate that routed the item to review.
	Gate ValidationGate
	// SourceQuality drives the authority term. It replaces the old binary "official"
	// flag, which could not tell an authorised portal from an unknown third party.
	SourceQuality  QualityClass
	SourceOfficial bool
	Confidence     float64
	// ProductPopularity is products.popularity_score. Its contribution is capped so
	// one very popular product cannot flatten every other signal.
	ProductPopularity       int
	ProductSecurityCritical bool
	ReleaseType             ReleaseType
	// ConflictUnresolved reports that this item is a multi-source disagreement the
	// authority ladder could not settle.
	ConflictUnresolved bool
}

// ReviewPriority is the computed score, its band, and why.
type ReviewPriority struct {
	Score int
	SLA   SLAClass
	// Reasons names each term that contributed, so a queue can explain its own order
	// instead of presenting an unexplained integer.
	Reasons []string
}

// Scoring weights. They are constants rather than literals in the formula so that
// tuning them is a diff a reviewer can read, and so a test can assert a band boundary
// without hardcoding the same number twice.
const (
	ReviewScoreBase                = 100
	ReviewScoreSecurityCritical    = 150
	ReviewScoreUnresolvedConflict  = 100
	ReviewScoreSecurityReleaseType = 50
	ReviewPopularityCap            = 100
	ReviewConfidenceWeight         = 100
	// ReviewHighPopularity is the popularity at or above which a security-critical
	// item is treated as urgent rather than high.
	ReviewHighPopularity = 50
)

// ScoreReview computes a review item's priority and SLA class.
//
// The formula is deliberately additive and explainable: a maintainer who disagrees with
// the queue's order must be able to read one function and say which term is wrong.
// Age is not a term. A decaying score would need a periodic re-scoring job over the
// whole queue to reproduce what the queue index already does -- it orders by
// (priority_score DESC, created_at), so the oldest item in a band is already first.
func ScoreReview(s ReviewSignals) ReviewPriority {
	p := ReviewPriority{Score: ReviewScoreBase}
	p.Reasons = append(p.Reasons, "base priority (+"+strconv.Itoa(ReviewScoreBase)+")")

	if rank := s.SourceQuality.Authority(); rank > 0 {
		p.Score += rank
		p.Reasons = append(p.Reasons, "source authority "+string(s.SourceQuality)+" (+"+strconv.Itoa(rank)+")")
	}

	// Confidence is truncated, not rounded: a term derived from a float should never
	// be able to hand out a point the extraction did not earn.
	if conf := int(clampUnit(s.Confidence) * ReviewConfidenceWeight); conf > 0 {
		p.Score += conf
		p.Reasons = append(p.Reasons, "extraction confidence (+"+strconv.Itoa(conf)+")")
	}

	if pop := clampInt(s.ProductPopularity, 0, ReviewPopularityCap); pop > 0 {
		p.Score += pop
		p.Reasons = append(p.Reasons, "product popularity (+"+strconv.Itoa(pop)+")")
	}

	if s.ProductSecurityCritical {
		p.Score += ReviewScoreSecurityCritical
		p.Reasons = append(p.Reasons, "security-critical product (+"+strconv.Itoa(ReviewScoreSecurityCritical)+")")
	}

	if s.ConflictUnresolved {
		p.Score += ReviewScoreUnresolvedConflict
		p.Reasons = append(p.Reasons, "unresolved multi-source conflict (+"+strconv.Itoa(ReviewScoreUnresolvedConflict)+")")
	}

	if s.ReleaseType == ReleaseTypeSecurityUpdate || s.ReleaseType == ReleaseTypeAdvisory {
		p.Score += ReviewScoreSecurityReleaseType
		p.Reasons = append(p.Reasons, "security-related release type "+string(s.ReleaseType)+" (+"+strconv.Itoa(ReviewScoreSecurityReleaseType)+")")
	}

	p.SLA = slaFor(s)
	return p
}

// slaFor bands the item. The bands are the design diagram's, restated against the
// signals that actually exist: an unresolved disagreement about a security-critical
// product is the one case where a wrong published answer is both likely and dangerous,
// which is what "urgent" is reserved for.
func slaFor(s ReviewSignals) SLAClass {
	switch {
	case s.ProductSecurityCritical && (s.ConflictUnresolved || s.ProductPopularity >= ReviewHighPopularity):
		return SLAUrgent
	case s.ConflictUnresolved || s.ProductSecurityCritical:
		return SLAHigh
	case s.SourceQuality.Authority() >= QualityAuthorizedPortal.Authority():
		return SLAStandard
	default:
		return SLALow
	}
}

func clampUnit(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

func clampInt(n, lo, hi int) int {
	switch {
	case n < lo:
		return lo
	case n > hi:
		return hi
	default:
		return n
	}
}
