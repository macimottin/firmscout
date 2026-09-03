package collectors_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/macimottin/firmscout/internal/adapters/collectors"
	"github.com/macimottin/firmscout/internal/domain"
)

// validConfig is the baseline every rejection case below mutates one field of, so
// that a test failing tells you which field was rejected rather than that "some
// config was invalid".
const validConfig = `
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata:
  id: example.changelog
  vendor: mikrotik
  version: 1
spec:
  engine: html_selectors
  product_match:
    product: mikrotik-routeros
  extract:
    release_container: "div.entry"
    fields:
      version:
        selector: "span.version"
        transform: trim
      release_date:
        selector: ":scope"
        regex: "(\\d{4}-\\d{2}-\\d{2})"
        date_format: "2006-01-02"
        precision: exact_day
    release_type: embedded_os
`

func TestLoadConfigAcceptsAValidDocument(t *testing.T) {
	cfg, err := collectors.LoadConfig([]byte(validConfig))
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if cfg.Metadata.ID != "example.changelog" {
		t.Errorf("id: got %q", cfg.Metadata.ID)
	}
	if cfg.CollectorVersion() != "1" {
		t.Errorf("collector version: got %q", cfg.CollectorVersion())
	}
	// Defaults are applied at load time, so nothing downstream has to guess.
	if cfg.Spec.Fetch.MaxBytes != collectors.DefaultMaxBytes {
		t.Errorf("max_bytes default: got %d", cfg.Spec.Fetch.MaxBytes)
	}
	if cfg.Spec.Fetch.Conditional != "auto" {
		t.Errorf("conditional default: got %q", cfg.Spec.Fetch.Conditional)
	}
	if cfg.Spec.Extract.Confidence.Base != collectors.DefaultConfidenceBase {
		t.Errorf("confidence base default: got %v", cfg.Spec.Extract.Confidence.Base)
	}
}

// TestLoadConfigRejects is the validation table. Every case names the field it
// expects to be blamed, because an error that does not say which line to fix is
// barely better than no error.
func TestLoadConfigRejects(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(string) string
		wantField   string
		wantMessage string
	}{
		{
			name:        "unknown engine",
			mutate:      replace("engine: html_selectors", "engine: xpath_magic"),
			wantField:   "spec.engine",
			wantMessage: "not a known engine",
		},
		{
			name:        "roadmap engine is refused as unimplemented, not as a typo",
			mutate:      replace("engine: html_selectors", "engine: json_path"),
			wantField:   "spec.engine",
			wantMessage: "reserved roadmap engine",
		},
		{
			name:        "missing release container",
			mutate:      replace("    release_container: \"div.entry\"\n", ""),
			wantField:   "spec.extract.release_container",
			wantMessage: "must be set",
		},
		{
			name:        "invalid precision value",
			mutate:      replace("precision: exact_day", "precision: to_the_minute"),
			wantField:   "spec.extract.fields.release_date.precision",
			wantMessage: "not a known precision",
		},
		{
			name:        "missing precision on a date field",
			mutate:      replace("        precision: exact_day\n", ""),
			wantField:   "spec.extract.fields.release_date.precision",
			wantMessage: "is required on a date field",
		},
		{
			// The §6 rule, enforced at load time: a day-bearing layout paired with
			// month_only would anchor every release on a day the source never
			// published.
			name: "month_only precision with a day-bearing layout",
			mutate: replace("        date_format: \"2006-01-02\"\n        precision: exact_day",
				"        date_format: \"2006-01-02\"\n        precision: month_only"),
			wantField:   "spec.extract.fields.release_date.date_format",
			wantMessage: "carries a day-of-month component",
		},
		{
			name: "exact_day precision with a month-only layout",
			mutate: replace("        date_format: \"2006-01-02\"\n        precision: exact_day",
				"        date_format: \"2006-01\"\n        precision: exact_day"),
			wantField:   "spec.extract.fields.release_date.date_format",
			wantMessage: "carries no day-of-month component",
		},
		{
			name:        "unrecognised apiVersion",
			mutate:      replace("apiVersion: firmscout.dev/v1alpha1", "apiVersion: firmscout.dev/v2"),
			wantField:   "apiVersion",
			wantMessage: "not a recognised schema version",
		},
		{
			name:        "missing version field",
			mutate:      replace("      version:\n        selector: \"span.version\"\n        transform: trim\n", ""),
			wantField:   "spec.extract.fields.version",
			wantMessage: "is required",
		},
		{
			name:        "unknown field name",
			mutate:      replace("      version:", "      verison:\n        selector: \"span.v\"\n      version:"),
			wantField:   "spec.extract.fields.verison",
			wantMessage: "not a field any engine populates",
		},
		{
			name:        "invalid release type",
			mutate:      replace("release_type: embedded_os", "release_type: firmware_ish"),
			wantField:   "spec.extract.release_type",
			wantMessage: "not part of the release-type vocabulary",
		},
		{
			name:        "invalid CSS selector",
			mutate:      replace("release_container: \"div.entry\"", "release_container: \"div[unclosed\""),
			wantField:   "spec.extract.release_container",
			wantMessage: "invalid CSS selector",
		},
		{
			name:        "product and family both set",
			mutate:      replace("    product: mikrotik-routeros", "    product: mikrotik-routeros\n    family: mikrotik-routers"),
			wantField:   "spec.product_match",
			wantMessage: "never both",
		},
		{
			name:        "neither product nor family",
			mutate:      replace("  product_match:\n    product: mikrotik-routeros\n", "  product_match: {}\n"),
			wantField:   "spec.product_match",
			wantMessage: "one of product or family",
		},
		{
			name:        "unknown transform",
			mutate:      replace("transform: trim", "transform: [trim, titlecase]"),
			wantField:   "spec.extract.fields.version.transform[1]",
			wantMessage: "not part of the transform vocabulary",
		},
		{
			name:        "transform missing its argument",
			mutate:      replace("transform: trim", "transform: [{strip_prefix: \"\"}]"),
			wantField:   "spec.extract.fields.version.transform[0]",
			wantMessage: "requires the literal string to strip",
		},
		{
			name:        "credential header",
			mutate:      replace("  extract:", "  fetch:\n    headers:\n      Authorization: \"Bearer hunter2\"\n  extract:"),
			wantField:   "spec.fetch.headers.Authorization",
			wantMessage: "may not carry credentials",
		},
		{
			name:        "misspelled key",
			mutate:      replace("release_type: embedded_os", "release_typ: embedded_os"),
			wantField:   "",
			wantMessage: "field release_typ not found",
		},
		{
			name:        "check frequency below the politeness floor",
			mutate:      replace("    release_type: embedded_os", "    release_type: embedded_os\n  schedule:\n    check_frequency_seconds: 5"),
			wantField:   "spec.schedule.check_frequency_seconds",
			wantMessage: "at least 60 seconds",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectors.LoadConfig([]byte(tc.mutate(validConfig)))
			if err == nil {
				t.Fatalf("config was accepted but should have been rejected")
			}
			if !errors.Is(err, domain.ErrValidation) {
				t.Errorf("error does not classify as a validation failure: %v", err)
			}
			if tc.wantField != "" && !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error does not name the offending field %q: %v", tc.wantField, err)
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Errorf("error message %q does not contain %q", err.Error(), tc.wantMessage)
			}
		})
	}
}

