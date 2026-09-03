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

// EndOfLife reports whether the product has reached a state where new releases are not
// expected. Scheduling uses this to check such products far less often.
func (p Product) EndOfLife() bool {
	return p.LifecycleStatus == LifecycleEOL || p.LifecycleStatus == LifecycleEOS
}
