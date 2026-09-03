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

// ProductDTO is the `Product` schema.
type ProductDTO struct {
	Slug            string              `json:"slug"`
	Name            string              `json:"name"`
	Vendor          VendorRef           `json:"vendor"`
	Family          *ProductRef         `json:"family"`
	Category        string              `json:"category,omitempty"`
	ReleaseType     string              `json:"releaseType,omitempty"`
	LatestRelease   *ReleaseSummaryDTO  `json:"latestRelease"`
	OfficialSources []OfficialSourceDTO `json:"officialSources"`
	LastVerifiedAt  *Timestamp          `json:"lastVerifiedAt,omitempty"`
}

// PresentProduct renders a product from the precomputed summary the public site reads.
func PresentProduct(s application.ProductSummary) ProductDTO {
	dto := ProductDTO{
		Slug:   s.ProductSlug,
		Name:   s.ProductName,
		Vendor: VendorRef{Slug: s.VendorSlug, Name: s.VendorName},
		// The summary carries no source list; an empty array is the honest answer
		// and keeps the key's type stable for a consumer that iterates it.
		OfficialSources: []OfficialSourceDTO{},
		LastVerifiedAt:  timestamp(s.LastVerifiedAt),
	}
	if len(s.CategorySlugs) > 0 {
		dto.Category = s.CategorySlugs[0]
	}
	dto.ReleaseType = s.LatestReleaseType
	if s.FamilyName != "" {
		dto.Family = &ProductRef{Name: s.FamilyName}
	}
	dto.LatestRelease = presentLatestSummary(s)
	return dto
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
}

// LatestReleaseResponse is the `LatestReleaseResponse` schema.
type LatestReleaseResponse struct {
	Vendor        VendorRef  `json:"vendor"`
	Product       ProductRef `json:"product"`
	LatestRelease ReleaseDTO `json:"latestRelease"`
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
	}
	if r.CorrectsReleaseID != "" {
		id := r.CorrectsReleaseID
		dto.CorrectsReleaseID = &id
	}
	return dto
}

// PresentReleases renders a page of releases for one product.
func PresentReleases(rs []domain.Release, product *ProductRef, nextCursor string) ReleaseListResponse {
	out := ReleaseListResponse{Releases: make([]ReleaseDTO, 0, len(rs)), Pagination: pagination(nextCursor)}
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
	Type      string     `json:"type"`
	Slug      string     `json:"slug"`
	Name      string     `json:"name"`
	Vendor    *VendorRef `json:"vendor,omitempty"`
	MatchedOn string     `json:"matchedOn"`
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
			Type:      "product",
			Slug:      s.ProductSlug,
			Name:      s.ProductName,
			Vendor:    &VendorRef{Slug: s.VendorSlug, Name: s.VendorName},
			MatchedOn: matchedOn(s, needle),
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
