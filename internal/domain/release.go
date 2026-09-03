package domain

import (
	"strings"
	"time"
)

// Release is a published fact: a version FirmScout asserts a vendor released, backed
// by evidence.
//
// Releases are immutable. There is no method on this type that changes a published
// fact. Withdrawal sets flags that record a later observation about the release;
// correction creates a new Release referencing this one. See ADR-0017 and the
// append-only rule in data-model.md.
type Release struct {
	ID       string
	VendorID string

	Version     VersionString
	ReleaseType ReleaseType
	Channel     string

	ReleaseDate     PartialDate
	PublicationDate PartialDate

	FirstObservedAt time.Time
	LastVerifiedAt  time.Time
	PublishedAt     time.Time

	ReleaseNotesURL string
	Stable          *bool
	// Recommended is a pointer because "the vendor did not say" is a distinct and
	// common answer from "the vendor said no". FirmScout never fills this in by
	// picking the newest release.
	Recommended *bool

	Withdrawn       bool
	WithdrawnAt     time.Time
	WithdrawnReason string

	CorrectsReleaseID     string
	SupersededByReleaseID string

	CandidateID      string
	CollectorRunID   string
	EvidenceID       string
	SourceConfidence float64
	ApprovedBy       string
	CreatedAt        time.Time
}

// Validate checks the invariants of a release. A release that fails this must never
// reach the releases table.
func (r Release) Validate() error {
	if strings.TrimSpace(r.VendorID) == "" {
		return invalid("release.vendor_id", "must be set")
	}
	if r.Version.IsZero() {
		return invalid("release.version", "must not be empty")
	}
	if !ValidReleaseType(r.ReleaseType) {
		return invalid("release.release_type", string(r.ReleaseType)+" is not a known release type")
	}
	if r.ReleaseType == ReleaseTypeUnknown {
		return invalid("release.release_type", "a published release must have a determined type")
	}
	if strings.TrimSpace(r.EvidenceID) == "" {
		return invalid("release.evidence_id", "a published fact must cite evidence")
	}
	if r.Withdrawn && r.WithdrawnAt.IsZero() {
		return invalid("release.withdrawn_at", "a withdrawn release must record when")
	}
	if !r.Withdrawn && !r.WithdrawnAt.IsZero() {
		return invalid("release.withdrawn_at", "set on a release that is not withdrawn")
	}
	if r.SourceConfidence < 0 || r.SourceConfidence > 1 {
		return invalid("release.source_confidence", "must be between 0 and 1")
	}
	return nil
}

// IsCorrection reports whether this release exists to correct an earlier one.
func (r Release) IsCorrection() bool { return r.CorrectsReleaseID != "" }

// Superseded reports whether a later release replaced this one.
func (r Release) Superseded() bool { return r.SupersededByReleaseID != "" }

// Serveable reports whether the release should appear as a current fact in public
// responses. Withdrawn releases remain queryable by identifier and in history, but they
// are not offered as the answer to "what is the latest version".
func (r Release) Serveable() bool { return !r.Withdrawn }

// ReleaseProductMapping records that a release applies to a product or family, under
// the given constraints.
type ReleaseProductMapping struct {
	ID              string
	ReleaseID       string
	ProductID       string
	ProductFamilyID string
	Applicability   Applicability
	// IsLatestObserved is a derived flag, not a fact. It marks which release is
	// currently the newest observed one for this product and channel, and is
	// maintained by the publication use case under a partial unique index.
	IsLatestObserved bool
	CreatedAt        time.Time
}

// Validate checks a mapping's invariants.
func (m ReleaseProductMapping) Validate() error {
	if strings.TrimSpace(m.ReleaseID) == "" {
		return invalid("release_mapping.release_id", "must be set")
	}
	hasProduct := strings.TrimSpace(m.ProductID) != ""
	hasFamily := strings.TrimSpace(m.ProductFamilyID) != ""
	if hasProduct == hasFamily {
		return invalid("release_mapping.target", "must reference exactly one of product or family")
	}
	return nil
}

// LatestComparison decides which of two releases is the later observation for a
// product and channel.
//
// It never compares version strings. The ordering is: known release date first, then
// first-observed timestamp as the tie-break. A release with an unknown date is only
// latest if nothing with a known date exists, because an undated observation is weaker
// evidence of recency than a dated one.
func LatestComparison(a, b Release) int {
	aKnown, bKnown := a.ReleaseDate.Known(), b.ReleaseDate.Known()
	switch {
	case aKnown && !bKnown:
		return 1
	case !aKnown && bKnown:
		return -1
	case aKnown && bKnown:
		if a.ReleaseDate.Before(b.ReleaseDate) {
			return -1
		}
		if b.ReleaseDate.Before(a.ReleaseDate) {
			return 1
		}
	}
	switch {
	case a.FirstObservedAt.Before(b.FirstObservedAt):
		return -1
	case b.FirstObservedAt.Before(a.FirstObservedAt):
		return 1
	default:
		return 0
	}
}

// Withdraw returns a copy of the release marked withdrawn. It returns a copy rather
// than mutating in place to keep the append-only discipline visible at the call site:
// the caller must persist the new value deliberately.
func (r Release) Withdraw(at time.Time, reason string) (Release, error) {
	if r.Withdrawn {
		return r, invalid("release.withdrawn", "release is already withdrawn")
	}
	if strings.TrimSpace(reason) == "" {
		return r, invalid("release.withdrawn_reason", "a withdrawal must record why")
	}
	out := r
	out.Withdrawn = true
	out.WithdrawnAt = at
	out.WithdrawnReason = reason
	return out, nil
}
