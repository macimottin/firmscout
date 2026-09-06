package domain

import (
	"strings"
	"time"
)

// ManagedBy records whether a registry row is owned by the Git registry (and therefore
// overwritten by `firmscout registry sync`) or created through an administrative path.
// See ADR-0016.
type ManagedBy string

const (
	ManagedByRegistry ManagedBy = "registry"
	ManagedByAdmin    ManagedBy = "admin"
)

// Vendor is a manufacturer or publisher.
type Vendor struct {
	ID           string
	Slug         string
	Name         string
	LegalName    string
	HomepageURL  string
	SupportURL   string
	CountryCode  string
	Notes        string
	ManagedBy    ManagedBy
	RegistryPath string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Validate checks a vendor's invariants.
func (v Vendor) Validate() error {
	if !ValidSlug(v.Slug) {
		return invalid("vendor.slug", "must be a lowercase kebab-case identifier")
	}
	if strings.TrimSpace(v.Name) == "" {
		return invalid("vendor.name", "must not be empty")
	}
	return nil
}

// Category classifies products into the areas a user browses by: networking,
// firewalls, servers, storage, meeting-room devices and so on.
//
// Categories are a controlled vocabulary declared in the registry, not values created
// on demand from whatever a product file happens to reference. That distinction
// matters: auto-creating a category on first use means a typo silently becomes a new
// category, and the browse experience quietly fragments.
type Category struct {
	ID           string
	Slug         string
	Name         string
	ParentID     string
	Description  string
	ManagedBy    ManagedBy
	RegistryPath string
}

// Validate checks a category's invariants.
func (c Category) Validate() error {
	if !ValidSlug(c.Slug) {
		return invalid("category.slug", "must be a lowercase kebab-case identifier")
	}
	if strings.TrimSpace(c.Name) == "" {
		return invalid("category.name", "must not be empty")
	}
	if c.ParentID != "" && c.ParentID == c.ID {
		return invalid("category.parent", "a category cannot be its own parent")
	}
	return nil
}

// ProductFamily groups products that share a release stream, such as a switch series
// whose members all run the same firmware image.
type ProductFamily struct {
	ID           string
	VendorID     string
	Slug         string
	Name         string
	Description  string
	ManagedBy    ManagedBy
	RegistryPath string
}

// Validate checks a family's invariants.
func (f ProductFamily) Validate() error {
	if strings.TrimSpace(f.VendorID) == "" {
		return invalid("product_family.vendor_id", "must be set")
	}
	if !ValidSlug(f.Slug) {
		return invalid("product_family.slug", "must be a lowercase kebab-case identifier")
	}
	if strings.TrimSpace(f.Name) == "" {
		return invalid("product_family.name", "must not be empty")
	}
	return nil
}

// AliasKind records why a product is also known by another name. The kind matters for
// matching: a model number is a strong signal, a common misspelling is a weak one.
type AliasKind string

const (
	AliasMarketingName     AliasKind = "marketing_name"
	AliasModelNumber       AliasKind = "model_number"
	AliasSKU               AliasKind = "sku"
	AliasRegionalName      AliasKind = "regional_name"
	AliasLegacyName        AliasKind = "legacy_name"
	AliasVendorInternal    AliasKind = "vendor_internal"
	AliasCommonMisspelling AliasKind = "common_misspelling"
)

// ValidAliasKind reports whether k is a declared alias kind.
func ValidAliasKind(k AliasKind) bool {
	switch k {
	case AliasMarketingName, AliasModelNumber, AliasSKU, AliasRegionalName,
		AliasLegacyName, AliasVendorInternal, AliasCommonMisspelling:
		return true
	}
	return false
}

// ProductAlias is another name the same product is known by.
type ProductAlias struct {
	ID              string
	ProductID       string
	Alias           string
	NormalizedAlias string
	Kind            AliasKind
	SourceNote      string
}

// NewProductAlias builds an alias, computing the normalised comparison form.
func NewProductAlias(id, productID, alias string, kind AliasKind) (ProductAlias, error) {
	trimmed := strings.TrimSpace(alias)
	if trimmed == "" {
		return ProductAlias{}, invalid("alias", "must not be empty")
	}
	if !ValidAliasKind(kind) {
		return ProductAlias{}, invalid("alias.kind", string(kind)+" is not a known alias kind")
	}
	normalized := NormalizeAlias(trimmed)
	if normalized == "" {
		return ProductAlias{}, invalid("alias", "normalises to an empty string")
	}
	return ProductAlias{
		ID:              id,
		ProductID:       productID,
		Alias:           trimmed,
		NormalizedAlias: normalized,
		Kind:            kind,
	}, nil
}

// RelationKind names why one product points at another.
//
// The vocabulary has exactly one member on purpose. "This device runs that operating
// system" is the only product-to-product edge FirmScout has measured evidence for;
// containment, succession and bundling are plausible and unevidenced, and a vocabulary
// invented ahead of its data is a set of columns nobody can populate honestly. The
// database CHECK is written IN ('runs_os') so widening it is one word.
type RelationKind string

const (
	// RelationRunsOS records that a hardware model runs a named operating system. It
	// is a navigation claim -- "the firmware for this box is published over there" --
	// and deliberately not an applicability claim: which release of that OS applies to
	// this exact model is a separate question this edge does not answer. See ADR-0024.
	RelationRunsOS RelationKind = "runs_os"
)

// ValidRelationKind reports whether k is a declared relation kind.
func ValidRelationKind(k RelationKind) bool {
	switch k {
	case RelationRunsOS:
		return true
	}
	return false
}

// ProductRelationship is a directed edge between two products.
//
// It is registry-managed and provenanced the way every other registry row is -- by the
// reviewable YAML file named in RegistryPath and by Git history -- rather than by an
// evidence_id. Evidence rows describe what a collector fetched, with a content hash and
// a collector version; an edge a human asserted in a pull request has neither, and
// minting a collector-shaped row for it would be a weaker provenance mechanism wearing
// the stronger one's clothes. See ADR-0016 and ADR-0024.
type ProductRelationship struct {
	ID            string
	FromProductID string
	ToProductID   string
	Kind          RelationKind
	// SourceNote records where the assertion came from, in the same free-text shape
	// ProductAlias.SourceNote uses: the URL fetched, when it was fetched, and the
	// excerpt that says it.
	SourceNote   string
	ManagedBy    ManagedBy
	RegistryPath string
}

// Validate checks a relationship's invariants.
func (r ProductRelationship) Validate() error {
	if strings.TrimSpace(r.FromProductID) == "" {
		return invalid("product_relationship.from_product_id", "must be set")
	}
	if strings.TrimSpace(r.ToProductID) == "" {
		return invalid("product_relationship.to_product_id", "must be set")
	}
	if !ValidRelationKind(r.Kind) {
		return invalid("product_relationship.kind", string(r.Kind)+" is not a known relation kind")
	}
	if r.FromProductID == r.ToProductID {
		return invalid("product_relationship.to_product_id", "a product cannot run itself")
	}
	return nil
}

// Product is an exact model or a software product with an independently released
// version stream.
type Product struct {
	ID                 string
	VendorID           string
	ProductFamilyID    string
	Slug               string
	Name               string
	ModelIdentifier    string
	Description        string
	DefaultReleaseType ReleaseType
	LifecycleStatus    LifecycleStatus
	PopularityScore    int
	SecurityCritical   bool
	CategorySlugs      []string
	ManagedBy          ManagedBy
	RegistryPath       string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Validate checks a product's invariants.
func (p Product) Validate() error {
	if strings.TrimSpace(p.VendorID) == "" {
		return invalid("product.vendor_id", "must be set")
	}
	if !ValidSlug(p.Slug) {
		return invalid("product.slug", "must be a lowercase kebab-case identifier")
	}
	if strings.TrimSpace(p.Name) == "" {
		return invalid("product.name", "must not be empty")
	}
	if p.DefaultReleaseType != "" && !ValidReleaseType(p.DefaultReleaseType) {
		return invalid("product.default_release_type", string(p.DefaultReleaseType)+" is not a known release type")
	}
	if !ValidLifecycleStatus(p.LifecycleStatus) {
		return invalid("product.lifecycle_status", string(p.LifecycleStatus)+" is not a known lifecycle status")
	}
	return nil
}

// IsHardwareModel reports whether this product names a physical device.
//
// A set model identifier is the whole test, and it is deliberately not exclusive with
// having a release stream: a rack server is a device and also publishes its own BIOS
// versions, so a product can be both. Callers that need "a device with no releases of
// its own" ask the summary's release count, not this.
func (p Product) IsHardwareModel() bool {
	return strings.TrimSpace(p.ModelIdentifier) != ""
}

// EndOfLife reports whether the product has reached a state where new releases are not
// expected. Scheduling uses this to check such products far less often.
func (p Product) EndOfLife() bool {
	return p.LifecycleStatus == LifecycleEOL || p.LifecycleStatus == LifecycleEOS
}
