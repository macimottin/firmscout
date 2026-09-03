// Package registry loads FirmScout's Git-managed registry from YAML.
//
// The registry is the curated half of the hybrid dataset described in ADR-0016:
// vendors, product families, products, aliases and sources live in reviewable files,
// while observed facts live only in PostgreSQL. This package is the adapter behind
// application.RegistryLoader; it knows what YAML is so that the sync use case does not.
//
// The documents it parses are validated in CI against packages/schemas/*.json. That
// validation is a convenience for contributors, not a security boundary: this loader
// re-checks everything it depends on, because a file can reach a running system
// without passing through CI.
package registry

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// APIVersion is the only registry document version this loader accepts. A future
// breaking change to the document shape bumps it, so an old file fails loudly rather
// than being silently misread.
const APIVersion = "firmscout.dev/v1alpha1"

// Loader reads registry documents from a filesystem.
type Loader struct {
	fsys fs.FS
	root string
}

var _ application.RegistryLoader = (*Loader)(nil)

// New returns a loader reading from root within fsys.
func New(fsys fs.FS, root string) *Loader {
	if root == "" {
		root = "."
	}
	return &Loader{fsys: fsys, root: root}
}

// envelope is the common head of every registry document, read first so that a file
// can be dispatched to the right decoder and an unknown kind reported by name.
type envelope struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

// Load reads and parses every YAML document under the root.
//
// Files are processed in sorted order so that a run is reproducible and a diff of the
// output is stable. Parsing is strict: an unknown field is an error rather than a
// silent no-op, because a misspelled key that is quietly ignored produces a source
// that looks configured and behaves differently.
func (l *Loader) Load(ctx context.Context) ([]application.RegistryDocument, error) {
	var paths []string
	err := fs.WalkDir(l.fsys, l.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := path.Ext(p); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk registry at %s: %w", l.root, err)
	}
	sort.Strings(paths)

	docs := make([]application.RegistryDocument, 0, len(paths))
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := fs.ReadFile(l.fsys, p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		parsed, err := parseFile(p, raw)
		if err != nil {
			return nil, err
		}
		docs = append(docs, parsed...)
	}
	return docs, nil
}

// parseFile splits a file into documents. Most files hold exactly one, but a YAML
// stream may hold several, which is how the category vocabulary is written: one list
// in one reviewable file rather than a directory of near-empty ones.
func parseFile(p string, raw []byte) ([]application.RegistryDocument, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	var out []application.RegistryDocument
	for i := 0; ; i++ {
		var node yaml.Node
		err := dec.Decode(&node)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if node.Kind == 0 {
			continue // an empty document, typically a trailing separator
		}
		chunk, err := yaml.Marshal(&node)
		if err != nil {
			return nil, fmt.Errorf("%s: re-encode document %d: %w", p, i+1, err)
		}
		label := p
		if i > 0 {
			label = fmt.Sprintf("%s (document %d)", p, i+1)
		}
		doc, err := parseDocument(label, chunk)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: file contains no registry document", p)
	}
	return out, nil
}

func parseDocument(p string, raw []byte) (application.RegistryDocument, error) {
	var env envelope
	if err := yaml.Unmarshal(raw, &env); err != nil {
		return application.RegistryDocument{}, fmt.Errorf("%s: %w", p, err)
	}
	if env.APIVersion != APIVersion {
		return application.RegistryDocument{}, fmt.Errorf(
			"%s: apiVersion %q is not supported (this build reads %q)", p, env.APIVersion, APIVersion)
	}

	switch env.Kind {
	case "Vendor":
		return parseVendor(p, raw)
	case "Category":
		return parseCategory(p, raw)
	case "ProductFamily":
		return parseFamily(p, raw)
	case "Product":
		return parseProduct(p, raw)
	case "Source":
		return parseSource(p, raw)
	default:
		return application.RegistryDocument{}, fmt.Errorf(
			"%s: unknown kind %q (expected Vendor, Category, ProductFamily, Product or Source)", p, env.Kind)
	}
}

// strictUnmarshal decodes with KnownFields enabled, so a misspelled key is an error.
func strictUnmarshal(p string, raw []byte, out any) error {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	return nil
}

type vendorDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Slug string `yaml:"slug"`
	} `yaml:"metadata"`
	Spec struct {
		Name                string   `yaml:"name"`
		LegalName           string   `yaml:"legal_name"`
		HomepageURL         string   `yaml:"homepage_url"`
		SupportURL          string   `yaml:"support_url"`
		SecurityAdvisoryURL string   `yaml:"security_advisory_url"`
		CountryCode         string   `yaml:"country_code"`
		Notes               string   `yaml:"notes"`
		Aliases             []string `yaml:"aliases"`
	} `yaml:"spec"`
}

func parseVendor(p string, raw []byte) (application.RegistryDocument, error) {
	var d vendorDoc
	if err := strictUnmarshal(p, raw, &d); err != nil {
		return application.RegistryDocument{}, err
	}
	v := domain.Vendor{
		Slug:        d.Metadata.Slug,
		Name:        d.Spec.Name,
		LegalName:   d.Spec.LegalName,
		HomepageURL: d.Spec.HomepageURL,
		SupportURL:  d.Spec.SupportURL,
		CountryCode: d.Spec.CountryCode,
		Notes:       d.Spec.Notes,
		ManagedBy:   domain.ManagedByRegistry,
	}
	if err := v.Validate(); err != nil {
		return application.RegistryDocument{}, fmt.Errorf("%s: %w", p, err)
	}
	return application.RegistryDocument{Path: p, Vendor: &v}, nil
}

type categoryDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Slug string `yaml:"slug"`
	} `yaml:"metadata"`
	Spec struct {
		Name        string `yaml:"name"`
		Parent      string `yaml:"parent"`
		Description string `yaml:"description"`
	} `yaml:"spec"`
}

func parseCategory(p string, raw []byte) (application.RegistryDocument, error) {
	var d categoryDoc
	if err := strictUnmarshal(p, raw, &d); err != nil {
		return application.RegistryDocument{}, err
	}
	c := domain.Category{
		Slug:        d.Metadata.Slug,
		Name:        d.Spec.Name,
		ParentID:    d.Spec.Parent, // resolved to an id by the sync use case
		Description: d.Spec.Description,
		ManagedBy:   domain.ManagedByRegistry,
	}
	if err := c.Validate(); err != nil {
		return application.RegistryDocument{}, fmt.Errorf("%s: %w", p, err)
	}
	if c.ParentID != "" && !domain.ValidSlug(c.ParentID) {
		return application.RegistryDocument{}, fmt.Errorf(
			"%s: parent %q must be a category slug", p, c.ParentID)
	}
	return application.RegistryDocument{Path: p, Category: &c}, nil
}

type familyDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Slug   string `yaml:"slug"`
		Vendor string `yaml:"vendor"`
	} `yaml:"metadata"`
	Spec struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	} `yaml:"spec"`
}

func parseFamily(p string, raw []byte) (application.RegistryDocument, error) {
	var d familyDoc
	if err := strictUnmarshal(p, raw, &d); err != nil {
		return application.RegistryDocument{}, err
	}
	f := domain.ProductFamily{
		VendorID:    d.Metadata.Vendor, // resolved to an id by the sync use case
		Slug:        d.Metadata.Slug,
		Name:        d.Spec.Name,
		Description: d.Spec.Description,
		ManagedBy:   domain.ManagedByRegistry,
	}
	if f.VendorID == "" {
		return application.RegistryDocument{}, fmt.Errorf("%s: metadata.vendor is required", p)
	}
	return application.RegistryDocument{Path: p, Family: &f}, nil
}

type productDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Slug   string `yaml:"slug"`
		Vendor string `yaml:"vendor"`
		Family string `yaml:"family"`
	} `yaml:"metadata"`
	Spec struct {
		Name               string   `yaml:"name"`
		ModelIdentifier    string   `yaml:"model_identifier"`
		Description        string   `yaml:"description"`
		DefaultReleaseType string   `yaml:"default_release_type"`
		LifecycleStatus    string   `yaml:"lifecycle_status"`
		SecurityCritical   bool     `yaml:"security_critical"`
		PopularityScore    int      `yaml:"popularity_score"`
		CategorySlugs      []string `yaml:"category_slugs"`
		Aliases            []struct {
			Alias      string `yaml:"alias"`
			Kind       string `yaml:"kind"`
			SourceNote string `yaml:"source_note"`
		} `yaml:"aliases"`
	} `yaml:"spec"`
}

