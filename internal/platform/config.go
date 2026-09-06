// Package platform is FirmScout's composition root: configuration, dependency wiring
// and process lifecycle.
//
// It is the only package permitted to import every layer, because assembling the
// object graph is precisely the job that requires knowing about all of them. Nothing
// else may import it; internal/archtest enforces that.
package platform

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the complete runtime configuration, read from the environment.
//
// Every value has a documented default that is safe for local development, so a
// contributor can run the stack without a configuration file. Nothing here defaults to
// a production-shaped value: an unset OTLP endpoint disables tracing rather than
// pointing somewhere unexpected, and an unset database URL is a startup error rather
// than a silent fallback.
type Config struct {
	// Service identifies this process in telemetry: firmscout-api, firmscout-worker
	// or firmscout-cli.
	Service     string
	Version     string
	Environment string

	// DatabaseURL is required. There is no default, because guessing at a database
	// connection is how a process ends up writing to the wrong one.
	DatabaseURL     string
	DatabaseMaxConn int32

	HTTPAddr            string
	HTTPReadTimeout     time.Duration
	HTTPWriteTimeout    time.Duration
	HTTPShutdownTimeout time.Duration

	// OTLPEndpoint is optional. When empty, telemetry degrades to local-only
	// metrics and no tracing, and the process starts normally. A missing collector
	// must never prevent FirmScout from running.
	OTLPEndpoint string
	OTLPInsecure bool
	LogLevel     string
	LogFormat    string

	// Collection settings.
	UserAgent          string
	FetchMaxBytes      int64
	FetchTimeout       time.Duration
	PerHostConcurrency int

	// Worker settings.
	WorkerID          string
	WorkerConcurrency int
	JobLeaseDuration  time.Duration
	SchedulerInterval time.Duration

	// Ingestion policy.
	ConfidenceThreshold float64
	FutureDateTolerance time.Duration

	// Registry and artifacts.
	RegistryDir  string
	CollectorDir string
	ArtifactDir  string

	// ReviewAPIEnabled exposes the /internal review surface. It defaults to false and
	// there is deliberately no environment in which it defaults to true: the surface
	// performs writes -- publishing a release, resolving a conflict -- on the word of
	// an actor nobody authenticated (ADR-0021). Reaching it must take a decision
	// somebody made on purpose, not an unset variable.
	ReviewAPIEnabled bool
}

// DefaultUserAgent identifies FirmScout to the sites it monitors, including a contact
// URL so an operator who sees the traffic can find out what it is and ask us to stop.
const DefaultUserAgent = "FirmScout/0.1 (+https://github.com/macimottin/firmscout; open-source firmware version catalogue)"

// Load reads configuration from the environment, applying defaults and validating.
func Load(service string) (Config, error) {
	c := Config{
		Service:             service,
		Version:             env("FIRMSCOUT_VERSION", "dev"),
		Environment:         env("FIRMSCOUT_ENV", "development"),
		DatabaseURL:         os.Getenv("FIRMSCOUT_DATABASE_URL"),
		DatabaseMaxConn:     int32(envInt("FIRMSCOUT_DATABASE_MAX_CONN", 10)),
		HTTPAddr:            env("FIRMSCOUT_HTTP_ADDR", ":8080"),
		HTTPReadTimeout:     envDuration("FIRMSCOUT_HTTP_READ_TIMEOUT", 15*time.Second),
		HTTPWriteTimeout:    envDuration("FIRMSCOUT_HTTP_WRITE_TIMEOUT", 30*time.Second),
		HTTPShutdownTimeout: envDuration("FIRMSCOUT_HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
		OTLPEndpoint:        os.Getenv("FIRMSCOUT_OTLP_ENDPOINT"),
		OTLPInsecure:        envBool("FIRMSCOUT_OTLP_INSECURE", true),
		LogLevel:            env("FIRMSCOUT_LOG_LEVEL", "info"),
		LogFormat:           env("FIRMSCOUT_LOG_FORMAT", "json"),
		UserAgent:           env("FIRMSCOUT_USER_AGENT", DefaultUserAgent),
		FetchMaxBytes:       int64(envInt("FIRMSCOUT_FETCH_MAX_BYTES", 8<<20)),
		FetchTimeout:        envDuration("FIRMSCOUT_FETCH_TIMEOUT", 30*time.Second),
		PerHostConcurrency:  envInt("FIRMSCOUT_PER_HOST_CONCURRENCY", 2),
		WorkerID:            env("FIRMSCOUT_WORKER_ID", defaultWorkerID()),
		WorkerConcurrency:   envInt("FIRMSCOUT_WORKER_CONCURRENCY", 4),
		JobLeaseDuration:    envDuration("FIRMSCOUT_JOB_LEASE", 5*time.Minute),
		SchedulerInterval:   envDuration("FIRMSCOUT_SCHEDULER_INTERVAL", 60*time.Second),
		ConfidenceThreshold: envFloat("FIRMSCOUT_CONFIDENCE_THRESHOLD", 0.85),
		FutureDateTolerance: envDuration("FIRMSCOUT_FUTURE_DATE_TOLERANCE", 48*time.Hour),
		RegistryDir:         env("FIRMSCOUT_REGISTRY_DIR", "dataset"),
		CollectorDir:        env("FIRMSCOUT_COLLECTOR_DIR", "collectors/config"),
		ArtifactDir:         env("FIRMSCOUT_ARTIFACT_DIR", ".artifacts"),
		ReviewAPIEnabled:    envBool("FIRMSCOUT_REVIEW_API_ENABLED", false),
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate checks the configuration is usable.
func (c Config) Validate() error {
	if strings.TrimSpace(c.DatabaseURL) == "" {
		return fmt.Errorf("FIRMSCOUT_DATABASE_URL is required")
	}
	if c.ConfidenceThreshold < 0 || c.ConfidenceThreshold > 1 {
		return fmt.Errorf("FIRMSCOUT_CONFIDENCE_THRESHOLD must be between 0 and 1, got %v", c.ConfidenceThreshold)
	}
	if c.FetchMaxBytes <= 0 {
		return fmt.Errorf("FIRMSCOUT_FETCH_MAX_BYTES must be positive")
	}
	if c.PerHostConcurrency < 1 {
		return fmt.Errorf("FIRMSCOUT_PER_HOST_CONCURRENCY must be at least 1")
	}
	if c.WorkerConcurrency < 1 {
		return fmt.Errorf("FIRMSCOUT_WORKER_CONCURRENCY must be at least 1")
	}
	return nil
}

// Redacted returns a copy safe to log: the database URL's credentials are removed.
// Configuration is logged at startup, and a connection string in a log line is a
// credential leak that outlives the process.
func (c Config) Redacted() Config {
	out := c
	out.DatabaseURL = redactURL(c.DatabaseURL)
	return out
}

func redactURL(raw string) string {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	scheme := strings.Index(raw, "://")
	if scheme < 0 {
		return "[redacted]"
	}
	return raw[:scheme+3] + "[redacted]" + raw[at:]
}

func defaultWorkerID() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "worker"
	}
	return h
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
