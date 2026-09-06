package httpapi

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// This file is the wire format. Everything a consumer can see is declared here, and
// nothing else in the package writes JSON by hand.
//
// # The rule this file exists to enforce
//
// A release date is rendered at exactly the precision the vendor published and no
// finer. `releaseDate` is "2026-09-02" for exact_day, "2026-02" for month_only,
// "2026" for year_only, and the key is absent altogether for unknown --
// `releaseDatePrecision` is present in all four cases.
//
// The tempting alternative -- always emitting YYYY-MM-DD and zero-filling the unknown
// components -- is what makes a consumer's patch-scheduling tool silently treat
// "sometime in February 2026" as "1 February 2026". That is a fabricated fact with no
// basis in anything a vendor said, and it is fabricated by *us*, in this file, at the
// last possible moment before the data leaves the system. domain.PartialDate is
// specifically built so this cannot happen by accident: it exposes no Day() accessor,
// and its String() already renders at the known precision. The presenter's whole job
// is to not undo that.

// Timestamp renders an instant as RFC 3339 in UTC at second precision, which is the
// format docs/architecture/api.md §3 declares for every timestamp in the API.
type Timestamp time.Time

// MarshalJSON renders t as a quoted RFC 3339 UTC string.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	v := time.Time(t).UTC().Truncate(time.Second)
	b := make([]byte, 0, len(time.RFC3339)+2)
	b = append(b, '"')
	b = v.AppendFormat(b, time.RFC3339)
	b = append(b, '"')
	return b, nil
}

// UnmarshalJSON parses an RFC 3339 string. The presenter only ever writes these, but
// a symmetric type is what lets a contract test, a client library or a golden-file
// comparison round-trip a response without a parallel set of types.
func (t *Timestamp) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	*t = Timestamp(v)
	return nil
}

// timestamp returns a *Timestamp, or nil for the zero time so that "we do not know
// when" is rendered as an absent key rather than as the year 1.
func timestamp(t time.Time) *Timestamp {
	if t.IsZero() {
		return nil
	}
	ts := Timestamp(t)
	return &ts
}

// ---------------------------------------------------------------------------
// Shared references
// ---------------------------------------------------------------------------

// VendorRef is the {slug, name} summary embedded in other objects.
type VendorRef struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// ProductRef is the {slug, name} summary embedded in other objects.
//
// Slug carries omitempty because one of FirmScout's read models (the product family
// on a product summary) knows a name but not a slug. Emitting `"slug": ""` there
// would offer a consumer an identifier that resolves to nothing.
type ProductRef struct {
	Slug string `json:"slug,omitempty"`
	Name string `json:"name"`
}

// Pagination is the cursor envelope every list response carries. NextCursor is a
// pointer so an exhausted list renders `"nextCursor": null` rather than dropping the
// key, which is what the contract promises and what lets a consumer's loop terminate
// on an explicit null.
type Pagination struct {
	NextCursor *string `json:"nextCursor"`
}

func pagination(cursor string) Pagination {
	if cursor == "" {
		return Pagination{NextCursor: nil}
	}
	c := cursor
	return Pagination{NextCursor: &c}
}

// ---------------------------------------------------------------------------
// Vendor
// ---------------------------------------------------------------------------

// VendorDTO is the `Vendor` schema.
//
// ProductCount and LastVerifiedAt are pointers with omitempty because domain.Vendor
// carries neither: they are aggregate facts that belong to a read model the vendor
// repository does not yet expose. An absent key says "not known here"; a zero
// productCount would say "this vendor has no products", which is a different and
// usually false claim.
type VendorDTO struct {
	Slug           string     `json:"slug"`
	Name           string     `json:"name"`
	Website        string     `json:"website,omitempty"`
	ProductCount   *int       `json:"productCount,omitempty"`
	LastVerifiedAt *Timestamp `json:"lastVerifiedAt,omitempty"`
}

// VendorListResponse is the `VendorListResponse` schema.
type VendorListResponse struct {
	Vendors    []VendorDTO `json:"vendors"`
	Pagination Pagination  `json:"pagination"`
}

// PresentVendor renders one vendor.
func PresentVendor(v domain.Vendor) VendorDTO {
	return VendorDTO{
		Slug:    v.Slug,
		Name:    v.Name,
		Website: v.HomepageURL,
	}
}

