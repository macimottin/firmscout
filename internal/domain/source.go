package domain

import (
	"strings"
	"time"
)

// SourceType is the shape of a monitored location, which determines which collector
// engines can handle it and which change signals are available.
type SourceType string

const (
	SourceTypeRESTAPI             SourceType = "rest_api"
	SourceTypeGraphQLAPI          SourceType = "graphql_api"
	SourceTypeRSSAtom             SourceType = "rss_atom"
	SourceTypeXMLFeed             SourceType = "xml_feed"
	SourceTypeJSONEndpoint        SourceType = "json_endpoint"
	SourceTypeHTMLPage            SourceType = "html_page"
	SourceTypePDFReleaseNotes     SourceType = "pdf_release_notes"
	SourceTypeDownloadPortal      SourceType = "download_portal"
	SourceTypeGitHubReleases      SourceType = "github_releases"
	SourceTypeSitemap             SourceType = "sitemap"
	SourceTypeAuthenticatedPortal SourceType = "authenticated_portal"
	SourceTypeManual              SourceType = "manual"
)

// ValidSourceType reports whether t is a declared source type.
func ValidSourceType(t SourceType) bool {
	switch t {
	case SourceTypeRESTAPI, SourceTypeGraphQLAPI, SourceTypeRSSAtom, SourceTypeXMLFeed,
		SourceTypeJSONEndpoint, SourceTypeHTMLPage, SourceTypePDFReleaseNotes,
		SourceTypeDownloadPortal, SourceTypeGitHubReleases, SourceTypeSitemap,
		SourceTypeAuthenticatedPortal, SourceTypeManual:
		return true
	}
	return false
}

// QualityClass ranks a source's authority. It is the basis for conflict resolution:
// an official manufacturer source outranks a community one, and a third-party source
// never silently outranks an official one.
type QualityClass string

const (
	QualityOfficialManufacturer QualityClass = "official_manufacturer"
	QualityAuthorizedPortal     QualityClass = "authorized_portal"
	QualityVendorRepository     QualityClass = "vendor_repository"
	QualityTrustedCommunity     QualityClass = "trusted_community"
	QualityUnknownThirdParty    QualityClass = "unknown_third_party"
)

// authorityRank orders quality classes for conflict resolution. Higher wins.
var authorityRank = map[QualityClass]int{
	QualityOfficialManufacturer: 50,
	QualityAuthorizedPortal:     40,
	QualityVendorRepository:     30,
	QualityTrustedCommunity:     20,
	QualityUnknownThirdParty:    10,
}

// ValidQualityClass reports whether q is a declared quality class.
func ValidQualityClass(q QualityClass) bool {
	_, ok := authorityRank[q]
	return ok
}

// Authority returns the comparable rank of a quality class.
func (q QualityClass) Authority() int { return authorityRank[q] }

// RobotsPolicyStatus records what the host's robots.txt says about the source path for
// FirmScout's declared user agent. See ADR-0018.
type RobotsPolicyStatus string

const (
	RobotsAllowed       RobotsPolicyStatus = "allowed"
	RobotsDisallowed    RobotsPolicyStatus = "disallowed"
	RobotsUnknown       RobotsPolicyStatus = "unknown"
	RobotsNotApplicable RobotsPolicyStatus = "not_applicable"
)

// TermsReviewStatus records a human's reading of the site's terms of use.
type TermsReviewStatus string

const (
	TermsPending    TermsReviewStatus = "pending"
	TermsApproved   TermsReviewStatus = "approved"
	TermsRestricted TermsReviewStatus = "restricted"
	TermsProhibited TermsReviewStatus = "prohibited"
)

// AuthenticationType records what credentials a source demands. FirmScout never
// bypasses authentication; a source that requires credentials it does not have is
// recorded honestly and left uncollected.
type AuthenticationType string

const (
	AuthNone                AuthenticationType = "none"
	AuthAPIKey              AuthenticationType = "api_key"
	AuthAccountRequired     AuthenticationType = "account_required"
	AuthEntitlementRequired AuthenticationType = "entitlement_required"
)

// SourceHealth is the source state machine.
type SourceHealth string

const (
	SourceDiscovered             SourceHealth = "discovered"
	SourcePendingReview          SourceHealth = "pending_review"
	SourceActive                 SourceHealth = "active"
	SourceDegraded               SourceHealth = "degraded"
	SourceRateLimited            SourceHealth = "rate_limited"
	SourceAuthenticationRequired SourceHealth = "authentication_required"
	SourceBroken                 SourceHealth = "broken"
	SourceRelocated              SourceHealth = "relocated"
	SourceDisabled               SourceHealth = "disabled"
	SourceRetired                SourceHealth = "retired"
)