func parseProduct(p string, raw []byte) (application.RegistryDocument, error) {
	var d productDoc
	if err := strictUnmarshal(p, raw, &d); err != nil {
		return application.RegistryDocument{}, err
	}
	if d.Metadata.Vendor == "" {
		return application.RegistryDocument{}, fmt.Errorf("%s: metadata.vendor is required", p)
	}

	lifecycle := domain.LifecycleStatus(orDefault(d.Spec.LifecycleStatus, string(domain.LifecycleActive)))
	if !domain.ValidLifecycleStatus(lifecycle) {
		return application.RegistryDocument{}, fmt.Errorf(
			"%s: lifecycle_status %q is not a known status", p, d.Spec.LifecycleStatus)
	}

	prod := domain.Product{
		VendorID:         d.Metadata.Vendor,
		ProductFamilyID:  d.Metadata.Family,
		Slug:             d.Metadata.Slug,
		Name:             d.Spec.Name,
		ModelIdentifier:  d.Spec.ModelIdentifier,
		Description:      d.Spec.Description,
		LifecycleStatus:  lifecycle,
		SecurityCritical: d.Spec.SecurityCritical,
		PopularityScore:  d.Spec.PopularityScore,
		CategorySlugs:    d.Spec.CategorySlugs,
		ManagedBy:        domain.ManagedByRegistry,
	}
	if d.Spec.DefaultReleaseType != "" {
		rt := domain.ReleaseType(d.Spec.DefaultReleaseType)
		if !domain.ValidReleaseType(rt) {
			return application.RegistryDocument{}, fmt.Errorf(
				"%s: default_release_type %q is not a known release type", p, d.Spec.DefaultReleaseType)
		}
		prod.DefaultReleaseType = rt
	}
	if err := prod.Validate(); err != nil {
		return application.RegistryDocument{}, fmt.Errorf("%s: %w", p, err)
	}

	aliases := make([]domain.ProductAlias, 0, len(d.Spec.Aliases))
	for i, a := range d.Spec.Aliases {
		kind := domain.AliasKind(orDefault(a.Kind, string(domain.AliasMarketingName)))
		alias, err := domain.NewProductAlias("", "", a.Alias, kind)
		if err != nil {
			return application.RegistryDocument{}, fmt.Errorf("%s: alias %d: %w", p, i+1, err)
		}
		alias.SourceNote = a.SourceNote
		aliases = append(aliases, alias)
	}

	return application.RegistryDocument{Path: p, Product: &prod, Aliases: aliases}, nil
}

type sourceDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Slug   string `yaml:"slug"`
		Vendor string `yaml:"vendor"`
	} `yaml:"metadata"`
	Spec struct {
		Product             string   `yaml:"product"`
		Family              string   `yaml:"family"`
		CoversProducts      []string `yaml:"covers_products"`
		SourceType          string   `yaml:"source_type"`
		URL                 string   `yaml:"url"`
		Official            bool     `yaml:"official"`
		QualityClass        string   `yaml:"quality_class"`
		RobotsPolicyStatus  string   `yaml:"robots_policy_status"`
		RobotsCheckedAt     string   `yaml:"robots_checked_at"`
		TermsReviewStatus   string   `yaml:"terms_review_status"`
		TermsReviewNote     string   `yaml:"terms_review_note"`
		AuthenticationType  string   `yaml:"authentication_type"`
		Enabled             bool     `yaml:"enabled"`
		Health              string   `yaml:"health"`
		ConfidenceScore     float64  `yaml:"confidence_score"`
		CollectorID         string   `yaml:"collector_id"`
		ExpectedContentType string   `yaml:"expected_content_type"`
		CheckFrequency      int      `yaml:"check_frequency_seconds"`
		MinFrequency        int      `yaml:"min_frequency_seconds"`
		Normalize           struct {
			SectionSelector string   `yaml:"section_selector"`
			Strip           []string `yaml:"strip"`
		} `yaml:"normalize"`
		Notes string `yaml:"notes"`
	} `yaml:"spec"`
}

