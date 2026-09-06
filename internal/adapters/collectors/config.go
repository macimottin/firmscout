// Package collectors implements FirmScout's configuration-driven collector engines.
//
// A collector config is a declarative YAML document (collectors/config/<vendor>/*.yaml)
// interpreted by a fixed engine -- html_selectors, text_regex or rss_atom in the MVP --
// into the application.Collector contract. The specification is
// docs/architecture/collector-config-spec.md; this package is its implementation.
//
// The security model the whole arrangement rests on is that a config is data, not
// code: there is no eval, no scripting field, no way to name an external program, and
// no field that can cause a second network request. The transform vocabulary is closed
// (§5 of the spec), every regex is RE2 (no backtracking, therefore no catastrophic
// backtracking class at all), and every selector is compiled once at load time so a
// typo is a loud configuration error rather than a selector that silently matches
// nothing forever.
package collectors

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/cascadia"
	"gopkg.in/yaml.v3"

	"github.com/macimottin/firmscout/internal/domain"
)

// ---------------------------------------------------------------------------
// Document shape
// ---------------------------------------------------------------------------

// APIVersionV1Alpha1 is the only schema version this loader interprets.
//
// An unrecognised apiVersion is refused outright rather than parsed best-effort: the
// whole point of versioning the schema is that a future change to field *semantics*
// must not be silently reinterpreted under the old meaning.
const APIVersionV1Alpha1 = "firmscout.dev/v1alpha1"

// KindCollectorConfig is the only kind this loader interprets.
const KindCollectorConfig = "CollectorConfig"

// Engine selects which extraction engine interprets spec.
type Engine string

// MVP engines.
const (
	EngineHTMLSelectors Engine = "html_selectors"
	EngineTextRegex     Engine = "text_regex"
	// EngineRSSAtom reads a parsed RSS 2.0 or Atom feed. See rss_atom.go for the
	// engine and the closed feed-field selector vocabulary it accepts.
	EngineRSSAtom Engine = "rss_atom"
)

// roadmapEngines are reserved names from collector-config-spec.md §4.3. They are named
// here so that a config declaring one gets "not implemented yet" rather than the same
// message as a typo -- those are different problems for the person reading the error.
var roadmapEngines = map[Engine]bool{
	"json_path": true, "xml_xpath": true,
	"pdf_text": true, "github_releases": true,
}