// PresentVendors renders a page of vendors.
func PresentVendors(vs []domain.Vendor, nextCursor string) VendorListResponse {
	out := VendorListResponse{Vendors: make([]VendorDTO, 0, len(vs)), Pagination: pagination(nextCursor)}
	for _, v := range vs {
		out.Vendors = append(out.Vendors, PresentVendor(v))
	}
	return out
}

// ---------------------------------------------------------------------------
// Product
// ---------------------------------------------------------------------------

// ReleaseSummaryDTO is the `ReleaseSummary` schema: the compact form embedded in a
// product, not the full release.
//
// ReleaseDate uses omitempty and ReleaseDatePrecision does not. That asymmetry is the
// entire date discipline in two struct tags: the value disappears when it is not
// known, the precision never does.
type ReleaseSummaryDTO struct {
	ID                   string `json:"id"`
	RawVersion           string `json:"rawVersion"`
	Channel              string `json:"channel,omitempty"`
	ReleaseDate          string `json:"releaseDate,omitempty"`
	ReleaseDatePrecision string `json:"releaseDatePrecision"`
}

// OfficialSourceDTO is the `OfficialSource` schema.
type OfficialSourceDTO struct {
	URL      string `json:"url"`
	Kind     string `json:"kind"`
	Official bool   `json:"official"`
}

// Applicability bases. A closed vocabulary rather than a sentence, so the one place the
// sentence is written is the web client, where it can be tested as a pure function --
// the same split conflict/ConflictBanner already uses.
const (
	// ApplicabilityOwnReleases means no other product's releases are in play: this
	// product has releases mapped to itself and runs nothing FirmScout catalogues, so
	// there is no per-model question left open. For an operating system that is the
	// whole claim.
	ApplicabilityOwnReleases = "own_releases"
	// ApplicabilityRunsOSUnverified means this product runs an operating system that
	// publishes releases, and FirmScout has NOT established which of them apply to
	// this exact model.
	//
	// It says nothing about whether this product ALSO publishes releases of its own.
	// That used to be baked into this member's meaning ("publishes no release stream
	// of its own and runs one that does"), which is what made the two claims
	// inexpressible together; OwnReleasesDTO answers that half separately now.
	ApplicabilityRunsOSUnverified = "runs_os_unverified"
	// ApplicabilityNoneRecorded means neither is true yet: no releases, no runs edge.
	ApplicabilityNoneRecorded = "none_recorded"
)

// OwnReleasesDTO is the `OwnReleases` schema: whether the releases this response
// carries are mapped to this product itself, and how many there are.
//
// It is a separate member of ApplicabilityDTO rather than a value of Basis because it
// answers a different question. "These releases are this product's own" is a mapping
// fact -- a release_product_mappings row a human authored or a collector wrote -- and
// "which of the operating system's releases apply to this exact model" is a
// verification nobody has performed. A product can need both answered at once, and
// ADR-0024 names that shape as the reason the model exists: "a rack server is a device
// AND publishes its own BIOS versions, so a product can be both".
//
// Mapped is deliberately not called "verified". Its evidence is a mapping row, not a
// verification, and the word was doing exactly the work this split exists to stop --
// letting a BIOS-shaped release of the device's own vouch for an operating-system
// applicability nobody checked.
type OwnReleasesDTO struct {
	Mapped bool `json:"mapped"`
	// ReleaseCount is how many non-withdrawn releases are mapped to this product. It
	// is sent alongside Mapped, rather than instead of it, so a consumer branching on
	// the boolean never has to decide what a count of zero means.
	ReleaseCount int `json:"releaseCount"`
}