func parseSource(p string, raw []byte) (application.RegistryDocument, error) {
	var d sourceDoc
	if err := strictUnmarshal(p, raw, &d); err != nil {
		return application.RegistryDocument{}, err
	}
	if d.Metadata.Vendor == "" {
		return application.RegistryDocument{}, fmt.Errorf("%s: metadata.vendor is required", p)
	}

	robots := domain.RobotsPolicyStatus(orDefault(d.Spec.RobotsPolicyStatus, string(domain.RobotsUnknown)))
	switch robots {
	case domain.RobotsAllowed, domain.RobotsDisallowed, domain.RobotsUnknown, domain.RobotsNotApplicable:
	default:
		return application.RegistryDocument{}, fmt.Errorf(
			"%s: robots_policy_status %q is not a known status", p, d.Spec.RobotsPolicyStatus)
	}

	// Terms default to pending, never to approved. A file that omits the field
	// describes a source nobody has reviewed, and the safe reading of that is "do
	// not collect". See ADR-0018.
	terms := domain.TermsReviewStatus(orDefault(d.Spec.TermsReviewStatus, string(domain.TermsPending)))
	switch terms {
	case domain.TermsPending, domain.TermsApproved, domain.TermsRestricted, domain.TermsProhibited:
	default:
		return application.RegistryDocument{}, fmt.Errorf(
			"%s: terms_review_status %q is not a known status", p, d.Spec.TermsReviewStatus)
	}

	quality := domain.QualityClass(orDefault(d.Spec.QualityClass, string(domain.QualityUnknownThirdParty)))
	if !domain.ValidQualityClass(quality) {
		return application.RegistryDocument{}, fmt.Errorf(
			"%s: quality_class %q is not a known class", p, d.Spec.QualityClass)
	}

	// A source may not claim to be an official manufacturer source while pointing
	// somewhere that is not classified as one. Letting the two disagree is how a
	// third-party page comes to outrank a vendor's own.
	if d.Spec.Official && quality != domain.QualityOfficialManufacturer && quality != domain.QualityAuthorizedPortal {
		return application.RegistryDocument{}, fmt.Errorf(
			"%s: official is true but quality_class is %q; an official source must be an official_manufacturer or authorized_portal",
			p, quality)
	}

	health := domain.SourceHealth(orDefault(d.Spec.Health, string(domain.SourceDiscovered)))
	if !domain.ValidSourceHealth(health) {
		return application.RegistryDocument{}, fmt.Errorf(
			"%s: health %q is not a known state", p, d.Spec.Health)
	}

	src := domain.Source{
		VendorID:              d.Metadata.Vendor, // resolved to ids by the sync use case
		ProductID:             d.Spec.Product,
		ProductFamilyID:       d.Spec.Family,
		Slug:                  d.Metadata.Slug,
		SourceType:            domain.SourceType(d.Spec.SourceType),
		URL:                   d.Spec.URL,
		Official:              d.Spec.Official,
		QualityClass:          quality,
		RobotsPolicyStatus:    robots,
		TermsReviewStatus:     terms,
		TermsReviewNote:       d.Spec.TermsReviewNote,
		AuthenticationType:    domain.AuthenticationType(orDefault(d.Spec.AuthenticationType, string(domain.AuthNone))),
		Enabled:               d.Spec.Enabled,
		Health:                health,
		CollectorID:           d.Spec.CollectorID,
		ExpectedContentType:   d.Spec.ExpectedContentType,
		CheckFrequencySeconds: orDefaultInt(d.Spec.CheckFrequency, 86400),
		MinFrequencySeconds:   d.Spec.MinFrequency,
		Confidence:            orDefaultFloat(d.Spec.ConfidenceScore, 0.5),
		Normalize: domain.NormalizeConfig{
			SectionSelector: d.Spec.Normalize.SectionSelector,
			Strip:           d.Spec.Normalize.Strip,
		},
		ManagedBy: domain.ManagedByRegistry,
	}

	if d.Spec.RobotsCheckedAt != "" {
		when, err := parseFlexibleDate(d.Spec.RobotsCheckedAt)
		if err != nil {
			return application.RegistryDocument{}, fmt.Errorf("%s: robots_checked_at: %w", p, err)
		}
		src.RobotsCheckedAt = when
	}

	if !domain.ValidSourceType(src.SourceType) {
		return application.RegistryDocument{}, fmt.Errorf(
			"%s: source_type %q is not a known type", p, d.Spec.SourceType)
	}
	if err := src.Validate(); err != nil {
		return application.RegistryDocument{}, fmt.Errorf("%s: %w", p, err)
	}

	return application.RegistryDocument{
		Path:           p,
		Source:         &src,
		SourceProducts: d.Spec.CoversProducts,
	}, nil
}

// parseFlexibleDate accepts the handful of shapes YAML and humans produce for a plain
// date, and rejects everything else rather than guessing.
func parseFlexibleDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	layouts := []string{
		"2006-01-02",
		time.RFC3339,
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02T15:04:05Z07:00",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse %q as a date", s)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func orDefaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func orDefaultFloat(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}
