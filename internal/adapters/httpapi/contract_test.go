package httpapi

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
)

// The contract test for the class of lie this document keeps producing: a key listed as
// required that the API does not always send.
//
// It has now been fixed three times by hand -- releaseType and lastVerifiedAt and
// productCount, then category and website, then channel, product, retrievedAt and the
// two release timestamps -- and each round was found by a reviewer reading YAML against
// struct tags rather than by anything that runs. A required key is not a documentation
// nicety: a generated client types it non-optional and crashes on the first response
// that omits it, which is exactly what happened to the web app's product and search
// pages on the first device ever catalogued.
//
// So the assertion is made against real presenter output rather than against the struct
// tags, and every sample below is deliberately the WORST case -- the value with every
// optional field unset -- because a required key is a promise about the emptiest
// response the endpoint can produce, not about the richest.
//
// It does not assert the converse (that an unrequired key is sometimes omitted).
// Under-declaring understates a guarantee; over-declaring breaks a client.

// openAPISchemas is the components/schemas half of the contract, parsed as loosely as
// this test needs: a name, its required list, and nothing else.
type openAPISchemas struct {
	Components struct {
		Schemas map[string]struct {
			Required []string `yaml:"required"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

func loadOpenAPISchemas(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatalf("read %s: %v", openAPIPath, err)
	}
	var doc openAPISchemas
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", openAPIPath, err)
	}
	if len(doc.Components.Schemas) == 0 {
		t.Fatalf("%s declares no component schemas", openAPIPath)
	}
	out := map[string][]string{}
	for name, s := range doc.Components.Schemas {
		out[name] = s.Required
	}
	return out
}

// emptiestSamples pairs each schema name with the emptiest value the presenter can
// produce for it. A schema absent from this map is not checked, and the test says so by
// name rather than passing silently -- an undocumented schema is how the last round of
// this defect survived review.
func emptiestSamples(t *testing.T) map[string]any {
	t.Helper()

	// A product with nothing recorded: no category, no release type, no release, no
	// verification timestamp, no aliases, no runs edge, no conflict.
	bare := PresentProduct(application.ProductSummary{ProductSlug: "empty", ProductName: "Empty"})

	// A device, which is where family and runs come from: a family name with no slug,
	// and a runs entry that always has one.
	device := PresentProduct(deviceSummary())

	// A product with a release of its own but no channel on it, which is the case that
	// omits ReleaseSummary.channel.
	noChannel := osSummary()
	noChannel.LatestChannel = ""
	withSummary := PresentProduct(noChannel)

	// A release with no channel, no dates, no observation timestamps and no product
	// context -- the shape GET /releases/{id} serves when the mapping is gone, and the
	// shape /latest embeds, where the enclosing object already names the product.
	release := PresentRelease(domain.Release{
		ID:          "rel_01J8Z3K9QWERTYUIOPASDFGH",
		Version:     mustVersion(t, "7.24.2"),
		ReleaseType: domain.ReleaseTypeEmbeddedOS,
		ReleaseDate: domain.UnknownDate,
	}, nil)

	// A conflict whose disputing sources stated no channel: the empty string is a real
	// value here, and the key is still sent.
	conflict := presentProductConflict(&application.ProductConflictSummary{
		Versions:    []string{"6.49.18", "7.24.2"},
		SourceCount: 2,
		DetectedAt:  time.Date(2026, 9, 3, 18, 30, 0, 0, time.UTC),
	})

	return map[string]any{
		"Vendor":               PresentVendor(domain.Vendor{Slug: "mikrotik", Name: "MikroTik"}),
		"VendorSummary":        VendorRef{Slug: "mikrotik", Name: "MikroTik"},
		"VendorListResponse":   PresentVendors(nil, ""),
		"Product":              bare,
		"ProductSummary":       device.Runs[0],
		"ProductFamilySummary": *device.Family,
		"Applicability":        bare.FirmwareApplicability,
		"OwnReleases":          bare.FirmwareApplicability.OwnReleases,
		"ReleaseSummary":       *withSummary.LatestRelease,
		"Release":              release,
		"ReleaseSource":        *release.Source,
		"Evidence":             *release.Evidence,
		"Conflict":             *conflict,
		"OfficialSource":       OfficialSourceDTO{URL: "https://mikrotik.com", Kind: "html"},
		"ReleaseListResponse":  PresentReleases(nil, nil, "", PresentHistoryWindow("professional", time.Time{}, false)),
		"HistoryWindow":        PresentHistoryWindow("professional", time.Time{}, false),
		"LatestReleaseResponse": PresentLatestRelease(application.LatestReleaseResult{
			Summary: application.ProductSummary{ProductSlug: "empty", ProductName: "Empty"},
		}),
		"SearchResultItem": PresentSearchResults([]application.ProductSummary{osSummary()}, "routeros").Results[0],
		"SearchResponse":   PresentSearchResults(nil, ""),
		"Pagination":       pagination(""),
		"HealthStatus":     HealthStatus{Status: StatusOK},
		"Problem":          Problem{Type: "https://firmscout.dev/problems/not-found", Title: "Resource not found", Status: 404},
	}
}

// Schemas with no Go value to render: pure enums and the request/parameter shapes.
// Listing them is what makes the "every schema is accounted for" check below able to
// fail loudly on a new one instead of quietly skipping it.
var schemasWithNoResponseValue = map[string]bool{
	"Channel":       true,
	"DatePrecision": true,
	"SourceKind":    true,
}

func TestEveryRequiredKeyIsActuallyAlwaysSent(t *testing.T) {
	t.Parallel()
	required := loadOpenAPISchemas(t)
	samples := emptiestSamples(t)

	for name, keys := range required {
		if schemasWithNoResponseValue[name] {
			continue
		}
		sample, ok := samples[name]
		if !ok {
			t.Errorf("schema %s has no sample in emptiestSamples; its required list is unchecked", name)
			continue
		}
		body, err := json.Marshal(sample)
		if err != nil {
			t.Errorf("marshal sample for %s: %v", name, err)
			continue
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("unmarshal sample for %s: %v", name, err)
			continue
		}
		for _, key := range keys {
			if _, present := got[key]; !present {
				t.Errorf("%s lists %s.%s as required, and the emptiest response the presenter can build omits it: %s",
					openAPIPath, name, key, body)
			}
		}
	}

	// The inverse bookkeeping: a schema that exists in the document and in neither map
	// is a hole this test would otherwise not report at all.
	for name := range required {
		if _, ok := samples[name]; !ok && !schemasWithNoResponseValue[name] {
			t.Errorf("schema %s is neither sampled nor declared value-less", name)
		}
	}
}
