package domain

import (
	"sort"
	"strings"
	"time"
)

// ConflictState is the lifecycle of a recorded disagreement.
type ConflictState string

const (
	ConflictOpen     ConflictState = "open"
	ConflictResolved ConflictState = "resolved"
)

// ValidConflictState reports whether s is a declared conflict state.
func ValidConflictState(s ConflictState) bool {
	switch s {
	case ConflictOpen, ConflictResolved:
		return true
	}
	return false
}

// SourceObservation is what one source currently claims about one product and channel.
//
// It exists because "what does source X say is newest" is not a question
// candidate_releases can answer: that table is an append-log of every observation ever
// made, and picking the current one out of it would require ordering candidates, which
// can only be done by version string (forbidden -- ADR-0017) or by discovery time
// (which says when FirmScout looked, not which release is newer). One row per source,
// advanced only by a later observation, states the ordering rule once.
type SourceObservation struct {
	SourceID  string
	ProductID string
	// Channel is the empty string when the source states no channel. It is never
	// null-like: the row is keyed by it, and two NULLs do not conflict in a unique
	// index, which would allow two "current" rows for one source.
	Channel           string
	RawVersion        string
	NormalizedVersion string
	ReleaseDate       PartialDate
	CandidateID       string
	EvidenceID        string
	// ObservedAt is when this claim was last seen; FirstObservedAt is when the source
	// first made it.
	ObservedAt      time.Time
	FirstObservedAt time.Time

	// Authority and eligibility are read from the source registry when observations
	// are loaded, never stored on the observation row. A source that is reclassified
	// or disabled must stop counting immediately, and a denormalised copy would keep
	// counting until something rewrote it.
	QualityClass QualityClass
	Official     bool
	Eligible     bool
}

// Validate checks the invariants an observation must satisfy before it is stored.
func (o SourceObservation) Validate() error {
	if strings.TrimSpace(o.SourceID) == "" {
		return invalid("source_observation.source_id", "must be set")
	}
	if strings.TrimSpace(o.ProductID) == "" {
		return invalid("source_observation.product_id", "an observation attributed to no product is not evidence of anything")
	}
	if strings.TrimSpace(o.RawVersion) == "" {
		return invalid("source_observation.raw_version", "must not be empty")
	}
	// This mirrors the source_observations_version_not_blank constraint. A blank
	// normalised version would make every source's claim compare equal to every
	// other's, which is a conflict detector that silently reports agreement.
	if strings.TrimSpace(o.NormalizedVersion) == "" {
		return invalid("source_observation.normalized_version", "must not be blank")
	}
	if o.QualityClass != "" && !ValidQualityClass(o.QualityClass) {
		return invalid("source_observation.quality_class", string(o.QualityClass)+" is not a known quality class")
	}
	if !ValidDatePrecision(o.ReleaseDate.Precision()) {
		return invalid("source_observation.release_date_precision", "unknown precision")
	}
	if o.ObservedAt.IsZero() {
		return invalid("source_observation.observed_at", "must record when the claim was seen")
	}
	return nil
}

// LaterObservation reports whether a is a later observation of the same product than b.
//
// The ordering is the one LatestComparison uses for releases and for the same reason:
// a known release date outranks an unknown one, then the later date wins, then the
// later observation instant breaks the tie. It never compares version strings.
func LaterObservation(a, b SourceObservation) bool {
	aKnown, bKnown := a.ReleaseDate.Known(), b.ReleaseDate.Known()
	switch {
	case aKnown && !bKnown:
		return true
	case !aKnown && bKnown:
		return false
	case aKnown && bKnown:
		if b.ReleaseDate.Before(a.ReleaseDate) {
			return true
		}
		if a.ReleaseDate.Before(b.ReleaseDate) {
			return false
		}
	}
	return b.ObservedAt.Before(a.ObservedAt)
}

// ConflictStatus is what AssessSourceConflict concluded.
type ConflictStatus string