// ApplicabilityDTO is the `Applicability` schema: whether the releases shown for this
// product are known to apply to it.
//
// It carries no omitempty and is never an omitted key, for the same reason
// HasSourceConflict is always present: an absent key reads as "applies", and "we have
// not verified which firmware image this model takes" is a different claim from "every
// release of its operating system applies". See ADR-0024 and api.md §3.2.
//
// # Two claims, two members
//
// Verified and Basis answer ONE question -- "which of another product's releases apply
// to this exact model" -- and OwnReleases answers the other -- "are the releases this
// response carries this product's own". They were one scalar until a product that is
// both a hardware model and its own release stream showed what that cost: it reported
// {verified: true, basis: "own_releases"} and dropped the runs_os caveat entirely,
// because the tests were ordered "own releases first" and a single basis has room for
// only the winner. The caveat is now retracted by a verification and never by a
// mapping row; a product that has both gets both members filled in.
type ApplicabilityDTO struct {
	Verified bool   `json:"verified"`
	Basis    string `json:"basis"`
	// OwnReleases is always present, for the same reason the enclosing object is:
	// absent, it would read as "this product has none", which is a claim.
	OwnReleases OwnReleasesDTO `json:"ownReleases"`
}

// ProductDTO is the `Product` schema.
type ProductDTO struct {
	Slug   string    `json:"slug"`
	Name   string    `json:"name"`
	Vendor VendorRef `json:"vendor"`
	// Family is the `ProductFamilySummary` schema, not `ProductSummary`: it carries a
	// name and never a slug, because family slugs are unique per vendor and no route
	// resolves one. The two are separate schemas so that relaxing this one does not
	// take the slug guarantee away from Runs, whose entries exist to be followed.
	Family *ProductRef `json:"family"`
	// ModelIdentifier is the vendor's published product code, or an explicit null. It
	// is a pointer rather than an omitempty string so that a consumer can tell "this is
	// not a hardware model" from a key that simply was not sent; the `runs` array and
	// firmwareApplicability.basis disambiguate which of the two a null means.
	ModelIdentifier *string `json:"modelIdentifier"`
	// Runs lists the products this one runs -- for a device, the operating system whose
	// releases are the ones being looked for. Always an array, empty rather than null
	// when there are none, so a consumer can range over it without a check. Each entry
	// carries a real slug that resolves on this same route.
	//
	// The relation kind the read model carries is deliberately not on the wire. The
	// vocabulary has one member, so rendering it would be a constant dressed as data;
	// a second kind is a contract change that gets its own field and its own review.
	Runs []ProductRef `json:"runs"`
	// Aliases lists the other strings this product is known by -- marketing names,
	// keyboard-typeable spellings, and above all the vendor's model number, which is
	// the string a fleet inventory actually holds.
	//
	// It is always an array, empty rather than null, and it is on the wire because the
	// alias is frequently the reason the caller is on this page at all: ADR-0024 makes
	// model-number search work through a `model_number` alias rather than through a
	// column, so the hit that brought a fleet manager here was produced by a string the
	// response was not sending back. A page that then says "no aliases recorded" is
	// contradicting the search result the reader just clicked.
	//
	// The alias KIND is deliberately not on the wire, unlike the registry's own model.
	// A consumer renders these as "also known as"; distinguishing a model number from a
	// marketing name needs modelIdentifier, which is already its own key and is the
	// authoritative answer to that question -- an alias kind would be a second, weaker
	// answer to it.
	Aliases []string `json:"aliases"`
	// FirmwareApplicability is always present. See ApplicabilityDTO.
	FirmwareApplicability ApplicabilityDTO    `json:"firmwareApplicability"`
	Category              string              `json:"category,omitempty"`
	ReleaseType           string              `json:"releaseType,omitempty"`
	LatestRelease         *ReleaseSummaryDTO  `json:"latestRelease"`
	OfficialSources       []OfficialSourceDTO `json:"officialSources"`
	// HasSourceConflict reports that eligible sources disagree about this product and
	// a human has not yet resolved it. It carries no omitempty and is always present:
	// an absent key would read as "no conflict", and the difference between "we
	// checked and they agree" and "we did not check" is the whole reason ADR-0020
	// records a disagreement instead of quietly picking a winner.
	HasSourceConflict bool `json:"hasSourceConflict"`
	// Conflict is additive alongside HasSourceConflict, never a replacement for it --
	// no omitempty, so it is an explicit `null` rather than a dropped key both when
	// there is no conflict and, honestly, when there is one this package cannot yet
	// describe. See ProductConflictDTO's doc comment for the second case.
	Conflict       *ProductConflictDTO `json:"conflict"`
	LastVerifiedAt *Timestamp          `json:"lastVerifiedAt,omitempty"`
}

