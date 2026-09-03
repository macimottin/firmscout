// Command firmscout is the operator's tool: migrations, registry synchronisation,
// manual source checks and API key management.
//
// It runs the same use cases the worker does, against the same adapters, which is
// deliberate: the fastest way to understand or debug the pipeline is to run one stage
// of it by hand and read what it did.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/macimottin/firmscout/internal/application"
	"github.com/macimottin/firmscout/internal/domain"
	"github.com/macimottin/firmscout/internal/platform"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `firmscout - operator tool for the FirmScout catalogue

Usage:
  firmscout <command> [flags]

Commands:
  migrate up            Apply pending database migrations
  migrate status        Show applied and pending migrations
  registry sync         Load dataset/ and collectors/config/ into the database
  registry validate     Parse the registry without writing anything
  sources list          List registered sources and whether they are collectable
  check-source          Run one source check now (--id or --slug with --vendor)
  version               Print the version

Environment:
  FIRMSCOUT_DATABASE_URL   required, PostgreSQL connection string
  FIRMSCOUT_REGISTRY_DIR   default "dataset"
  FIRMSCOUT_COLLECTOR_DIR  default "collectors/config"

Every command exits non-zero on failure and prints what it did on success.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "firmscout: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("no command given")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "version":
		fmt.Println("firmscout", version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "migrate":
		return runMigrate(ctx, args[1:])
	case "registry":
		return runRegistry(ctx, args[1:])
	case "sources":
		return runSources(ctx, args[1:])
	case "check-source":
		return runCheckSource(ctx, args[1:])
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// container builds the object graph, or explains clearly why it could not.
func container(ctx context.Context) (*platform.Container, error) {
	cfg, err := platform.Load("firmscout-cli")
	if err != nil {
		return nil, err
	}
	return platform.Build(ctx, cfg)
}

func runMigrate(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("migrate needs a subcommand: up or status")
	}
	c, err := container(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	switch args[0] {
	case "up":
		before, err := c.Migrator.Status(ctx)
		if err != nil {
			return fmt.Errorf("read migration status: %w", err)
		}
		if err := c.Migrator.Up(ctx); err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		after, err := c.Migrator.Status(ctx)
		if err != nil {
			return fmt.Errorf("read migration status: %w", err)
		}
		applied := len(after.Applied) - len(before.Applied)
		if applied == 0 {
			fmt.Println("Database is already up to date.")
			return nil
		}
		fmt.Printf("Applied %d migration(s).\n", applied)
		for _, m := range after.Applied[len(before.Applied):] {
			fmt.Printf("  %s\n", m)
		}
		return nil

	case "status":
		st, err := c.Migrator.Status(ctx)
		if err != nil {
			return fmt.Errorf("read migration status: %w", err)
		}
		rows := []string{"VERSION\tSTATE"}
		for _, m := range st.Applied {
			rows = append(rows, m+"\tapplied")
		}
		for _, m := range st.Pending {
			rows = append(rows, m+"\tPENDING")
		}
		if err := writeTable(rows); err != nil {
			return err
		}
		if len(st.Pending) > 0 {
			fmt.Printf("\n%d migration(s) pending. Run 'firmscout migrate up'.\n", len(st.Pending))
		}
		return nil

	default:
		return fmt.Errorf("unknown migrate subcommand %q", args[0])
	}
}

func runRegistry(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("registry needs a subcommand: sync or validate")
	}

	switch args[0] {
	case "validate":
		// Validation deliberately does not need a database. A contributor should be
		// able to check their file without running PostgreSQL.
		cfg, err := platform.Load("firmscout-cli")
		if err != nil {
			// A missing database URL must not block validation, so fall back to
			// defaults for this one command.
			cfg = platform.Config{RegistryDir: envOr("FIRMSCOUT_REGISTRY_DIR", "dataset")}
		}
		loader := platform.RegistryLoaderFor(cfg.RegistryDir)
		docs, err := loader.Load(ctx)
		if err != nil {
			return err
		}
		var vendors, categories, families, products, sources, collectable int
		for _, d := range docs {
			switch {
			case d.Vendor != nil:
				vendors++
			case d.Category != nil:
				categories++
			case d.Family != nil:
				families++
			case d.Product != nil:
				products++
			case d.Source != nil:
				sources++
				if d.Source.Enabled && d.Source.CompliancePermitsCollection() {
					collectable++
				}
			}
		}
		fmt.Printf("Registry is valid: %d vendor(s), %d category(ies), %d family(ies), %d product(s), %d source(s).\n",
			vendors, categories, families, products, sources)
		fmt.Printf("%d of %d source(s) are currently collectable; the rest await a compliance decision (ADR-0018).\n",
			collectable, sources)
		return nil

	case "sync":
		c, err := container(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close(ctx) }()

		uc := application.NewSyncRegistry(c.RegistryLoader(), c.Vendors, c.Categories, c.Products, c.Sources, c.IDs, c.Clock)
		report, err := uc.Execute(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Synchronised: %d vendor(s), %d category(ies), %d family(ies), %d product(s), %d alias(es), %d source(s).\n",
			report.VendorsUpserted, report.CategoriesUpserted, report.FamiliesUpserted,
			report.ProductsUpserted, report.AliasesUpserted, report.SourcesUpserted)
		if report.SourcesLeftDisabled > 0 {
			fmt.Printf("\n%d source(s) will NOT be checked:\n", report.SourcesLeftDisabled)
			for _, w := range report.Warnings {
				fmt.Printf("  %s\n", w)
			}
			fmt.Println("\nThis is expected for a new source. See docs/adr/0018-source-compliance-policy.md.")
		}
		return nil

	default:
		return fmt.Errorf("unknown registry subcommand %q", args[0])
	}
}

func runSources(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return errors.New("sources needs a subcommand: list")
	}
	c, err := container(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	due, err := c.Sources.ListDispatchable(ctx, c.Clock.Now(), 200)
	if err != nil {
		return fmt.Errorf("list sources: %w", err)
	}
	if len(due) == 0 {
		fmt.Println("No sources are due for a check.")
		fmt.Println("A source is checked only when it is enabled, healthy, past its next check time,")
		fmt.Println("and permitted by both its robots policy and its terms review.")
		return nil
	}
	rows := []string{"ID\tSLUG\tHEALTH\tNEXT CHECK\tURL"}
	for _, s := range due {
		rows = append(rows, strings.Join([]string{
			s.ID, s.Slug, string(s.Health), s.NextCheckAt.Format(time.RFC3339), s.URL,
		}, "\t"))
	}
	return writeTable(rows)
}

func runCheckSource(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check-source", flag.ContinueOnError)
	id := fs.String("id", "", "source id")
	slug := fs.String("slug", "", "source slug (requires --vendor)")
	vendor := fs.String("vendor", "", "vendor slug")
	force := fs.Bool("force", false, "check even if the source is not yet due (compliance is still enforced)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := container(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	sourceID := *id
	if sourceID == "" {
		if *slug == "" || *vendor == "" {
			return errors.New("give --id, or both --slug and --vendor")
		}
		v, err := c.Vendors.GetBySlug(ctx, *vendor)
		if err != nil {
			return fmt.Errorf("vendor %q: %w", *vendor, err)
		}
		s, err := c.Sources.GetBySlug(ctx, v.ID, *slug)
		if err != nil {
			return fmt.Errorf("source %q: %w", *slug, err)
		}
		sourceID = s.ID
	}

	src, err := c.Sources.GetByID(ctx, sourceID)
	if err != nil {
		return err
	}
	if !src.CompliancePermitsCollection() {
		// --force overrides scheduling, never compliance. There is no flag that
		// makes FirmScout fetch from a source it has not been permitted to fetch
		// from, and adding one would defeat the point of ADR-0018.
		return fmt.Errorf("source %q may not be collected: robots=%s terms=%s (see docs/adr/0018-source-compliance-policy.md)",
			src.Slug, src.RobotsPolicyStatus, src.TermsReviewStatus)
	}
	if !src.Dispatchable(c.Clock.Now()) && !*force {
		fmt.Printf("Source %q is not due until %s. Re-run with --force to check anyway.\n",
			src.Slug, src.NextCheckAt.Format(time.RFC3339))
		return nil
	}

	res, err := application.NewCheckSource(c.CheckSourceDeps()).Execute(ctx, sourceID)
	if err != nil {
		return err
	}
	fmt.Printf("Checked %s\n", src.URL)
	fmt.Printf("  outcome:        %s\n", res.Outcome)
	if res.ChangeSignal != "" {
		fmt.Printf("  change signal:  %s\n", res.ChangeSignal)
	}
	if res.Reason != "" {
		fmt.Printf("  reason:         %s\n", res.Reason)
	}
	if res.ContentHash != "" {
		fmt.Printf("  content hash:   %s\n", truncate(res.ContentHash, 16))
	}
	fmt.Printf("  next check:     %s\n", res.NextCheckAt.Format(time.RFC3339))

	if !res.ExtractionEnqueued {
		return nil
	}
	fmt.Println("  extraction:     enqueued")

	// Run the rest of the pipeline inline so the operator sees the whole story
	// rather than having to start a worker to find out what happened.
	deps := c.IngestDeps()
	ex, err := application.NewExtractCandidates(deps).Execute(ctx, sourceID, res.ArtifactID)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	fmt.Printf("  extracted:      %d candidate(s), %d new, %d already known\n",
		ex.Extracted, ex.NewCandidates, ex.Duplicates)

	for _, cid := range ex.CandidateIDs {
		val, err := application.NewValidateCandidate(deps).Execute(ctx, cid)
		if err != nil {
			return fmt.Errorf("validate %s: %w", cid, err)
		}
		fmt.Printf("  %s: %s (%s)\n", cid, val.Decision, val.Reason)
		if val.Decision != domain.GatePassed {
			continue
		}
		pub, err := application.NewPublishRelease(deps).Execute(ctx, cid)
		if err != nil {
			return fmt.Errorf("publish %s: %w", cid, err)
		}
		if pub.Published {
			fmt.Printf("    published release %s (%s)\n", pub.ReleaseID, pub.Version)
		} else {
			fmt.Printf("    not published: %s\n", pub.Reason)
		}
	}
	return nil
}

// writeTable prints aligned columns, reporting a write error rather than discarding it.
func writeTable(rows []string) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		if _, err := io.WriteString(w, r+"\n"); err != nil {
			return err
		}
	}
	return w.Flush()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