const (
	// ConflictNone: no other eligible source reports a different version.
	ConflictNone ConflictStatus = "none"
	// ConflictOutranked: other eligible sources disagree, but every one of them sits
	// strictly below this source on the authority ladder, so the higher-authority
	// value stands and the disagreement is recorded rather than surfaced.
	ConflictOutranked ConflictStatus = "outranked"
	// ConflictUnresolved: at least one disagreeing source sits at or above this
	// source's authority. This is never auto-resolved -- see ADR-0020.
	ConflictUnresolved ConflictStatus = "unresolved"
)

// ConflictVerdict is the assessment of one source's claim against every other source's.
type ConflictVerdict struct {
	Status ConflictStatus
	// ConflictingVersions are the normalised versions reported by eligible sources
	// this source does not outrank, sorted and deduplicated.
	ConflictingVersions  []string
	ConflictingSourceIDs []string
	// OutrankedVersions are the normalised versions reported by eligible sources this
	// source outranks. They are retained, never discarded: a losing observation is
	// still evidence.
	OutrankedVersions  []string
	OutrankedSourceIDs []string
	// AuthorityRank is the highest authority among the conflicting sources, or 0 when
	// there are none.
	AuthorityRank int
	Reason        string
}

// AssessSourceConflict compares what one source reports with what every other source
// reports for the same product and channel.
//
// The ladder is the one in docs/diagrams/multi-source-conflict.md: a higher-authority
// source's value stands in for a lower one, and the reverse never happens regardless of
// recency or confidence. What the diagram allows and this function deliberately does
// not do is break a same-tier tie by recency or confidence -- see ADR-0020. Two
// official sources disagreeing is the finding, not a problem to be arbitrated.
//
// The comparison is string equality on the normalised version. It is never an ordering:
// "these two sources say different things" needs no notion of which is newer, and
// version strings have no defined order (ADR-0017).
//
// others may include the subject's own previous observation and observations from
// ineligible sources; both are ignored, so the caller can pass everything it loaded.
func AssessSourceConflict(subject SourceObservation, others []SourceObservation) ConflictVerdict {
	v := ConflictVerdict{Status: ConflictNone}

	subjectRank := subject.QualityClass.Authority()
	for _, o := range others {
		// An ineligible source is one FirmScout may not collect from or whose health
		// says its content cannot be trusted right now. Its claim is not evidence, so
		// it neither raises a conflict nor loses one.
		if !o.Eligible {
			continue
		}
		if o.SourceID == "" || o.SourceID == subject.SourceID {
			continue
		}
		if strings.TrimSpace(o.NormalizedVersion) == "" {
			continue
		}
		if o.NormalizedVersion == subject.NormalizedVersion {
			continue
		}
		if o.QualityClass.Authority() < subjectRank {
			v.OutrankedVersions = append(v.OutrankedVersions, o.NormalizedVersion)
			v.OutrankedSourceIDs = append(v.OutrankedSourceIDs, o.SourceID)
			continue
		}
		v.ConflictingVersions = append(v.ConflictingVersions, o.NormalizedVersion)
		v.ConflictingSourceIDs = append(v.ConflictingSourceIDs, o.SourceID)
		if r := o.QualityClass.Authority(); r > v.AuthorityRank {
			v.AuthorityRank = r
		}
	}

	v.ConflictingVersions = sortedSet(v.ConflictingVersions)
	v.ConflictingSourceIDs = sortedSet(v.ConflictingSourceIDs)
	v.OutrankedVersions = sortedSet(v.OutrankedVersions)
	v.OutrankedSourceIDs = sortedSet(v.OutrankedSourceIDs)

	switch {
	case len(v.ConflictingVersions) > 0:
		v.Status = ConflictUnresolved
		v.Reason = "sources FirmScout does not outrank report " + strings.Join(v.ConflictingVersions, ", ") +
			" where this source reports " + subject.NormalizedVersion
	case len(v.OutrankedVersions) > 0:
		v.Status = ConflictOutranked
		v.Reason = "lower-authority sources report " + strings.Join(v.OutrankedVersions, ", ") +
			"; the higher-authority value " + subject.NormalizedVersion + " stands"
	default:
		v.Status = ConflictNone
		v.Reason = "no eligible source reports a different version"
	}
	return v
}