// PresentProduct renders a product from the precomputed summary the public site reads.
func PresentProduct(s application.ProductSummary) ProductDTO {
	dto := ProductDTO{
		Slug:              s.ProductSlug,
		Name:              s.ProductName,
		Vendor:            VendorRef{Slug: s.VendorSlug, Name: s.VendorName},
		OfficialSources:   presentOfficialSources(s),
		HasSourceConflict: s.HasSourceConflict,
		Conflict:          presentProductConflict(s.Conflict),
		LastVerifiedAt:    timestamp(s.LastVerifiedAt),
	}
	if len(s.CategorySlugs) > 0 {
		dto.Category = s.CategorySlugs[0]
	}
	dto.ReleaseType = s.LatestReleaseType
	if s.FamilyName != "" {
		dto.Family = &ProductRef{Name: s.FamilyName}
	}
	dto.LatestRelease = presentLatestSummary(s)
	dto.Runs = presentRuns(s)
	dto.Aliases = presentAliases(s)
	dto.ModelIdentifier = nullableString(s.ModelIdentifier)
	dto.FirmwareApplicability = presentApplicability(s)
	return dto
}

// presentApplicability answers the two applicability questions independently: whether
// this product's own releases are mapped to it, and whether the per-model applicability
// of an operating system's releases has been established.
//
// The runs edge is tested BEFORE the release count, which is the inversion that fixes
// the defect. Under the old order any release mapped to the product -- a rack server's
// own BIOS build, say -- outranked the runs edge and retracted the caveat, so the
// response claimed verified applicability for an operating system nobody had checked
// this model against. A caveat may only be retracted by a verification; a mapping row
// is not one, and until a collector resolves per-model applicability (ADR-0024
// "Revisit when") there is no input to this function that can retract it.
//
// The rejected alternative was to keep the single scalar and add a fourth basis member
// for "own releases and an unverified runs edge". It encodes the pair as a product
// rather than as two fields, so every future claim doubles the vocabulary, and every
// consumer switching on basis silently mishandles the new member -- including the web
// client, whose fallback branch would have shown the wrong sentence rather than none.
func presentApplicability(s application.ProductSummary) ApplicabilityDTO {
	own := OwnReleasesDTO{Mapped: s.ReleaseCount > 0, ReleaseCount: s.ReleaseCount}
	switch {
	case len(s.Runs) > 0:
		return ApplicabilityDTO{Verified: false, Basis: ApplicabilityRunsOSUnverified, OwnReleases: own}
	case own.Mapped:
		return ApplicabilityDTO{Verified: true, Basis: ApplicabilityOwnReleases, OwnReleases: own}
	default:
		return ApplicabilityDTO{Verified: false, Basis: ApplicabilityNoneRecorded, OwnReleases: own}
	}
}

// presentRuns renders the products this product runs, always as a non-nil slice.
//
// An entry with no slug is dropped rather than rendered. ProductRef.Slug is omitempty,
// so such an entry would serialise as a bare name a consumer cannot follow, and a link
// that resolves to nothing is the one thing this array exists to avoid: its whole
// purpose is telling a fleet manager where the firmware actually lives.
func presentRuns(s application.ProductSummary) []ProductRef {
	out := make([]ProductRef, 0, len(s.Runs))
	for _, r := range s.Runs {
		if r.Slug == "" {
			continue
		}
		out = append(out, ProductRef{Slug: r.Slug, Name: r.Name})
	}
	return out
}

