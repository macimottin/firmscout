package domain_test

import (
	"errors"
	"testing"

	"github.com/macimottin/firmscout/internal/domain"
)

// A relationship is a claim about two different products. Every rejection below is a
// claim that would render as a product page pointing at nothing, or at itself.
func TestProductRelationshipRejectsSelfReference(t *testing.T) {
	t.Parallel()

	self := domain.ProductRelationship{
		FromProductID: "prd_a",
		ToProductID:   "prd_a",
		Kind:          domain.RelationRunsOS,
	}
	if err := self.Validate(); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a product running itself = %v, want domain.ErrValidation", err)
	}

	cases := []struct {
		name string
		rel  domain.ProductRelationship
	}{
		{
			name: "no source product",
			rel:  domain.ProductRelationship{ToProductID: "prd_os", Kind: domain.RelationRunsOS},
		},
		{
			name: "no target product",
			rel:  domain.ProductRelationship{FromProductID: "prd_device", Kind: domain.RelationRunsOS},
		},
		{
			// An unevidenced kind must not become data by being spelled in a YAML
			// file. The vocabulary widens by a decision record, not by a typo.
			name: "a kind nobody declared",
			rel: domain.ProductRelationship{
				FromProductID: "prd_device",
				ToProductID:   "prd_os",
				Kind:          domain.RelationKind("contains"),
			},
		},
		{
			name: "no kind at all",
			rel: domain.ProductRelationship{
				FromProductID: "prd_device",
				ToProductID:   "prd_os",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.rel.Validate(); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Validate() = %v, want domain.ErrValidation", err)
			}
		})
	}

	valid := domain.ProductRelationship{
		FromProductID: "prd_device",
		ToProductID:   "prd_os",
		Kind:          domain.RelationRunsOS,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a device running an operating system was rejected: %v", err)
	}
}

func TestValidRelationKind(t *testing.T) {
	t.Parallel()

	if !domain.ValidRelationKind(domain.RelationRunsOS) {
		t.Error("runs_os is not accepted as a relation kind")
	}
	// The database CHECK lists exactly this vocabulary. A kind Go accepts and
	// PostgreSQL rejects would surface as a constraint violation at sync time rather
	// than as a validation error naming the file.
	for _, k := range []domain.RelationKind{"", "contains", "succeeds", "bundled_with", "RUNS_OS"} {
		if domain.ValidRelationKind(k) {
			t.Errorf("%q was accepted as a relation kind", k)
		}
	}
}

// The device/software boundary is a question about one field, asked in one place. The
// last case is the one that matters: a rack server is a device AND publishes its own
// BIOS versions, so "is a device" must never be answered by "has no releases".
func TestIsHardwareModel(t *testing.T) {
	t.Parallel()

	device := domain.Product{
		VendorID:        "ven_mikrotik",
		Slug:            "mikrotik-crs328-24p-4s-rm",
		Name:            "CRS328-24P-4S+RM",
		ModelIdentifier: "CRS328-24P-4S+RM",
		LifecycleStatus: domain.LifecycleUnknown,
	}
	if !device.IsHardwareModel() {
		t.Error("a product with a model identifier is not reported as a hardware model")
	}

	os := domain.Product{
		VendorID:           "ven_mikrotik",
		Slug:               "mikrotik-routeros",
		Name:               "RouterOS",
		DefaultReleaseType: domain.ReleaseTypeEmbeddedOS,
		LifecycleStatus:    domain.LifecycleActive,
	}
	if os.IsHardwareModel() {
		t.Error("an operating system with no model identifier is reported as a hardware model")
	}

	blank := os
	blank.ModelIdentifier = "   "
	if blank.IsHardwareModel() {
		t.Error("whitespace was accepted as a model identifier")
	}

	both := device
	both.DefaultReleaseType = domain.ReleaseTypeBIOS
	if !both.IsHardwareModel() {
		t.Error("a device that also publishes its own release stream stopped being a device")
	}

	// A device is a product like any other: nothing about the model identifier makes
	// the rest of the product's invariants any different, and in particular an empty
	// default release type stays legal.
	if err := device.Validate(); err != nil {
		t.Fatalf("a device product failed validation: %v", err)
	}
}
