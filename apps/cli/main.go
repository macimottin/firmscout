// Command firmscout is the operator's tool: migrations, registry synchronisation,
// manual source checks and API key management.
//
// It runs the same use cases the worker does, against the same adapters, which is
// deliberate: the fastest way to understand or debug the pipeline is to run one stage
// of it by hand and read what it did.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/macimottin/firmscout/internal/adapters/postgres"
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
  snapshot export       Write the catalogue's observed facts to data/snapshot/
  snapshot import       Load a snapshot into the database (registry sync first)
  sources list          List registered sources and whether they are collectable
  sources activate      Move a reviewed source from pending_review to active,
                         so it becomes eligible for collection (--id or --slug
                         with --vendor)
  check-source          Run one source check now (--id or --slug with --vendor)
  conflicts list        Show unresolved source disagreements and whether each is queued
  review list           Show the open review queue, highest priority first
  review show           Show one review item in full, including any other candidates
                         a multi-source conflict has parked (--id)
  review accept         Accept a review item and publish its candidate (--id, --actor, --reason)
  review reject         Reject a review item's candidate (--id, --actor, --reason)
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
	case "snapshot":
		return runSnapshot(ctx, args[1:])
	case "sources":
		return runSources(ctx, args[1:])
	case "check-source":
		return runCheckSource(ctx, args[1:])
	case "conflicts":
		return runConflicts(ctx, args[1:])
	case "review":
		return runReview(ctx, args[1:])
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// runSnapshot dispatches the snapshot subcommands.
//
// A snapshot is how the catalogue's observed facts leave the database and enter Git, so
// that cloning this repository and running it produces a populated catalogue rather than
// an empty one. See ADR-0025 and internal/application/snapshot.go.
func runSnapshot(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("snapshot needs a subcommand: export or import")
	}
	switch args[0] {
	case "export":
		return runSnapshotExport(ctx, args[1:])
	case "import":
		return runSnapshotImport(ctx, args[1:])
	default:
		return fmt.Errorf("unknown snapshot subcommand %q, want export or import", args[0])
	}
}

// DefaultSnapshotDir is where a snapshot lives in this repository.
const DefaultSnapshotDir = "data/snapshot"

const (
	snapshotFactsFile    = "releases.ndjson"
	snapshotManifestFile = "manifest.json"
)

func runSnapshotExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("snapshot export", flag.ContinueOnError)
	dir := fs.String("dir", DefaultSnapshotDir, "directory to write the snapshot into")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := container(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	facts, err := c.Snapshots.ExportReleaseFacts(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", *dir, err)
	}

	// NDJSON, one fact per line, written with SetEscapeHTML(false) so a release-notes
	// URL keeps its ampersands instead of becoming \u0026 -- the file is meant to be
	// read by people and by tools that are not Go's html/template.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for i := range facts {
		if err := enc.Encode(facts[i]); err != nil {
			return fmt.Errorf("encode release %s: %w", facts[i].Release.ID, err)
		}
	}
	factsPath := filepath.Join(*dir, snapshotFactsFile)
	if err := os.WriteFile(factsPath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", factsPath, err)
	}

	vendors, products := postgres.SnapshotSlugs(facts)
	manifest := application.SnapshotManifest{
		FormatVersion: application.SnapshotVersion,
		GeneratedAt:   c.Clock.Now().UTC().Truncate(time.Second),
		ReleaseCount:  len(facts),
		Vendors:       vendors,
		Products:      products,
		License:       application.SnapshotLicense,
		Notice:        application.SnapshotNotice,
	}
	mbuf, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	manifestPath := filepath.Join(*dir, snapshotManifestFile)
	if err := os.WriteFile(manifestPath, append(mbuf, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", manifestPath, err)
	}

	fmt.Printf("Exported %d release(s) to %s\n", len(facts), factsPath)
	fmt.Printf("  vendors:  %s\n", strings.Join(vendors, ", "))
	fmt.Printf("  products: %s\n", strings.Join(products, ", "))
	fmt.Printf("  licence:  %s\n", application.SnapshotLicense)
	return nil
}

func runSnapshotImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("snapshot import", flag.ContinueOnError)
	dir := fs.String("dir", DefaultSnapshotDir, "directory to read the snapshot from")
	if err := fs.Parse(args); err != nil {
		return err
	}

	factsPath := filepath.Join(*dir, snapshotFactsFile)
	f, err := os.Open(factsPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", factsPath, err)
	}
	defer func() { _ = f.Close() }()

	var facts []application.ReleaseFact
	sc := bufio.NewScanner(f)
	// A fact with a long release-notes URL and an excerpt can exceed bufio's 64 KiB
	// default, and the failure mode is a truncated line that fails to parse rather than
	// a clear "line too long".
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var fact application.ReleaseFact
		if err := json.Unmarshal([]byte(text), &fact); err != nil {
			return fmt.Errorf("%s line %d: %w", factsPath, line, err)
		}
		facts = append(facts, fact)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", factsPath, err)
	}

	c, err := container(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	imported, skipped, err := c.Snapshots.ImportReleaseFacts(ctx, facts)
	if err != nil {
		return err
	}
	fmt.Printf("Imported %d release(s); %d already present.\n", imported, skipped)
	return nil
}

// runConflicts dispatches the conflict subcommands.
func runConflicts(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("conflicts needs a subcommand: list")
	}
	switch args[0] {
	case "list":
		return runConflictsList(ctx, args[1:])
	default:
		return fmt.Errorf("unknown conflicts subcommand %q", args[0])
	}
}

// runConflictsList prints the unresolved multi-source disagreements.
//
// It is read-only on purpose, and deliberately not a way to resolve anything: ADR-0020
// puts the resolution of a disagreement on the review queue, so the way to settle one is
// "review accept" or "review reject" on the item covering it. What this command adds is
// the question the queue cannot answer -- whether every open conflict actually has such
// an item. A conflict whose QUEUED column reads "-" has reached nobody, which is the one
// failure ADR-0020 exists to prevent, so it prints as a dash rather than an empty cell
// for the same reason "review list" prints one for a deleted product: a blank column
// reads as a bug in the command instead of the fact it is.
func runConflictsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("conflicts list", flag.ContinueOnError)
	limit := fs.Int("limit", application.DefaultOpenConflictPageSize, "maximum conflicts to show")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := container(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	entries, err := application.NewListOpenConflicts(c.ConflictQueryDeps()).Execute(ctx, *limit)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("No source conflicts are open.")
		return nil
	}

	rows := []string{"CONFLICT\tPRODUCT\tCHANNEL\tVERSIONS\tSOURCES\tAGE\tQUEUED"}
	unqueued := 0
	for _, e := range entries {
		productSlug := e.ProductSlug
		if productSlug == "" {
			productSlug = "-"
		}
		queued := e.ReviewItem.ID
		if !e.Queued || queued == "" {
			queued = "-"
			unqueued++
		}
		rows = append(rows, strings.Join([]string{
			e.Conflict.ID, productSlug, e.Conflict.Channel,
			strings.Join(e.Conflict.Versions, ","),
			fmt.Sprint(len(e.Conflict.SourceIDs)),
			e.Age.Truncate(time.Minute).String(), queued,
		}, "\t"))
	}
	if err := writeTable(rows); err != nil {
		return err
	}
	if unqueued > 0 {
		// Worth saying out loud rather than leaving in a column: this is the state
		// ADR-0020 is meant to make impossible, so it should not need spotting.
		fmt.Printf("\n%d open conflict(s) have no review item; nobody has been asked to settle them.\n", unqueued)
	}
	return nil
}

// runReview dispatches the review queue's operator commands: list, show, accept and reject.
//
// Accept and reject used to be deliberately absent from this binary, on the theory that
// having to go through the switched-off HTTP surface to make a decision was itself a
// control. It was not: apps/web turned out willing to run that surface's writes
// server-side on a visitor's behalf, which defeated the actual control ADR-0021 relies
// on -- network placement -- without the HTTP surface itself ever being reachable from
// outside its private subnet (see ADR-0021's 2026-09-05 amendment). apps/web's decision
// UI is gone; this command is now the only way to accept or reject an item, and it does
// not call the HTTP surface at all -- it runs the same use case the HTTP handler runs,
// directly against the database. Its control is the one thing a public web request
// cannot forge: a database connection string and a shell on a host that holds one.
func runReview(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("review needs a subcommand: list, show, accept, or reject")
	}
	switch args[0] {
	case "list":
		return runReviewList(ctx, args[1:])
	case "show":
		return runReviewShow(ctx, args[1:])
	case "accept":
		return runReviewDecision(ctx, "accept", args[1:])
	case "reject":
		return runReviewDecision(ctx, "reject", args[1:])
	default:
		return fmt.Errorf("unknown review subcommand %q", args[0])
	}
}