// presentAliases renders a product's other known names, always as a non-nil slice.
//
// The raw alias text is what ships, not the normalised form: the normalised form exists
// so a lookup can match loosely, and rendering it would show a fleet manager a mangled
// version of the string they pasted. An empty entry is dropped rather than rendered --
// application.ProductSummary.Aliases comes from a database column, and an empty string
// in a list of "other names for this product" is not a name.
func presentAliases(s application.ProductSummary) []string {
	out := make([]string, 0, len(s.Aliases))
	for _, a := range s.Aliases {
		if a == "" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// nullableString returns nil for the empty string, so an unset value serialises as an
// explicit null rather than as "".
func nullableString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// presentLatestSummary renders the product's latest-release summary, or nil when the
// product has no published release yet.
func presentLatestSummary(s application.ProductSummary) *ReleaseSummaryDTO {
	if s.LatestReleaseID == "" {
		return nil
	}
	date, precision := RenderPartialDate(s.LatestReleaseDate)
	return &ReleaseSummaryDTO{
		ID:                   s.LatestReleaseID,
		RawVersion:           s.LatestRawVersion,
		Channel:              s.LatestChannel,
		ReleaseDate:          date,
		ReleaseDatePrecision: precision,
	}
}

// presentOfficialSources renders the sources that have actually contributed a
// currently-mapped, non-withdrawn release to this product -- application.ProductSummary
// carries no other kind, so there is nothing here to filter. It always returns a
// non-nil slice: an empty product genuinely has none yet, and that is an empty array,
// never a null a consumer would have to special-case before ranging over it.
func presentOfficialSources(s application.ProductSummary) []OfficialSourceDTO {
	out := make([]OfficialSourceDTO, 0, len(s.OfficialSources))
	for _, src := range s.OfficialSources {
		out = append(out, OfficialSourceDTO{
			URL: src.URL,
			// Kind already went through domain.PublicSourceKind when the summary was
			// scanned (see postgres.scanSummary); it is copied through unchanged so a
			// release's source.kind and this entry can never disagree about what
			// "kind" means for the same underlying source.
			Kind:     src.Kind,
			Official: src.Official,
		})
	}
	return out
}

// ProductConflictDTO is the `Conflict` schema: the specific disagreement behind
// hasSourceConflict, additive alongside it.
//
// hasSourceConflict answers "is something disputed"; this answers "what, exactly" --
// which channel, which versions, how many sources, and since when. It carries no
// source ids: those are internal registry identifiers, not something a public consumer
// needs to act on the disagreement.
//
// It is nil exactly when the summary genuinely has no detail to give, which is not
// only "there is no open conflict". A precomputed row written before this field
// existed, or a conflict the detector opened but has not finished recording a channel
// and versions for, also renders nil here -- deliberately, not defensively. This
// package renders whatever RefreshProductSummary actually computed and never backfills
// a placeholder object just to make Conflict and HasSourceConflict agree; the two can
// only actually agree once the row behind them does.
type ProductConflictDTO struct {
	// Channel has no omitempty: the empty string here is a real, documented value
	// ("the disputing sources stated no channel"), distinct from the key being
	// absent. omitempty would drop the key entirely for exactly that case, which
	// is indistinguishable on the wire from the field never having existed --
	// openapi.yaml's Conflict schema promises the key is always present.
	Channel     string    `json:"channel"`
	Versions    []string  `json:"versions"`
	SourceCount int       `json:"sourceCount"`
	DetectedAt  Timestamp `json:"detectedAt"`
}

// presentProductConflict renders the open-conflict detail behind a product's
// hasSourceConflict, or nil when the summary carries none. See ProductConflictDTO's
// doc comment for why "none" here is rendered honestly rather than synthesised from
// HasSourceConflict alone.
func presentProductConflict(c *application.ProductConflictSummary) *ProductConflictDTO {
	if c == nil {
		return nil
	}
	versions := make([]string, len(c.Versions))
	copy(versions, c.Versions)
	return &ProductConflictDTO{
		Channel:     c.Channel,
		Versions:    versions,
		SourceCount: c.SourceCount,
		DetectedAt:  Timestamp(c.DetectedAt),
	}
}

// ---------------------------------------------------------------------------
// Release
// ---------------------------------------------------------------------------

// EvidenceDTO is the `Evidence` schema.
type EvidenceDTO struct {
	RetrievedAt *Timestamp `json:"retrievedAt,omitempty"`
	Excerpt     string     `json:"excerpt"`
}

// ReleaseSourceDTO is the `ReleaseSource` schema.
type ReleaseSourceDTO struct {
	Official bool   `json:"official"`
	URL      string `json:"url"`
	Kind     string `json:"kind"`
}

// ReleaseDTO is the `Release` schema.
//
// Recommended is a *bool without omitempty, so it serialises as `null` when the
// vendor never designated a recommended version. That is deliberate and it is the
// second-most important rule in this file after the date discipline: `false` would
// claim the vendor said "not recommended", and substituting "the newest release" for
// a missing designation would invent a vendor recommendation out of a sort order.
type ReleaseDTO struct {
	ID                   string            `json:"id"`
	Product              *ProductRef       `json:"product,omitempty"`
	RawVersion           string            `json:"rawVersion"`
	NormalizedVersion    string            `json:"normalizedVersion"`
	ReleaseType          string            `json:"releaseType"`
	Channel              string            `json:"channel,omitempty"`
	ReleaseDate          string            `json:"releaseDate,omitempty"`
	ReleaseDatePrecision string            `json:"releaseDatePrecision"`
	Recommended          *bool             `json:"recommended"`
	Withdrawn            bool              `json:"withdrawn"`
	CorrectsReleaseID    *string           `json:"correctsReleaseId"`
	Source               *ReleaseSourceDTO `json:"source,omitempty"`
	Evidence             *EvidenceDTO      `json:"evidence,omitempty"`
	FirstObservedAt      *Timestamp        `json:"firstObservedAt,omitempty"`
	LastVerifiedAt       *Timestamp        `json:"lastVerifiedAt,omitempty"`
}

// ReleaseListResponse is the `ReleaseListResponse` schema.
type ReleaseListResponse struct {
	Releases   []ReleaseDTO `json:"releases"`
	Pagination Pagination   `json:"pagination"`
	// Window says whether this page is the whole archive or a slice of it. See
	// HistoryWindowDTO.
	Window HistoryWindowDTO `json:"window"`
}

// HistoryWindowDTO tells a consumer whether the history they are reading is complete.
//
// api.md §1 forbids hiding a field from a tier; it does not forbid windowing, and
// windowing is what ADR-0007 sells. But a consumer who cannot distinguish a windowed
// history from a complete one has been misled by omission -- they would read a catalogue
// with two years of releases as a product that shipped twice. One small object, always
// present, closes that: `windowed` is the flag, `since` is the boundary that was
// applied, and `detail` says in words how to get the rest.
type HistoryWindowDTO struct {
	Windowed bool   `json:"windowed"`
	Since    string `json:"since,omitempty"`
	Detail   string `json:"detail,omitempty"`
	// LatestOutsideWindow says that this product's newest release is older than the
	// window, so an empty page here is not a product that never shipped. Without it
	// the catalogue answers one question two ways: /latest reports a version and this
	// endpoint reports nothing, and only one of those looks like an answer.
	//
	// It carries omitempty because it is meaningless on an unwindowed response, where
	// the whole member is `{"windowed": false}` and there is no boundary for anything
	// to fall outside of.
	LatestOutsideWindow bool `json:"latestOutsideWindow,omitempty"`
}

// The sentences a windowed caller reads. Both name the plan that lifts the window,
// because a flag with no remedy attached is a dead end; the second also reconciles the
// two endpoints, because a consumer holding an empty history page and a populated
// /latest response should not have to work out which one to believe.
const (
	historyWindowDetail = "This history is windowed to the most recent " +
		"12 months. A Professional plan or above returns the complete archive."
	historyWindowLatestOutsideDetail = historyWindowDetail +
		" This product's newest release is older than the window, which is why this page " +
		"is empty; GET /api/v1/products/{slug}/latest still reports it, because the window " +
		"bounds history depth and never the current answer."
)

// PresentHistoryWindow renders the window that was applied to a page of history.
//
// The plan decides, not the emptiness of since: a Professional caller and an anonymous
// one whose window happens to predate every release in the catalogue must not produce
// the same answer, because only one of them is being told the truth about their tier.
func PresentHistoryWindow(plan string, since time.Time, latestOutside bool) HistoryWindowDTO {
	if !application.HistoryWindowed(plan) {
		return HistoryWindowDTO{Windowed: false}
	}
	dto := HistoryWindowDTO{Windowed: true, Detail: historyWindowDetail}
	if !since.IsZero() {
		dto.Since = since.UTC().Truncate(time.Second).Format(time.RFC3339)
	}
	if latestOutside {
		dto.LatestOutsideWindow = true
		dto.Detail = historyWindowLatestOutsideDetail
	}
	return dto
}

// LatestReleaseResponse is the `LatestReleaseResponse` schema.
//
// HasSourceConflict is not decoration. This is the endpoint whose entire purpose is
// answering "what version should I be on?", and answering it with a version that
// eligible sources disagree about, with no flag a consumer can check, is precisely the
// outcome ADR-0020 exists to prevent -- the disagreement is recorded rather than
// arbitrated so that it can be surfaced here. It carries no omitempty for the same
// reason the field on Product does: an absent key reads as "no conflict", and "we
// checked and they agree" is a different claim from "we did not check".
//
// RecommendedRelease is the vendor's own designation and is null when the vendor never
// made one. It is a separate member rather than a flag on latestRelease because the two
// can be different releases: the newest version is not automatically the safest, and a
// vendor that recommends an older build is making a statement FirmScout must pass on
// rather than override with a sort order.
type LatestReleaseResponse struct {
	Vendor             VendorRef   `json:"vendor"`
	Product            ProductRef  `json:"product"`
	LatestRelease      ReleaseDTO  `json:"latestRelease"`
	RecommendedRelease *ReleaseDTO `json:"recommendedRelease"`
	HasSourceConflict  bool        `json:"hasSourceConflict"`
	// Conflict is additive alongside HasSourceConflict, same meaning and same
	// nil-when-absent rule as on Product (§3.2's ProductConflictDTO).
	Conflict *ProductConflictDTO `json:"conflict"`
}

// PresentLatestRelease renders the answer to "what version should I be on?", including
// the two things that make it honest: the vendor's recommendation and whether anybody
// disagrees about the answer.
func PresentLatestRelease(result application.LatestReleaseResult) LatestReleaseResponse {
	s := result.Summary
	ref := ProductRef{Slug: s.ProductSlug, Name: s.ProductName}
	out := LatestReleaseResponse{
		Vendor:            VendorRef{Slug: s.VendorSlug, Name: s.VendorName},
		Product:           ref,
		LatestRelease:     PresentRelease(result.Release, nil),
		HasSourceConflict: result.HasConflict,
		Conflict:          presentProductConflict(result.Conflict),
	}
	if result.Recommended != nil {
		rec := PresentRelease(*result.Recommended, &ref)
		out.RecommendedRelease = &rec
	}
	return out
}

// PresentRelease renders one release. product may be nil when the caller could not
// resolve which product the release belongs to; the key is then omitted rather than
// filled with a guess.
func PresentRelease(r domain.Release, product *ProductRef) ReleaseDTO {
	date, precision := RenderPartialDate(r.ReleaseDate)
	dto := ReleaseDTO{
		ID:                   r.ID,
		Product:              product,
		RawVersion:           r.Version.Raw(),
		NormalizedVersion:    r.Version.Normalized(),
		ReleaseType:          string(r.ReleaseType),
		Channel:              r.Channel,
		ReleaseDate:          date,
		ReleaseDatePrecision: precision,
		// Copied through unchanged, nil and all. See the type comment.
		Recommended:     r.Recommended,
		Withdrawn:       r.Withdrawn,
		FirstObservedAt: timestamp(r.FirstObservedAt),
		LastVerifiedAt:  timestamp(r.LastVerifiedAt),
		// releases.evidence_id is "NOT NULL REFERENCES evidence (id) ON DELETE
		// RESTRICT" (database/migrations/00001_initial.sql): every release that
		// reaches this function through GetByID, LatestForProduct or ListForProduct
		// has a real source and real evidence behind it, joined in by scanRelease.
		// These are rendered unconditionally rather than guarded behind a nil check,
		// because a nil check here would treat a hard database invariant as an
		// optional relationship it is not -- that gap is exactly what was crashing
		// GET /releases/{id} before this field was ever set.
		Source: &ReleaseSourceDTO{
			Official: r.Source.Official,
			URL:      r.Source.URL,
			Kind:     r.Source.Kind,
		},
		Evidence: &EvidenceDTO{
			RetrievedAt: timestamp(r.Evidence.RetrievedAt),
			Excerpt:     r.Evidence.Excerpt,
		},
	}
	if r.CorrectsReleaseID != "" {
		id := r.CorrectsReleaseID
		dto.CorrectsReleaseID = &id
	}
	return dto
}

// PresentReleases renders a page of releases for one product.
func PresentReleases(rs []domain.Release, product *ProductRef, nextCursor string, window HistoryWindowDTO) ReleaseListResponse {
	out := ReleaseListResponse{
		Releases:   make([]ReleaseDTO, 0, len(rs)),
		Pagination: pagination(nextCursor),
		Window:     window,
	}
	for _, r := range rs {
		out.Releases = append(out.Releases, PresentRelease(r, product))
	}
	return out
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// Match kinds reported in a search result item.
const (
	MatchedOnName   = "name"
	MatchedOnAlias  = "alias"
	MatchedOnVendor = "vendor"
)

// SearchResultItemDTO is the `SearchResultItem` schema.
type SearchResultItemDTO struct {
	Type string `json:"type"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	// ModelIdentifier echoes the vendor's product code back on a hit, so a fleet
	// manager who pasted the string stamped on the chassis can see that this row is the
	// thing they hold. It carries omitempty, unlike the detail response's field: a
	// search result is a compact hit descriptor, and an explicit null on every software
	// product in every result set is noise rather than honesty.
	ModelIdentifier string     `json:"modelIdentifier,omitempty"`
	Vendor          *VendorRef `json:"vendor,omitempty"`
	MatchedOn       string     `json:"matchedOn"`
}

// SearchResponse is the `SearchResponse` schema.
type SearchResponse struct {
	Results    []SearchResultItemDTO `json:"results"`
	Pagination Pagination            `json:"pagination"`
}

// PresentSearchResults renders search hits, labelling each with which field the query
// most plausibly matched so a UI can show "matched alias RB4011" style context.
func PresentSearchResults(summaries []application.ProductSummary, query string) SearchResponse {
	out := SearchResponse{
		Results:    make([]SearchResultItemDTO, 0, len(summaries)),
		Pagination: pagination(""),
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	for _, s := range summaries {
		out.Results = append(out.Results, SearchResultItemDTO{
			Type:            "product",
			Slug:            s.ProductSlug,
			Name:            s.ProductName,
			ModelIdentifier: s.ModelIdentifier,
			Vendor:          &VendorRef{Slug: s.VendorSlug, Name: s.VendorName},
			MatchedOn:       matchedOn(s, needle),
		})
	}
	return out
}

// matchedOn reports which field the query most plausibly hit.
//
// The read port returns ranked rows, not the reason each one ranked, so this is a
// presentation-layer inference over data already in hand rather than a fact from the
// index. It never invents a field the row does not have, and it falls back to "name",
// which is the field every row has.
func matchedOn(s application.ProductSummary, needle string) string {
	if needle == "" {
		return MatchedOnName
	}
	if strings.Contains(strings.ToLower(s.ProductName), needle) ||
		strings.Contains(strings.ToLower(s.ProductSlug), needle) {
		return MatchedOnName
	}
	for _, a := range s.Aliases {
		if strings.Contains(strings.ToLower(a), needle) {
			return MatchedOnAlias
		}
	}
	if strings.Contains(strings.ToLower(s.VendorName), needle) ||
		strings.Contains(strings.ToLower(s.VendorSlug), needle) {
		return MatchedOnVendor
	}
	return MatchedOnName
}

// ---------------------------------------------------------------------------
// System
// ---------------------------------------------------------------------------

// HealthStatus is the `HealthStatus` schema.
type HealthStatus struct {
	Status string `json:"status"`
}

// Health statuses.
const (
	StatusOK          = "ok"
	StatusDegraded    = "degraded"
	StatusUnavailable = "unavailable"
)

// ---------------------------------------------------------------------------
// The date discipline
// ---------------------------------------------------------------------------

// RenderPartialDate returns the value to serialise as `releaseDate` and the value to
// serialise as `releaseDatePrecision`.
//
// The value is empty exactly when the precision is "unknown", which combined with
// `omitempty` on the date field and no omitempty on the precision field produces the
// four cases the contract requires. It delegates the rendering itself to
// domain.PartialDate.String(), which already truncates to the known precision -- the
// presenter must not re-derive a format from the anchor, because the anchor's day and
// month components are canonical padding, not data.
func RenderPartialDate(d domain.PartialDate) (value, precision string) {
	return d.String(), string(d.Precision())
}