// TestFixedChannelAndChannelFieldConflict is its own test because expressing it as a
// string mutation of the baseline is unreadable.
func TestFixedChannelAndChannelFieldConflict(t *testing.T) {
	const conflicting = `
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata: {id: example.conflict, vendor: mikrotik, version: 1}
spec:
  engine: html_selectors
  product_match: {product: mikrotik-routeros}
  extract:
    release_container: "div.entry"
    channel: stable
    fields:
      version: {selector: "span.version"}
      channel: {selector: "span.channel"}
    release_type: embedded_os
`
	_, err := collectors.LoadConfig([]byte(conflicting))
	if err == nil {
		t.Fatal("a config setting both a fixed channel and a channel field was accepted")
	}
	if !strings.Contains(err.Error(), "spec.extract.channel") {
		t.Errorf("error does not name spec.extract.channel: %v", err)
	}
}

// TestTextRegexFieldSelectorMustNameARealGroup: a selector naming a capture group
// that does not exist would silently produce an empty field forever.
func TestTextRegexFieldSelectorMustNameARealGroup(t *testing.T) {
	const bad = `
apiVersion: firmscout.dev/v1alpha1
kind: CollectorConfig
metadata: {id: example.text, vendor: mikrotik, version: 1}
spec:
  engine: text_regex
  product_match: {product: mikrotik-routeros}
  extract:
    release_container: "(?P<version>\\S+) (?P<epoch>\\d+)"
    fields:
      version: {selector: verison}
    release_type: embedded_os
`
	_, err := collectors.LoadConfig([]byte(bad))
	if err == nil {
		t.Fatal("a field naming a nonexistent capture group was accepted")
	}
	if !strings.Contains(err.Error(), "named capture group") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

// TestLoadDirLoadsTheShippedConfigs is the check that keeps the checked-in configs
// honest: they are loaded and validated by the same code production uses.
func TestLoadDirLoadsTheShippedConfigs(t *testing.T) {
	configs, err := collectors.LoadDir(os.DirFS(repoRoot), configRoot)
	if err != nil {
		t.Fatalf("shipped configs do not load: %v", err)
	}
	if len(configs) < 2 {
		t.Fatalf("want at least the two MikroTik configs, got %d", len(configs))
	}

	byID := make(map[string]collectors.Config, len(configs))
	for _, cfg := range configs {
		byID[cfg.Metadata.ID] = cfg
		if cfg.Metadata.Vendor == "" {
			t.Errorf("%s declares no vendor", cfg.Path)
		}
	}
	for _, id := range []string{"mikrotik.changelogs", "mikrotik.newest-stable"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("config %q was not loaded", id)
		}
	}
	// The commented template is documentation with placeholder values, not a config
	// anyone wants registered.
	for _, cfg := range configs {
		if strings.Contains(cfg.Path, "TEMPLATE") {
			t.Errorf("TEMPLATE.yaml was loaded as a config: %s", cfg.Path)
		}
	}
}

func replace(old, new string) func(string) string {
	return func(s string) string {
		out := strings.Replace(s, old, new, 1)
		if out == s {
			panic("test mutation did not apply: " + old)
		}
		return out
	}
}