// runReviewList prints a filtered page of the queue.
func runReviewList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review list", flag.ContinueOnError)
	state := fs.String("state", "", "comma-separated states (default: open and in_progress)")
	kind := fs.String("kind", "", "comma-separated review kinds")
	sla := fs.String("sla", "", "comma-separated SLA classes: urgent, high, standard, low")
	vendor := fs.String("vendor", "", "restrict to one vendor slug")
	product := fs.String("product", "", "restrict to one product slug")
	limit := fs.Int("limit", 50, "maximum items to show")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := container(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	page, err := application.NewListReviewQueue(c.ReviewQueryDeps()).Execute(ctx, application.ReviewQueueQuery{
		States:      splitList(*state),
		Kinds:       splitList(*kind),
		SLAClasses:  splitList(*sla),
		VendorSlug:  *vendor,
		ProductSlug: *product,
		Limit:       *limit,
	})
	if err != nil {
		return err
	}
	if len(page.Entries) == 0 {
		fmt.Println("The review queue is empty.")
		return nil
	}
	rows := []string{"ID\tSLA\tSCORE\tKIND\tSTATE\tAGE\tPRODUCT\tTITLE"}
	for _, e := range page.Entries {
		productSlug := e.ProductSlug
		if productSlug == "" {
			// ON DELETE SET NULL leaves an item whose product is gone. Printing an
			// empty column would read as a bug in this command rather than as the
			// fact it is.
			productSlug = "-"
		}
		rows = append(rows, strings.Join([]string{
			e.Item.ID, e.Item.SLAClass, fmt.Sprint(e.Item.PriorityScore), e.Item.Kind,
			e.Item.State, e.Age.Truncate(time.Minute).String(), productSlug, e.Item.Title,
		}, "\t"))
	}
	if err := writeTable(rows); err != nil {
		return err
	}
	if page.NextCursor != "" {
		fmt.Printf("\nMore items are waiting; raise --limit to see them.\n")
	}
	return nil
}

// runReviewShow prints one review item in full, including -- for a multi-source
// conflict -- every other candidate the conflict has parked in human_review_required
// alongside it.
//
// The queue names one candidate as the item's subject (SubjectID): the one "review
// accept" or "review reject" acts on. A multi-source conflict can leave more than one
// candidate waiting on that same decision, and the subject can only name one of them --
// see PayloadKeyConflictCandidates's doc comment (internal/application/ports.go). Before
// this command, that key was written by validation and read by nothing except a test
// helper, so the reachability the comment promises held only inside the database. This
// is that promise's reader.
func runReviewShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review show", flag.ContinueOnError)
	id := fs.String("id", "", "review item id (rev_...)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("--id is required")
	}

	c, err := container(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	detail, err := application.NewGetReviewItem(c.ReviewQueryDeps()).Execute(ctx, *id)
	if err != nil {
		return err
	}
	item := detail.Entry.Item

	productSlug := detail.Entry.ProductSlug
	if productSlug == "" {
		// Matches "review list": a blank column here reads as a bug in this command,
		// not as the fact that ON DELETE SET NULL left the product gone.
		productSlug = "-"
	}
	fmt.Printf("Review item %s: %s (%s)\n", item.ID, item.Kind, item.State)
	fmt.Printf("  SLA:     %s\n", item.SLAClass)
	fmt.Printf("  Score:   %d\n", item.PriorityScore)
	fmt.Printf("  Title:   %s\n", item.Title)
	if item.Detail != "" {
		fmt.Printf("  Detail:  %s\n", item.Detail)
	}
	fmt.Printf("  Product: %s\n", productSlug)
	fmt.Printf("  Subject: %s %s", item.SubjectType, item.SubjectID)
	if detail.CandidateFound {
		fmt.Printf(" (version %s, state %s)", detail.Candidate.Version.Raw(), detail.Candidate.State)
	} else {
		fmt.Print(" (candidate not found)")
	}
	fmt.Println()
	if gate := item.Payload[application.PayloadKeyFailingGate]; gate != "" {
		fmt.Printf("  Failing gate: %s\n", gate)
	}

	if !detail.ConflictFound {
		return nil
	}
	fmt.Printf("\nConflict %s (%s)\n", detail.Conflict.ID, detail.Conflict.State)
	if versions := item.Payload[application.PayloadKeyConflictVersions]; versions != "" {
		fmt.Printf("  Versions in dispute: %s\n", versions)
	}
	if sources := item.Payload[application.PayloadKeyConflictSources]; sources != "" {
		fmt.Printf("  Sources:             %s\n", sources)
	}
	candidates := splitList(item.Payload[application.PayloadKeyConflictCandidates])
	if len(candidates) == 0 {
		// The write path (internal/application.reviewPayload) populates this for
		// every conflict item; an empty list here means either an item written
		// before that key existed, or the write path itself has regressed. Either
		// way, this says so instead of silently showing only the subject.
		fmt.Println("  Candidates parked awaiting this decision: none recorded on this item")
		return nil
	}
	fmt.Println("  Candidates parked awaiting this decision:")
	for _, cid := range candidates {
		marker := ""
		if cid == item.SubjectID {
			marker = " (subject -- what accept/reject would act on)"
		}
		fmt.Printf("    - %s%s\n", cid, marker)
	}
	return nil
}