// Config is one parsed and validated collector configuration document.
type Config struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`

	// Path records where this config was loaded from, for error messages. It is not
	// a YAML field.
	Path string `yaml:"-"`
}

// Metadata identifies a config.
type Metadata struct {
	// ID becomes the running collector's ID(). It is recorded on every piece of
	// evidence the collector produces, so renaming it makes past extractions
	// unexplainable; a rename is a new id plus a deliberate migration.
	ID string `yaml:"id"`
	// Vendor is the vendor slug; cross-file existence under dataset/vendors/ is a
	// registry-consistency check, not something this loader can verify from one file.
	Vendor string `yaml:"vendor"`
	// Version is the config's own revision, bumped whenever a change to spec could
	// alter extraction output. It becomes the collector's Version().
	Version int `yaml:"version"`
}

// Spec is everything engine-specific.
type Spec struct {
	Engine       Engine        `yaml:"engine"`
	ProductMatch ProductMatch  `yaml:"product_match"`
	Fetch        FetchSpec     `yaml:"fetch"`
	Normalize    NormalizeSpec `yaml:"normalize"`
	Extract      ExtractSpec   `yaml:"extract"`
	Schedule     ScheduleSpec  `yaml:"schedule"`
}

// ProductMatch declares which catalogue entry this source's candidates are matched
// against. It produces a hint, never a resolved product identity: resolution is a
// validation gate in the application layer, which is where an ambiguous match can be
// routed to a human instead of guessed.
type ProductMatch struct {
	Product string `yaml:"product"`
	Family  string `yaml:"family"`
}

// FetchSpec carries the limits the fetcher and the SDK wrapper enforce. Nothing in
// this block is interpreted by an extraction engine; it is here because the config is
// the single description of how a source is handled.
type FetchSpec struct {
	ExpectedContentType string            `yaml:"expected_content_type"`
	MaxBytes            int64             `yaml:"max_bytes"`
	TimeoutSeconds      int               `yaml:"timeout_seconds"`
	Headers             map[string]string `yaml:"headers"`
	Conditional         string            `yaml:"conditional"`
}

// Fetch defaults, from collector-config-spec.md §3.3.
const (
	DefaultMaxBytes       = int64(2097152) // 2 MiB
	DefaultTimeoutSeconds = 30
	ConditionalAuto       = "auto"
)

var validConditional = map[string]bool{
	"auto": true, "etag_only": true, "last_modified_only": true, "none": true,
}

// forbiddenHeaders are request headers a config may not set. A config-driven collector
// is not permitted to carry secrets: a source that needs credentials is either out of
// scope or handled by a code collector where credential handling is reviewed Go using
// the platform's secret path, never a YAML file someone pasted a bearer token into.
var forbiddenHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true,
	"x-api-key": true, "api-key": true, "x-auth-token": true,
}

// NormalizeSpec declares how content is reduced before hashing and extraction.
type NormalizeSpec struct {
	// SectionSelector restricts change-detection hashing, and extraction in the
	// html_selectors engine, to the matching elements. Empty means the whole
	// document.
	SectionSelector string `yaml:"section_selector"`
	// Strip removes matching elements before hashing and before extraction.
	Strip []string `yaml:"strip"`
	// Replace applies bounded regex substitutions to the normalised text. The
	// text_regex engine applies these before matching; html_selectors leaves them to
	// the Normalizer port, because rewriting markup with a regex before parsing it is
	// how a selector starts matching something its author never saw.
	Replace []ReplaceRule `yaml:"replace"`

	sectionSel cascadia.Selector
	stripSel   []cascadia.Selector
}

// ReplaceRule is one bounded regex substitution.
type ReplaceRule struct {
	Pattern string `yaml:"pattern"`
	With    string `yaml:"with"`

	re *regexp.Regexp
}

// ExtractSpec is the declarative extraction rule set.
type ExtractSpec struct {
	// ReleaseContainer identifies each repeating unit that becomes one candidate: a
	// CSS selector for html_selectors, a regex for text_regex.
	ReleaseContainer string `yaml:"release_container"`
	// Fields maps a candidate field name to the rule that populates it. "version" is
	// required; every other field is optional and takes its zero value when absent.
	Fields map[string]*FieldSpec `yaml:"fields"`
	// ReleaseType is fixed for every candidate this config produces. Config-driven
	// collectors do not infer a release type per candidate: a source that genuinely
	// mixes types needs one config per type, or a code collector.
	ReleaseType string `yaml:"release_type"`
	// Channel is a fixed channel for sources with no per-candidate channel. Set at
	// most one of this and fields.channel.
	Channel string `yaml:"channel"`
	// Applicability carries static constraints merged into every candidate.
	Applicability ApplicabilitySpec `yaml:"applicability"`
	// EvidenceExcerpt selects the text that justifies each candidate. Its length is
	// bounded by the engine regardless of what it matches; a config cannot opt out.
	EvidenceExcerpt string `yaml:"evidence_excerpt"`
	// Confidence tunes the collector's self-assessment. See ConfidenceSpec.
	Confidence ConfidenceSpec `yaml:"confidence"`

	containerSel cascadia.Selector
	containerRe  *regexp.Regexp
	excerptSel   cascadia.Selector
	excerptGroup string
	excerptScope bool
}

// ApplicabilitySpec are static applicability overrides.
type ApplicabilitySpec struct {
	HardwareRevision string `yaml:"hardware_revision"`
	Region           string `yaml:"region"`
	DeploymentMode   string `yaml:"deployment_mode"`
	Note             string `yaml:"note"`
}

// ConfidenceSpec configures the confidence score a collector reports.
//
// Confidence is the collector's estimate of its own extraction reliability. It is an
// input to the validation gates, never a substitute for them: a confident collector
// does not get to skip a gate, and a diffident one does not get its candidate rejected
// without a reason.
type ConfidenceSpec struct {
	// Base is the score when every required field extracted cleanly. Default 0.9 --
	// deliberately short of 1.0, because "a selector matched" is evidence that the
	// page still looks the way it did, not proof the extracted fact is correct.
	Base float64 `yaml:"base"`
	// MissingOptionalPenalty is subtracted once per optional field that was declared
	// but produced nothing. Default 0.1.
	MissingOptionalPenalty float64 `yaml:"missing_optional_penalty"`
	// Floor bounds how far penalties can push the score. Default 0.3.
	Floor float64 `yaml:"floor"`
}

// Confidence defaults.
const (
	DefaultConfidenceBase    = 0.9
	DefaultMissingPenalty    = 0.1
	DefaultConfidenceFloor   = 0.3
	maxConfidenceUpperBound  = 1.0
	defaultEvidenceExcerptOf = ScopeSelector
)

// ScheduleSpec is the source's polling cadence as the config author understands it.
//
// It is advisory: the authoritative cadence lives on the source record in
// dataset/sources/, which is what the scheduler reads. It is carried here so a
// reviewer reading one collector config can see the intended cost profile without
// opening a second file, and so the two can be diffed for disagreement.
type ScheduleSpec struct {
	CheckFrequencySeconds int `yaml:"check_frequency_seconds"`
	MinFrequencySeconds   int `yaml:"min_frequency_seconds"`
}

// ScopeSelector refers to the matched container itself rather than a descendant of it.
// CSS has no such selector that cascadia implements, so the engines handle it before
// any selector is compiled.
const ScopeSelector = ":scope"

// FieldSpec is one field-extraction rule.
type FieldSpec struct {
	// Selector locates the raw value inside the container. For html_selectors it is
	// a CSS selector or ":scope"; for text_regex it is ":scope" (the whole match) or
	// the name of a named capture group of the container regex.
	Selector string `yaml:"selector"`
	// Attribute reads an HTML attribute instead of the element's text.
	Attribute string `yaml:"attribute"`
	// Regex is applied to the selected text; capture group 1 is kept when the
	// pattern has one, otherwise the whole match.
	Regex string `yaml:"regex"`
	// Transform is one transform name or an ordered pipeline of them.
	Transform TransformPipeline `yaml:"transform"`
	// Map translates a vendor's own label into FirmScout's vocabulary. A value with
	// no entry is a hard error for that candidate, never a pass-through: an unmapped
	// channel badge means the vendor introduced a label nobody has interpreted yet,
	// and inventing a meaning for it is how a "development" build gets published as
	// stable.
	Map map[string]string `yaml:"map"`
	// DateFormat is a Go reference-time layout, or the special value "epoch_seconds".
	DateFormat string `yaml:"date_format"`
	// Precision is the PartialDate precision this field is declared to produce. It is
	// required on a date field and is the hinge of the whole no-invented-precision
	// discipline: see §6 of the spec and the load-time consistency check below.
	Precision string `yaml:"precision"`
	// Required defaults to true. An optional field that produces nothing lowers the
	// candidate's confidence instead of discarding the candidate.
	Required *bool `yaml:"required"`

	name  string
	sel   cascadia.Selector
	scope bool
	group string
	re    *regexp.Regexp
}

// IsRequired reports whether the field must produce a value.
func (f *FieldSpec) IsRequired() bool { return f.Required == nil || *f.Required }

// EpochSecondsFormat is the date_format value meaning "the text is a Unix timestamp in
// seconds" rather than a Go layout.
const EpochSecondsFormat = "epoch_seconds"

// Field names the engines give meaning to. Any other key under spec.extract.fields is
// rejected at load time rather than silently ignored, because a field named "verison"
// that quietly does nothing is worse than a config that refuses to load.
const (
	FieldVersion           = "version"
	FieldNormalizedVersion = "normalized_version"
	FieldChannel           = "channel"
	FieldReleaseDate       = "release_date"
	FieldPublicationDate   = "publication_date"
	FieldReleaseNotesURL   = "release_notes_url"
	FieldProductHint       = "product_hint"
	FieldHardwareRevision  = "hardware_revision"
	FieldRegion            = "region"
	FieldDeploymentMode    = "deployment_mode"
)

var knownFields = map[string]bool{
	FieldVersion: true, FieldNormalizedVersion: true, FieldChannel: true,
	FieldReleaseDate: true, FieldPublicationDate: true, FieldReleaseNotesURL: true,
	FieldProductHint: true, FieldHardwareRevision: true, FieldRegion: true,
	FieldDeploymentMode: true,
}

func isDateField(name string) bool {
	return name == FieldReleaseDate || name == FieldPublicationDate
}

// ---------------------------------------------------------------------------
// Transforms
// ---------------------------------------------------------------------------

// Transform names, from collector-config-spec.md §5. The vocabulary is closed:
// extending it means adding a named, reviewed, tested transform here, never an escape
// hatch that evaluates an expression from the config.
const (
	TransformTrim         = "trim"
	TransformLowercase    = "lowercase"
	TransformUppercase    = "uppercase"
	TransformStripPrefix  = "strip_prefix"
	TransformStripSuffix  = "strip_suffix"
	TransformRegexExtract = "regex_extract"
	TransformRegexReplace = "regex_replace"
	TransformMap          = "map"
	TransformParseDate    = "parse_date"
)

// Transform is one step of a field's transform pipeline.
//
// Accepted YAML forms:
//
//	trim                                        # no argument
//	{strip_prefix: "v"}                         # single-key mapping, scalar argument
//	{regex_extract: "([0-9.]+)"}
//	{regex_replace: {pattern: "\\s+", with: " "}}
//	{name: regex_replace, pattern: "\\s+", with: " "}   # explicit form
type Transform struct {
	Name string `yaml:"name"`
	// Arg is the literal prefix/suffix for strip_prefix/strip_suffix, or the pattern
	// for regex_extract/regex_replace.
	Arg string `yaml:"pattern"`
	// With is the replacement for regex_replace.
	With string `yaml:"with"`

	re *regexp.Regexp
}

// TransformPipeline is an ordered list of transforms. YAML may give one name, one
// mapping, or a sequence of either.
type TransformPipeline []Transform

// UnmarshalYAML accepts a scalar, a mapping or a sequence.
func (p *TransformPipeline) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.SequenceNode:
		out := make(TransformPipeline, 0, len(value.Content))
		for _, n := range value.Content {
			var t Transform
			if err := t.UnmarshalYAML(n); err != nil {
				return err
			}
			out = append(out, t)
		}
		*p = out
		return nil
	case yaml.ScalarNode, yaml.MappingNode:
		var t Transform
		if err := t.UnmarshalYAML(value); err != nil {
			return err
		}
		*p = TransformPipeline{t}
		return nil
	default:
		return fmt.Errorf("transform must be a name, a mapping or a list of them")
	}
}

// UnmarshalYAML accepts the four documented forms.
func (t *Transform) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		t.Name = value.Value
		return nil
	case yaml.MappingNode:
		if len(value.Content) == 0 {
			return fmt.Errorf("transform mapping is empty")
		}
		// Explicit form: {name: ..., pattern: ..., with: ...}
		for i := 0; i+1 < len(value.Content); i += 2 {
			if value.Content[i].Value == "name" {
				type plain struct {
					Name    string `yaml:"name"`
					Pattern string `yaml:"pattern"`
					With    string `yaml:"with"`
				}
				var pl plain
				if err := value.Decode(&pl); err != nil {
					return err
				}
				t.Name, t.Arg, t.With = pl.Name, pl.Pattern, pl.With
				return nil
			}
		}
		// Single-key form: {strip_prefix: "v"} or {regex_replace: {pattern:, with:}}
		if len(value.Content) != 2 {
			return fmt.Errorf("transform mapping must have exactly one key naming the transform, or an explicit \"name\" key")
		}
		t.Name = value.Content[0].Value
		arg := value.Content[1]
		switch arg.Kind {
		case yaml.ScalarNode:
			t.Arg = arg.Value
		case yaml.MappingNode:
			type args struct {
				Pattern string `yaml:"pattern"`
				With    string `yaml:"with"`
			}
			var a args
			if err := arg.Decode(&a); err != nil {
				return err
			}
			t.Arg, t.With = a.Pattern, a.With
		default:
			return fmt.Errorf("transform %q argument must be a scalar or a mapping", t.Name)
		}
		return nil
	default:
		return fmt.Errorf("transform must be a name or a mapping")
	}
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ConfigError names the offending field, so that a contributor is told which line to
// fix rather than that "the config is invalid".
type ConfigError struct {
	Path    string // file path, when known
	Field   string // dotted field path, e.g. "spec.extract.fields.version.selector"
	Message string
}

func (e *ConfigError) Error() string {
	var b strings.Builder
	b.WriteString("collector config")
	if e.Path != "" {
		b.WriteString(" ")
		b.WriteString(e.Path)
	}
	if e.Field != "" {
		b.WriteString(": ")
		b.WriteString(e.Field)
	}
	b.WriteString(": ")
	b.WriteString(e.Message)
	return b.String()
}

// Unwrap makes every configuration failure classifiable as a validation failure.
func (e *ConfigError) Unwrap() error { return domain.ErrValidation }

func fieldErr(field, format string, args ...any) error {
	return &ConfigError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// LoadConfig parses and validates one collector configuration document.
//
// Unknown keys are rejected. A misspelled key that YAML would happily ignore is the
// most expensive kind of configuration bug: everything loads, nothing errors, and the
// field the author thought they set is simply absent for as long as nobody notices.
func LoadConfig(data []byte) (Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, &ConfigError{Message: "parse YAML: " + err.Error()}
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadDir loads every collector config under root in fsys.
//
// TEMPLATE.yaml and any file whose name begins with "_" or "." are skipped: the
// template is documentation with placeholder values, not a config anybody wants
// registered. Every failure is reported, not just the first, so one pass tells a
// contributor everything that is wrong.
func LoadDir(fsys fs.FS, root string) ([]Config, error) {
	var configs []Config
	var problems []error

	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := path.Base(p)
		if base == "TEMPLATE.yaml" || strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
			return nil
		}
		if ext := path.Ext(base); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, readErr := fs.ReadFile(fsys, p)
		if readErr != nil {
			problems = append(problems, fmt.Errorf("read %s: %w", p, readErr))
			return nil
		}
		cfg, loadErr := LoadConfig(data)
		if loadErr != nil {
			var ce *ConfigError
			if errors.As(loadErr, &ce) {
				ce.Path = p
				problems = append(problems, ce)
			} else {
				problems = append(problems, fmt.Errorf("%s: %w", p, loadErr))
			}
			return nil
		}
		cfg.Path = p
		configs = append(configs, cfg)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}

	// Deterministic order: two runs of the loader must build the registry the same
	// way, or "which collector won" becomes a function of filesystem iteration order.
	sort.Slice(configs, func(i, j int) bool { return configs[i].Metadata.ID < configs[j].Metadata.ID })
	return configs, nil
}

// CollectorVersion is the config's revision rendered as the collector's Version().
func (c Config) CollectorVersion() string { return strconv.Itoa(c.Metadata.Version) }

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func (c *Config) validate() error {
	if c.APIVersion != APIVersionV1Alpha1 {
		return fieldErr("apiVersion", "%q is not a recognised schema version (expected %q)", c.APIVersion, APIVersionV1Alpha1)
	}
	if c.Kind != KindCollectorConfig {
		return fieldErr("kind", "%q is not a recognised kind (expected %q)", c.Kind, KindCollectorConfig)
	}
	if strings.TrimSpace(c.Metadata.ID) == "" {
		return fieldErr("metadata.id", "must be set")
	}
	if strings.TrimSpace(c.Metadata.Vendor) == "" {
		return fieldErr("metadata.vendor", "must be set")
	}
	if c.Metadata.Version < 1 {
		return fieldErr("metadata.version", "must be a positive integer starting at 1, got %d", c.Metadata.Version)
	}

	switch c.Spec.Engine {
	case EngineHTMLSelectors, EngineTextRegex, EngineRSSAtom:
	case "":
		return fieldErr("spec.engine", "must be set (%s, %s or %s)", EngineHTMLSelectors, EngineTextRegex, EngineRSSAtom)
	default:
		if roadmapEngines[c.Spec.Engine] {
			return fieldErr("spec.engine", "%q is a reserved roadmap engine that the MVP loader does not implement", c.Spec.Engine)
		}
		return fieldErr("spec.engine", "%q is not a known engine (expected %s, %s or %s)", c.Spec.Engine, EngineHTMLSelectors, EngineTextRegex, EngineRSSAtom)
	}

	if err := c.validateProductMatch(); err != nil {
		return err
	}
	if err := c.validateFetch(); err != nil {
		return err
	}
	if err := c.validateNormalize(); err != nil {
		return err
	}
	if err := c.validateExtract(); err != nil {
		return err
	}
	return c.validateSchedule()
}

func (c *Config) validateProductMatch() error {
	product := strings.TrimSpace(c.Spec.ProductMatch.Product)
	family := strings.TrimSpace(c.Spec.ProductMatch.Family)
	switch {
	case product == "" && family == "":
		return fieldErr("spec.product_match", "one of product or family must be set")
	case product != "" && family != "":
		return fieldErr("spec.product_match", "set product or family, never both")
	}
	return nil
}

func (c *Config) validateFetch() error {
	f := &c.Spec.Fetch
	if f.MaxBytes == 0 {
		f.MaxBytes = DefaultMaxBytes
	}
	if f.MaxBytes < 0 {
		return fieldErr("spec.fetch.max_bytes", "must be positive, got %d", f.MaxBytes)
	}
	if f.TimeoutSeconds == 0 {
		f.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if f.TimeoutSeconds < 0 {
		return fieldErr("spec.fetch.timeout_seconds", "must be positive, got %d", f.TimeoutSeconds)
	}
	if f.Conditional == "" {
		f.Conditional = ConditionalAuto
	}
	if !validConditional[f.Conditional] {
		return fieldErr("spec.fetch.conditional", "%q is not one of auto, etag_only, last_modified_only, none", f.Conditional)
	}
	for name := range f.Headers {
		if forbiddenHeaders[strings.ToLower(strings.TrimSpace(name))] {
			return fieldErr("spec.fetch.headers."+name,
				"a config-driven collector may not carry credentials; a source needing authentication is either out of scope or a code collector")
		}
	}
	return nil
}

func (c *Config) validateNormalize() error {
	n := &c.Spec.Normalize
	if n.SectionSelector != "" {
		if c.Spec.Engine != EngineHTMLSelectors {
			return fieldErr("spec.normalize.section_selector", "is only meaningful for the %s engine", EngineHTMLSelectors)
		}
		sel, err := compileSelector(n.SectionSelector)
		if err != nil {
			return fieldErr("spec.normalize.section_selector", "%v", err)
		}
		n.sectionSel = sel
	}
	n.stripSel = n.stripSel[:0]
	for i, s := range n.Strip {
		if c.Spec.Engine != EngineHTMLSelectors {
			return fieldErr(fmt.Sprintf("spec.normalize.strip[%d]", i), "is only meaningful for the %s engine", EngineHTMLSelectors)
		}
		sel, err := compileSelector(s)
		if err != nil {
			return fieldErr(fmt.Sprintf("spec.normalize.strip[%d]", i), "%v", err)
		}
		n.stripSel = append(n.stripSel, sel)
	}
	for i := range n.Replace {
		r := &n.Replace[i]
		if c.Spec.Engine == EngineRSSAtom {
			// rewriting XML with a regex before parsing it is how a selector starts
			// matching something its author never wrote -- the same reasoning
			// section_selector and strip are refused for above, restated here
			// because replace has no engine restriction for the other two engines.
			return fieldErr(fmt.Sprintf("spec.normalize.replace[%d]", i),
				"is not supported for the %s engine: it would rewrite XML with a regex before parsing it", EngineRSSAtom)
		}
		if strings.TrimSpace(r.Pattern) == "" {
			return fieldErr(fmt.Sprintf("spec.normalize.replace[%d].pattern", i), "must be set")
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return fieldErr(fmt.Sprintf("spec.normalize.replace[%d].pattern", i), "invalid RE2 pattern: %v", err)
		}
		r.re = re
	}
	return nil
}

func (c *Config) validateExtract() error {
	e := &c.Spec.Extract

	if c.Spec.Engine == EngineRSSAtom {
		// Unlike the other two engines, the container here is a feed entry, not a
		// selector, so the key defaults rather than being required (D15,
		// collector-config-spec.md §4.4). It stays required-with-a-default rather
		// than being silently ignored: a config that says "item" and is handed an
		// Atom feed extracts nothing and warns, which is a repair signal, not
		// something to paper over.
		if strings.TrimSpace(e.ReleaseContainer) == "" {
			e.ReleaseContainer = FeedContainerAuto
		}
		switch e.ReleaseContainer {
		case FeedContainerAuto, FeedContainerItem, FeedContainerEntry:
		default:
			return fieldErr("spec.extract.release_container",
				"%q is not one of %s, %s or %s", e.ReleaseContainer, FeedContainerAuto, FeedContainerItem, FeedContainerEntry)
		}
	} else {
		if strings.TrimSpace(e.ReleaseContainer) == "" {
			return fieldErr("spec.extract.release_container", "must be set: it identifies each repeating unit that becomes one candidate")
		}
		switch c.Spec.Engine {
		case EngineHTMLSelectors:
			sel, err := compileSelector(e.ReleaseContainer)
			if err != nil {
				return fieldErr("spec.extract.release_container", "%v", err)
			}
			e.containerSel = sel
		case EngineTextRegex:
			re, err := regexp.Compile(e.ReleaseContainer)
			if err != nil {
				return fieldErr("spec.extract.release_container", "invalid RE2 pattern: %v", err)
			}
			e.containerRe = re
		}
	}

	if strings.TrimSpace(e.ReleaseType) == "" {
		return fieldErr("spec.extract.release_type", "must be set; a collector that cannot determine the type declares %q rather than guessing", domain.ReleaseTypeUnknown)
	}
	if !domain.ValidReleaseType(domain.ReleaseType(e.ReleaseType)) {
		return fieldErr("spec.extract.release_type", "%q is not part of the release-type vocabulary", e.ReleaseType)
	}

	if len(e.Fields) == 0 {
		return fieldErr("spec.extract.fields", "must declare at least the version field")
	}
	if _, ok := e.Fields[FieldVersion]; !ok {
		return fieldErr("spec.extract.fields.version", "is required: a candidate with no version cannot be stored")
	}
	if e.Channel != "" {
		if _, ok := e.Fields[FieldChannel]; ok {
			return fieldErr("spec.extract.channel",
				"set either a fixed channel here or a per-candidate fields.channel rule, never both")
		}
	}

	names := make([]string, 0, len(e.Fields))
	for name := range e.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !knownFields[name] {
			return fieldErr("spec.extract.fields."+name, "is not a field any engine populates")
		}
		if err := c.validateField(name, e.Fields[name]); err != nil {
			return err
		}
	}

	if e.EvidenceExcerpt == "" {
		e.EvidenceExcerpt = defaultEvidenceExcerptOf
	}
	switch {
	case e.EvidenceExcerpt == ScopeSelector:
		e.excerptScope = true
	case c.Spec.Engine == EngineHTMLSelectors:
		sel, err := compileSelector(e.EvidenceExcerpt)
		if err != nil {
			return fieldErr("spec.extract.evidence_excerpt", "%v", err)
		}
		e.excerptSel = sel
	case c.Spec.Engine == EngineTextRegex:
		if !hasGroup(e.containerRe, e.EvidenceExcerpt) {
			return fieldErr("spec.extract.evidence_excerpt",
				"%q is neither %q nor a named capture group of spec.extract.release_container", e.EvidenceExcerpt, ScopeSelector)
		}
		e.excerptGroup = e.EvidenceExcerpt
	case c.Spec.Engine == EngineRSSAtom:
		if !feedFieldNames[e.EvidenceExcerpt] {
			return fieldErr("spec.extract.evidence_excerpt",
				"%q is neither %q nor one of the rss_atom feed fields (%s)",
				e.EvidenceExcerpt, ScopeSelector, strings.Join(sortedFeedFieldNames(), ", "))
		}
		// Reusing excerptGroup rather than adding a parallel field: both text_regex
		// and rss_atom are "look this named thing up on the container", and giving
		// that idea two field names across two engines would be the drift this
		// package otherwise avoids.
		e.excerptGroup = e.EvidenceExcerpt
	}

	return c.validateConfidence()
}

func (c *Config) validateConfidence() error {
	conf := &c.Spec.Extract.Confidence
	if conf.Base == 0 {
		conf.Base = DefaultConfidenceBase
	}
	if conf.MissingOptionalPenalty == 0 {
		conf.MissingOptionalPenalty = DefaultMissingPenalty
	}
	if conf.Floor == 0 {
		conf.Floor = DefaultConfidenceFloor
	}
	if conf.Base <= 0 || conf.Base > maxConfidenceUpperBound {
		return fieldErr("spec.extract.confidence.base", "must be in (0, 1], got %v", conf.Base)
	}
	if conf.MissingOptionalPenalty < 0 || conf.MissingOptionalPenalty > 1 {
		return fieldErr("spec.extract.confidence.missing_optional_penalty", "must be in [0, 1], got %v", conf.MissingOptionalPenalty)
	}
	if conf.Floor < 0 || conf.Floor > conf.Base {
		return fieldErr("spec.extract.confidence.floor", "must be in [0, base], got %v", conf.Floor)
	}
	return nil
}

func (c *Config) validateField(name string, f *FieldSpec) error {
	if f == nil {
		return fieldErr("spec.extract.fields."+name, "must be a mapping")
	}
	f.name = name
	fieldPath := "spec.extract.fields." + name

	if strings.TrimSpace(f.Selector) == "" {
		return fieldErr(fieldPath+".selector", "must be set (use %q for the container itself)", ScopeSelector)
	}
	switch {
	case f.Selector == ScopeSelector:
		f.scope = true
	case c.Spec.Engine == EngineHTMLSelectors:
		sel, err := compileSelector(f.Selector)
		if err != nil {
			return fieldErr(fieldPath+".selector", "%v", err)
		}
		f.sel = sel
	case c.Spec.Engine == EngineTextRegex:
		// For text_regex a non-:scope selector names a capture group of the
		// container pattern. Verifying it exists now turns a silent empty field into
		// a load-time error.
		f.group = f.Selector
		if !hasGroup(c.Spec.Extract.containerRe, f.group) {
			return fieldErr(fieldPath+".selector",
				"%q is neither %q nor a named capture group of spec.extract.release_container", f.Selector, ScopeSelector)
		}
	case c.Spec.Engine == EngineRSSAtom:
		// The vocabulary is closed (collector-config-spec.md §4.4): a selector
		// outside it would silently match nothing, forever, on every check.
		if !feedFieldNames[f.Selector] {
			return fieldErr(fieldPath+".selector",
				"%q is not one of the rss_atom feed fields (%s) or %q",
				f.Selector, strings.Join(sortedFeedFieldNames(), ", "), ScopeSelector)
		}
	}

	if f.Attribute != "" && c.Spec.Engine != EngineHTMLSelectors {
		return fieldErr(fieldPath+".attribute", "is only meaningful for the %s engine", EngineHTMLSelectors)
	}

	if f.Regex != "" {
		re, err := regexp.Compile(f.Regex)
		if err != nil {
			return fieldErr(fieldPath+".regex", "invalid RE2 pattern: %v", err)
		}
		f.re = re
	}

	for i := range f.Transform {
		if err := validateTransform(fieldPath, i, &f.Transform[i], f, name); err != nil {
			return err
		}
	}
	if len(f.Map) == 0 && f.Map != nil {
		return fieldErr(fieldPath+".map", "is present but empty; remove it or give it entries")
	}

	return c.validateDateField(fieldPath, name, f)
}

// validateDateField implements collector-config-spec.md §6: the declared precision
// must match what the paired layout can actually determine, checked at load time,
// before any source is ever fetched with this config.
//
// A layout carrying a zone offset is accepted at every precision, month_only and
// year_only included, and that is a decision rather than an oversight. parseDate reads
// the calendar components in the offset the source published them in, so "Jan 2006
// -0700" applied to "Aug 2026 +0200" determines August 2026 exactly as unambiguously as
// a zoneless layout does; there is nothing left for this function to refuse. The
// alternative considered was rejecting such a pairing here, and it was dropped because
// it would forbid a well-defined config for no gain while leaving the exact_day path --
// which must accept offsets, every RFC 1123 feed carries one -- under a different rule
// from its neighbours. Anyone changing parseDate's zone handling must change this
// comment and this validation with it: the two agree deliberately, not by accident.
func (c *Config) validateDateField(fieldPath, name string, f *FieldSpec) error {
	if !isDateField(name) {
		if f.Precision != "" {
			return fieldErr(fieldPath+".precision", "is only meaningful on a date field")
		}
		if f.DateFormat != "" {
			return fieldErr(fieldPath+".date_format", "is only meaningful on a date field")
		}
		return nil
	}

	if f.Precision == "" {
		return fieldErr(fieldPath+".precision",
			"is required on a date field: it declares exactly how much of the date the source actually publishes")
	}
	p := domain.DatePrecision(f.Precision)
	if !domain.ValidDatePrecision(p) {
		return fieldErr(fieldPath+".precision",
			"%q is not a known precision (exact_day, month_only, year_only, unknown)", f.Precision)
	}

	if p == domain.PrecisionUnknown {
		if f.DateFormat != "" {
			return fieldErr(fieldPath+".date_format", "must not be set when the precision is unknown")
		}
		return nil
	}

	if f.DateFormat == EpochSecondsFormat {
		// An epoch is precise to the second, so any declared precision is
		// expressible from it. Declaring something coarser than exact_day is a
		// deliberate, honest statement that the vendor does not document the
		// timestamp as the release date -- the engine truncates rather than
		// promotes.
		return nil
	}

	switch p {
	case domain.PrecisionExactDay:
		if f.DateFormat == "" {
			return fieldErr(fieldPath+".date_format", "is required when the precision is exact_day")
		}
		if !layoutHasDay(f.DateFormat) {
			return fieldErr(fieldPath+".date_format",
				"layout %q carries no day-of-month component, so it cannot support precision exact_day", f.DateFormat)
		}
	case domain.PrecisionMonthOnly:
		if f.DateFormat == "" {
			return fieldErr(fieldPath+".date_format", "is required when the precision is month_only")
		}
		if layoutHasDay(f.DateFormat) {
			return fieldErr(fieldPath+".date_format",
				"layout %q carries a day-of-month component but the precision is month_only; "+
					"pairing them would anchor every release on a day the source never published", f.DateFormat)
		}
		if !layoutHasMonth(f.DateFormat) {
			return fieldErr(fieldPath+".date_format",
				"layout %q carries no month component, so it cannot support precision month_only", f.DateFormat)
		}
	case domain.PrecisionYearOnly:
		if f.DateFormat != "" && layoutHasDay(f.DateFormat) {
			return fieldErr(fieldPath+".date_format",
				"layout %q carries a day-of-month component but the precision is year_only", f.DateFormat)
		}
	}
	return nil
}

func validateTransform(fieldPath string, i int, t *Transform, f *FieldSpec, fieldName string) error {
	p := fmt.Sprintf("%s.transform[%d]", fieldPath, i)
	switch t.Name {
	case TransformTrim, TransformLowercase, TransformUppercase:
		if t.Arg != "" || t.With != "" {
			return fieldErr(p, "%s takes no argument", t.Name)
		}
	case TransformStripPrefix, TransformStripSuffix:
		if t.Arg == "" {
			return fieldErr(p, "%s requires the literal string to strip", t.Name)
		}
	case TransformRegexExtract:
		if t.Arg == "" {
			return fieldErr(p, "regex_extract requires a pattern")
		}
		re, err := regexp.Compile(t.Arg)
		if err != nil {
			return fieldErr(p, "invalid RE2 pattern: %v", err)
		}
		t.re = re
	case TransformRegexReplace:
		if t.Arg == "" {
			return fieldErr(p, "regex_replace requires a pattern")
		}
		re, err := regexp.Compile(t.Arg)
		if err != nil {
			return fieldErr(p, "invalid RE2 pattern: %v", err)
		}
		t.re = re
	case TransformMap:
		if len(f.Map) == 0 {
			return fieldErr(p, "the map transform requires a map on the same field")
		}
	case TransformParseDate:
		if !isDateField(fieldName) {
			return fieldErr(p, "parse_date is only valid on a date field")
		}
	case "":
		return fieldErr(p, "transform has no name")
	default:
		return fieldErr(p, "%q is not part of the transform vocabulary", t.Name)
	}
	return nil
}

func (c *Config) validateSchedule() error {
	s := &c.Spec.Schedule
	if s.CheckFrequencySeconds < 0 {
		return fieldErr("spec.schedule.check_frequency_seconds", "must not be negative")
	}
	if s.MinFrequencySeconds < 0 {
		return fieldErr("spec.schedule.min_frequency_seconds", "must not be negative")
	}
	if s.CheckFrequencySeconds > 0 && s.CheckFrequencySeconds < 60 {
		return fieldErr("spec.schedule.check_frequency_seconds", "must be at least 60 seconds; politeness is not optional")
	}
	if s.MinFrequencySeconds > 0 && s.CheckFrequencySeconds > 0 && s.CheckFrequencySeconds < s.MinFrequencySeconds {
		return fieldErr("spec.schedule.check_frequency_seconds",
			"%d is below the declared minimum interval of %d", s.CheckFrequencySeconds, s.MinFrequencySeconds)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// compileSelector compiles a CSS selector, refusing ":scope" because callers must
// handle it before reaching here.
func compileSelector(s string) (cascadia.Selector, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, errors.New("selector must not be empty")
	}
	if trimmed == ScopeSelector {
		return nil, fmt.Errorf("%q is handled by the engine and must not be compiled", ScopeSelector)
	}
	sel, err := cascadia.Compile(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid CSS selector %q: %w", trimmed, err)
	}
	return sel, nil
}

func hasGroup(re *regexp.Regexp, name string) bool {
	if re == nil {
		return false
	}
	for _, n := range re.SubexpNames() {
		if n == name {
			return true
		}
	}
	return false
}

// layoutHasDay reports whether a Go time layout renders a day-of-month.
//
// It is decided empirically -- format two dates that differ only in day of month and
// see whether the output differs -- rather than by scanning for "02"/"_2"/"2", because
// those substrings also occur inside "2006", inside literal text, and inside a
// zone offset, and a scanner that gets that wrong either rejects valid configs or,
// far worse, accepts a month_only field whose layout silently pins a day.
func layoutHasDay(layout string) bool {
	a := time.Date(2006, time.January, 2, 15, 4, 5, 0, time.UTC)
	b := time.Date(2006, time.January, 3, 15, 4, 5, 0, time.UTC)
	return a.Format(layout) != b.Format(layout)
}

// layoutHasMonth reports whether a Go time layout renders a month.
func layoutHasMonth(layout string) bool {
	a := time.Date(2006, time.January, 15, 15, 4, 5, 0, time.UTC)
	b := time.Date(2006, time.February, 15, 15, 4, 5, 0, time.UTC)
	return a.Format(layout) != b.Format(layout)
}