// sourceTransitions declares every legal edge of the source health state machine.
// A transition not listed here is impossible, which is what makes "retired sources are
// never checked again" a property of the type rather than a rule someone must remember.
var sourceTransitions = map[SourceHealth][]SourceHealth{
	SourceDiscovered:             {SourcePendingReview, SourceDisabled, SourceRetired},
	SourcePendingReview:          {SourceActive, SourceDisabled, SourceRetired},
	SourceActive:                 {SourceDegraded, SourceRateLimited, SourceAuthenticationRequired, SourceBroken, SourceRelocated, SourceDisabled, SourceRetired},
	SourceDegraded:               {SourceActive, SourceBroken, SourceRateLimited, SourceAuthenticationRequired, SourceRelocated, SourceDisabled, SourceRetired},
	SourceRateLimited:            {SourceActive, SourceDegraded, SourceBroken, SourceDisabled, SourceRetired},
	SourceAuthenticationRequired: {SourceActive, SourceDisabled, SourceRetired, SourceBroken},
	SourceBroken:                 {SourceActive, SourceDegraded, SourceRelocated, SourceDisabled, SourceRetired},
	SourceRelocated:              {SourceActive, SourceDisabled, SourceRetired},
	SourceDisabled:               {SourcePendingReview, SourceActive, SourceRetired},
	SourceRetired:                {}, // terminal
}

// ValidSourceHealth reports whether h is a declared health state.
func ValidSourceHealth(h SourceHealth) bool {
	_, ok := sourceTransitions[h]
	return ok
}

// CanTransitionTo reports whether the edge from h to next exists.
func (h SourceHealth) CanTransitionTo(next SourceHealth) bool {
	for _, allowed := range sourceTransitions[h] {
		if allowed == next {
			return true
		}
	}
	return false
}

// CheckOutcome is the result of one watcher run.
type CheckOutcome string

const (
	OutcomeUnchanged            CheckOutcome = "unchanged"
	OutcomeChanged              CheckOutcome = "changed"
	OutcomeUnavailable          CheckOutcome = "unavailable"
	OutcomeUnauthorized         CheckOutcome = "unauthorized"
	OutcomeRateLimited          CheckOutcome = "rate_limited"
	OutcomeRedirected           CheckOutcome = "redirected"
	OutcomeParserFailed         CheckOutcome = "parser_failed"
	OutcomeSuspiciousContent    CheckOutcome = "suspicious_content"
	OutcomeManualReviewRequired CheckOutcome = "manual_review_required"
)

// ValidCheckOutcome reports whether o is a declared outcome.
func ValidCheckOutcome(o CheckOutcome) bool {
	switch o {
	case OutcomeUnchanged, OutcomeChanged, OutcomeUnavailable, OutcomeUnauthorized,
		OutcomeRateLimited, OutcomeRedirected, OutcomeParserFailed,
		OutcomeSuspiciousContent, OutcomeManualReviewRequired:
		return true
	}
	return false
}

// ChangeSignal names the mechanism that decided whether a source changed, cheapest
// first. Recording it makes the cost profile of the whole fleet measurable.
type ChangeSignal string

const (
	SignalWebhook        ChangeSignal = "webhook"
	SignalFeed           ChangeSignal = "feed"
	SignalAPICursor      ChangeSignal = "api_cursor"
	SignalETag           ChangeSignal = "etag"
	SignalLastModified   ChangeSignal = "last_modified"
	SignalConditionalGet ChangeSignal = "conditional_get"
	SignalSitemapLastmod ChangeSignal = "sitemap_lastmod"
	SignalSectionHash    ChangeSignal = "section_hash"
	SignalContentHash    ChangeSignal = "content_hash"
	SignalFullCompare    ChangeSignal = "full_compare"
)

// NormalizeConfig declares how a source's content is reduced to a stable hash.
//
// The MikroTik changelog page is the worked example: it is roughly 409 KB, carries
// "cache-control: private" and serves no ETag, so hashing the whole document would
// report a change on every check. Hashing only the elements matched by
// SectionSelector -- the changelog entries themselves -- is stable across the page's
// volatile furniture.
type NormalizeConfig struct {
	// SectionSelector limits hashing to the matching elements. Empty means hash the
	// whole normalised document.
	SectionSelector string
	// Strip removes matching elements before normalisation: scripts, styles, cookie
	// banners, advertisement blocks, personalisation panels and any element whose
	// attributes are randomised per request.
	Strip []string
}

// Source is a monitored location together with its authority, its compliance status,
// its collection configuration and its change-detection state.
type Source struct {
	ID              string
	VendorID        string
	ProductID       string // empty when the source covers a family or a whole catalogue
	ProductFamilyID string // empty when the source covers a single product or a catalogue
	Slug            string
	SourceType      SourceType
	URL             string

	// Authority and compliance. These gate dispatch; see Dispatchable.
	Official           bool
	QualityClass       QualityClass
	RobotsPolicyStatus RobotsPolicyStatus
	RobotsCheckedAt    time.Time
	TermsReviewStatus  TermsReviewStatus
	TermsReviewNote    string
	AuthenticationType AuthenticationType
	Enabled            bool

	// Collection configuration.
	CollectorID           string
	ExpectedContentType   string
	CheckFrequencySeconds int
	MinFrequencySeconds   int
	// Normalize controls what is stripped before hashing and which part of the
	// document the hash covers. Getting this wrong is the difference between a
	// source that reports a change once a month and one that reports a change on
	// every single check.
	Normalize NormalizeConfig

	// Change-detection state, rewritten at the end of every check.
	ETag                  string
	LastModifiedValue     string
	NormalizedContentHash string
	LastCheckedAt         time.Time
	LastChangedAt         time.Time
	LastSuccessAt         time.Time
	NextCheckAt           time.Time
	ConsecutiveFailures   int
	ConsecutiveUnchanged  int
	RetryAfterUntil       time.Time

	Health              SourceHealth
	RelocatedToSourceID string
	Confidence          float64
	CreatedBy           string
	ApprovedBy          string

	// ManagedBy distinguishes rows owned by the Git registry, which `firmscout
	// registry sync` overwrites, from rows created through an administrative path.
	ManagedBy    ManagedBy
	RegistryPath string
}