// runReviewDecision runs "review accept" or "review reject". verb is the imperative
// the caller typed ("accept" or "reject"), kept distinct from
// application.ReviewDecisionAccepted/Rejected (the past-tense strings the use case and
// the audit trail use) so a usage or parse error names the subcommand the caller ran,
// not the state it would have left behind.
//
// It builds application.DecideReviewItem the same way runCheckSource builds
// application.PublishRelease -- straight from the container, no HTTP client involved --
// because a human accepting an item must publish through the exact same PublishRelease
// instance the worker runs (see platform.Container.ReviewDeps's doc comment); routing
// this through the internal HTTP surface instead would publish through a second copy of
// that use case for no reason other than habit.
func runReviewDecision(ctx context.Context, verb string, args []string) error {
	fs := flag.NewFlagSet("review "+verb, flag.ContinueOnError)
	id := fs.String("id", "", "review item id (rev_...)")
	actor := fs.String("actor", "", "your name, recorded unverified in the audit trail (required; ADR-0021)")
	reason := fs.String("reason", "", "why this decision was made; becomes part of the audit trail (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("--id is required")
	}
	// DecideReviewItem rejects a blank actor or reason itself, but failing here first
	// means a typo'd flag name (which leaves the value empty) is reported as a missing
	// flag rather than as a validation error about an audit trail the caller never
	// meant to leave blank.
	if strings.TrimSpace(*actor) == "" {
		return errors.New("--actor is required: a blank actor cannot enter the audit trail (ADR-0021)")
	}
	if strings.TrimSpace(*reason) == "" {
		return errors.New("--reason is required: a decision with no stated reason is not an audit trail, it is a timestamp")
	}

	c, err := container(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	publisher := application.NewPublishRelease(c.IngestDeps())
	uc := application.NewDecideReviewItem(c.ReviewDeps(publisher))
	input := application.ReviewDecisionInput{
		ItemID: *id,
		Actor:  *actor,
		// False, always, in this phase -- the CLI has no more way to verify who is
		// running it than the HTTP surface has to verify a header. See ADR-0021.
		ActorAuthenticated: false,
		Reason:             *reason,
	}

	var result application.ReviewDecisionResult
	switch verb {
	case "accept":
		result, err = uc.Accept(ctx, input)
	case "reject":
		result, err = uc.Reject(ctx, input)
	default:
		return fmt.Errorf("unknown review decision %q", verb)
	}
	if err != nil {
		return err
	}

	fmt.Printf("Review item %s: %s\n", result.ItemID, result.Decision)
	if result.ReleaseID != "" {
		fmt.Printf("  release:   %s (published=%v)\n", result.ReleaseID, result.Published)
	}
	if result.ConflictID != "" {
		fmt.Printf("  conflict:  %s resolved\n", result.ConflictID)
	}
	return nil
}

// splitList turns a comma-separated flag into the slice the query takes. An empty flag
// must produce a nil slice, not a one-element slice holding "", because the query layer
// reads an empty slice as "no filter" and would reject "" as an unknown value.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
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

		// WithSummaries is not optional in a real deployment, whatever its builder shape
		// suggests. A product with no product_summaries row is a 404 on the public API
		// and absent from search, so a sync without a refresher upserts a catalogue
		// nobody can read -- and a hardware model, which has no releases of its own and
		// therefore never reaches the publish path that would refresh it otherwise, would
		// be invisible for its entire life. See ADR-0024 D6.
		uc := application.NewSyncRegistry(c.RegistryLoader(), c.Vendors, c.Categories, c.Products, c.Sources, c.IDs, c.Clock).
			WithSummaries(c.Releases)
		report, err := uc.Execute(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Synchronised: %d vendor(s), %d category(ies), %d family(ies), %d product(s), %d alias(es), %d source(s).\n",
			report.VendorsUpserted, report.CategoriesUpserted, report.FamiliesUpserted,
			report.ProductsUpserted, report.AliasesUpserted, report.SourcesUpserted)
		// Relationships and summaries are reported rather than assumed, because both are
		// silent when they go wrong: a device with no runs edge renders as a model number
		// pointing nowhere, and a product with no summary is simply missing.
		fmt.Printf("%d product relationship(s) written; %d product summary(ies) refreshed.\n",
			report.RelationshipsUpserted, report.SummariesRefreshed)
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
	if len(args) == 0 {
		return errors.New("sources needs a subcommand: list, activate")
	}
	switch args[0] {
	case "list":
		return runSourcesList(ctx, args[1:])
	case "activate":
		return runSourcesActivate(ctx, args[1:])
	default:
		return fmt.Errorf("sources: unknown subcommand %q, want list or activate", args[0])
	}
}

