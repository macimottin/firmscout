package platform

import (
	"strings"
	"testing"
)

func TestLoadRequiresADatabaseURL(t *testing.T) {
	t.Setenv("FIRMSCOUT_DATABASE_URL", "")
	if _, err := Load("firmscout-api"); err == nil {
		t.Fatal("configuration loaded without a database URL; guessing at a connection is how a process writes to the wrong database")
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	t.Setenv("FIRMSCOUT_DATABASE_URL", "postgres://user:pass@localhost:5432/firmscout")
	c, err := Load("firmscout-api")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", c.HTTPAddr)
	}
	if c.UserAgent != DefaultUserAgent {
		t.Errorf("UserAgent = %q", c.UserAgent)
	}
	if !strings.Contains(c.UserAgent, "https://") {
		t.Error("the user agent must carry a contact URL so an operator can find out who is fetching")
	}
	if c.OTLPEndpoint != "" {
		t.Error("OTLP endpoint defaulted to a value; an unset endpoint must disable tracing, not guess")
	}
}

// A connection string in a log line is a credential leak that outlives the process.
func TestRedactedRemovesDatabaseCredentials(t *testing.T) {
	c := Config{DatabaseURL: "postgres://firmscout:hunter2@db.internal:5432/firmscout?sslmode=require"}
	got := c.Redacted().DatabaseURL
	if strings.Contains(got, "hunter2") {
		t.Fatalf("password survived redaction: %q", got)
	}
	if !strings.Contains(got, "db.internal") {
		t.Errorf("redaction removed the host too, making the value useless for debugging: %q", got)
	}
}

func TestValidateRejectsOutOfRangeValues(t *testing.T) {
	base := Config{DatabaseURL: "postgres://localhost/db", FetchMaxBytes: 1, PerHostConcurrency: 1, WorkerConcurrency: 1}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"confidence above 1", func(c *Config) { c.ConfidenceThreshold = 1.5 }},
		{"negative confidence", func(c *Config) { c.ConfidenceThreshold = -0.1 }},
		{"zero fetch limit", func(c *Config) { c.FetchMaxBytes = 0 }},
		{"zero host concurrency", func(c *Config) { c.PerHostConcurrency = 0 }},
		{"zero worker concurrency", func(c *Config) { c.WorkerConcurrency = 0 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("Validate accepted %s", tc.name)
			}
		})
	}
}

func TestIDGeneratorProducesSortablePrefixedIDs(t *testing.T) {
	g := NewIDGenerator()
	seen := map[string]bool{}
	var prev string
	for i := 0; i < 2000; i++ {
		id := g.NewID("rel")
		if !strings.HasPrefix(id, "rel_") {
			t.Fatalf("identifier lacks its type prefix: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate identifier generated: %q", id)
		}
		seen[id] = true
		if prev != "" && id <= prev {
			t.Fatalf("identifiers are not monotonically sortable: %q then %q", prev, id)
		}
		prev = id
	}
}