// Validate checks a source's invariants.
func (s Source) Validate() error {
	if strings.TrimSpace(s.VendorID) == "" {
		return invalid("source.vendor_id", "must be set")
	}
	if !ValidSlug(s.Slug) {
		return invalid("source.slug", "must be a lowercase kebab-case identifier")
	}
	if !ValidSourceType(s.SourceType) {
		return invalid("source.source_type", string(s.SourceType)+" is not a known source type")
	}
	if strings.TrimSpace(s.URL) == "" {
		return invalid("source.url", "must not be empty")
	}
	if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
		return invalid("source.url", "must use http or https")
	}
	if s.ProductID != "" && s.ProductFamilyID != "" {
		return invalid("source.scope", "a source targets a product or a family, never both")
	}
	if !ValidQualityClass(s.QualityClass) {
		return invalid("source.quality_class", string(s.QualityClass)+" is not a known quality class")
	}
	if !ValidSourceHealth(s.Health) {
		return invalid("source.health", string(s.Health)+" is not a known health state")
	}
	if s.CheckFrequencySeconds < 60 {
		return invalid("source.check_frequency_seconds", "must be at least 60")
	}
	if s.Confidence < 0 || s.Confidence > 1 {
		return invalid("source.confidence", "must be between 0 and 1")
	}
	return nil
}

// CompliancePermitsCollection reports whether the recorded robots and terms status
// allow FirmScout to fetch this source at all.
//
// This is the rule from ADR-0018 expressed in one place. It is checked before dispatch
// and again before fetching, because a source's status can change between the two.
func (s Source) CompliancePermitsCollection() bool {
	switch s.RobotsPolicyStatus {
	case RobotsAllowed, RobotsNotApplicable:
	default:
		return false
	}
	switch s.TermsReviewStatus {
	case TermsApproved, TermsRestricted:
		return true
	default:
		return false
	}
}

// Dispatchable reports whether the scheduler may enqueue a check for this source at
// time now. It mirrors the predicate of the sources_dispatchable_idx partial index, so
// the Go rule and the SQL rule cannot drift apart silently.
func (s Source) Dispatchable(now time.Time) bool {
	if !s.Enabled {
		return false
	}
	if s.Health != SourceActive && s.Health != SourceDegraded {
		return false
	}
	if !s.CompliancePermitsCollection() {
		return false
	}
	if !s.RetryAfterUntil.IsZero() && now.Before(s.RetryAfterUntil) {
		return false
	}
	return !now.Before(s.NextCheckAt)
}

// TransitionHealth moves the source to the next health state, rejecting edges that do
// not exist in the state machine.
func (s *Source) TransitionHealth(next SourceHealth) error {
	if !ValidSourceHealth(next) {
		return invalid("source.health", string(next)+" is not a known health state")
	}
	if s.Health == next {
		return nil
	}
	if !s.Health.CanTransitionTo(next) {
		return &TransitionError{Entity: "source", From: string(s.Health), To: string(next)}
	}
	s.Health = next
	return nil
}

// HealthForOutcome maps a check outcome and the resulting failure streak onto the
// health state the source should move to. It returns the current state when no change
// is warranted.
//
// The thresholds are deliberately forgiving: a single failure degrades rather than
// breaks a source, because transient network errors are common and marking a healthy
// vendor page broken creates review noise that trains maintainers to ignore the queue.
func HealthForOutcome(current SourceHealth, outcome CheckOutcome, consecutiveFailures int) SourceHealth {
	switch outcome {
	case OutcomeUnchanged, OutcomeChanged:
		if current == SourceDegraded || current == SourceRateLimited || current == SourceBroken {
			return SourceActive
		}
		return current
	case OutcomeUnauthorized:
		return SourceAuthenticationRequired
	case OutcomeRateLimited:
		return SourceRateLimited
	case OutcomeRedirected:
		// A redirect is not itself a failure; the fetcher follows it. A redirect
		// recorded as an outcome means it left the registered host, which needs a
		// human decision about whether the source moved.
		return SourceRelocated
	case OutcomeUnavailable, OutcomeParserFailed:
		if consecutiveFailures >= brokenFailureThreshold {
			return SourceBroken
		}
		return SourceDegraded
	case OutcomeSuspiciousContent, OutcomeManualReviewRequired:
		return SourceDegraded
	default:
		return current
	}
}

// brokenFailureThreshold is the number of consecutive failures after which a source is
// considered broken rather than merely degraded.
const brokenFailureThreshold = 3