// SourceConflict is a recorded, unresolved disagreement about one product and channel.
//
// It is a row rather than a derived boolean because the authority ladder decides
// whether a disagreement is a conflict at all, and re-deriving that in SQL would put a
// second copy of the ladder somewhere nothing tests it. It also outlives the candidate
// that triggered it, which is what a reviewer needs to resolve.
type SourceConflict struct {
	ID        string
	ProductID string
	Channel   string
	State     ConflictState
	// AuthorityRank is the rank at which the disagreement sits, so a queue can show
	// "two official sources" apart from "two community sources".
	AuthorityRank int
	// Versions and SourceIDs are the participants, sorted, including this conflict's
	// own reporting source.
	Versions     []string
	SourceIDs    []string
	ReviewItemID string
	DetectedAt   time.Time
	LastSeenAt   time.Time
	ResolvedAt   time.Time
	ResolvedBy   string
	Resolution   string
}

// Validate checks a conflict's invariants: an open conflict needs at least two versions
// from at least two sources, because a disagreement with one participant is not one.
func (c SourceConflict) Validate() error {
	if strings.TrimSpace(c.ProductID) == "" {
		return invalid("source_conflict.product_id", "must be set")
	}
	if !ValidConflictState(c.State) {
		return invalid("source_conflict.state", string(c.State)+" is not a known conflict state")
	}
	if c.State == ConflictOpen {
		// The same rule as the source_conflicts_participants constraint. A one-sided
		// "conflict" is a bug in the detector, and it must fail here rather than
		// reach a product page as a warning nobody can act on.
		if len(sortedSet(c.Versions)) < 2 {
			return invalid("source_conflict.versions", "an open conflict needs at least two distinct versions")
		}
		if len(sortedSet(c.SourceIDs)) < 2 {
			return invalid("source_conflict.source_ids", "an open conflict needs at least two distinct sources")
		}
		if !c.ResolvedAt.IsZero() {
			return invalid("source_conflict.resolved_at", "set on a conflict that is still open")
		}
	}
	if c.State == ConflictResolved && c.ResolvedAt.IsZero() {
		return invalid("source_conflict.resolved_at", "a resolved conflict must record when")
	}
	if c.DetectedAt.IsZero() {
		return invalid("source_conflict.detected_at", "must record when the disagreement was found")
	}
	return nil
}

// Resolve returns a copy of the conflict closed by a named actor.
//
// It returns a copy rather than mutating, for the reason Release.Withdraw does: the
// caller has to persist the new value deliberately, so a resolution can never be a
// side effect of reading one.
func (c SourceConflict) Resolve(at time.Time, resolvedBy, resolution string) (SourceConflict, error) {
	if c.State == ConflictResolved {
		return c, invalid("source_conflict.state", "conflict is already resolved")
	}
	if strings.TrimSpace(resolvedBy) == "" {
		return c, invalid("source_conflict.resolved_by", "a resolution must record who decided")
	}
	if strings.TrimSpace(resolution) == "" {
		return c, invalid("source_conflict.resolution", "a resolution must record what was decided")
	}
	out := c
	out.State = ConflictResolved
	out.ResolvedAt = at
	out.ResolvedBy = resolvedBy
	out.Resolution = resolution
	return out, nil
}

// sortedSet returns the deduplicated, sorted, blank-free form of a string slice. The
// order matters: these values reach a review item's payload and a conflict row, and a
// set whose order depends on which source happened to be read first would make two
// identical conflicts look like two different ones.
func sortedSet(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// MergeParticipants returns the sorted, deduplicated union of one value and a list,
// which is how a conflict's participant lists are built: the subject's own claim is a
// participant in the disagreement it reports, and leaving it out would record a
// conflict that names only the other side.
func MergeParticipants(own string, others []string) []string {
	return sortedSet(append([]string{own}, others...))
}