func runSourcesList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sources list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
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

// runSourcesActivate moves a source from pending_review to active, which is what
// makes gate 1 (source_eligible, internal/application/ingest.go) start accepting its
// candidates.
//
// It exists because nothing else in this codebase ever performs that transition.
// ADR-0018 gates *collection* on a human reviewing terms of use, and registry sync
// preserves whatever health a source already has rather than promoting it (a
// re-sync must never silently reactivate something an operator degraded or
// disabled). The result, discovered by actually running check-source against a
// freshly approved source rather than by reading the code, is that a source can be
// enabled, compliant, successfully fetched, and successfully extracted, and every
// single candidate it produces is still rejected at gate 1 forever -- "source is not
// eligible for collection or is not active" -- because pending_review is not active
// and nothing ever asked it to become so. That is a silent dead end: the fetch
// succeeds, the log looks like progress, and nothing is ever published.
//
// This command is the missing step, and nothing more: it performs exactly the
// transition the pending_review -> active edge of domain.Source's own state machine
// already declares legal, and refuses (via TransitionHealth) if the source is
// anywhere else -- retired, for instance, has no path back. It does not touch
// compliance fields; a source that fails CompliancePermitsCollection is still
// refused at fetch time regardless of health.
func runSourcesActivate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sources activate", flag.ContinueOnError)
	id := fs.String("id", "", "source id")
	slug := fs.String("slug", "", "source slug (requires --vendor)")
	vendor := fs.String("vendor", "", "vendor slug")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := container(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	sourceID, err := resolveSourceID(ctx, c, *id, *slug, *vendor)
	if err != nil {
		return err
	}

	src, err := c.Sources.GetByID(ctx, sourceID)
	if err != nil {
		return err
	}
	if src.Health == domain.SourceActive {
		fmt.Printf("Source %q is already active.\n", src.Slug)
		return nil
	}
	from := src.Health
	if err := src.TransitionHealth(domain.SourceActive); err != nil {
		return fmt.Errorf("activate %q: %w", src.Slug, err)
	}
	if err := c.Sources.Upsert(ctx, src); err != nil {
		return fmt.Errorf("activate %q: %w", src.Slug, err)
	}
	fmt.Printf("Source %q moved from %s to active.\n", src.Slug, from)
	if !src.CompliancePermitsCollection() {
		fmt.Printf("  Note: robots=%s terms=%s still refuses collection; activating health alone does not enable fetching.\n",
			src.RobotsPolicyStatus, src.TermsReviewStatus)
	}
	return nil
}

// resolveSourceID looks up a source by --id, or by --slug plus --vendor. Both
// check-source and sources activate take a source the same way, and having them
// diverge would mean fixing an ambiguous-lookup bug in one without the other.
func resolveSourceID(ctx context.Context, c *platform.Container, id, slug, vendor string) (string, error) {
	if id != "" {
		return id, nil
	}
	if slug == "" || vendor == "" {
		return "", errors.New("give --id, or both --slug and --vendor")
	}
	v, err := c.Vendors.GetBySlug(ctx, vendor)
	if err != nil {
		return "", fmt.Errorf("vendor %q: %w", vendor, err)
	}
	s, err := c.Sources.GetBySlug(ctx, v.ID, slug)
	if err != nil {
		return "", fmt.Errorf("source %q: %w", slug, err)
	}
	return s.ID, nil
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

	sourceID, err := resolveSourceID(ctx, c, *id, *slug, *vendor)
	if err != nil {
		return err
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
