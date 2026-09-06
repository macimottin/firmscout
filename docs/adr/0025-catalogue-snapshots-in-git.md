# ADR-0025: The catalogue's observed facts ship in Git as a snapshot

- **Status:** Accepted
- **Date:** 2026-09-06
- **Deciders:** repository owner
- **Requires qualified legal review:** yes — see Consequences
- **Related:** ADR-0007, ADR-0009, ADR-0016, ADR-0017, ADR-0018

## Context

[ADR-0016](0016-hybrid-dataset.md) split the catalogue in two. The **registry** — vendors,
families, products, aliases, sources, collector configurations — lives in Git as reviewed
YAML, because it is a set of decisions somebody made and should be able to argue about in a
pull request. The **observed facts** — releases, evidence, checks, candidates — live only in
PostgreSQL, because they are produced by machinery rather than authored, and because a
database is the right shape for something that grows every time a scheduler runs.

That split had a consequence nobody stated at the time: **cloning this repository and running
it gives you an empty product.** `migrate up` creates the schema, `registry sync` loads the
vocabulary, and then every page says the catalogue has nothing in it, because no source ships
collectable (ADR-0018) and the only way to obtain a release is to fetch from a manufacturer
yourself. The blueprint anticipated publishing data — §5's capability matrix promised "delayed
snapshots" to free users and a live feed to paying ones — but nothing was ever built, and the
shape it assumed was a commercial boundary rather than a file in the repository.

The repository owner's decision, stated directly, is that the data is public: anyone should be
able to build FirmScout locally and have it be *useful*, or host it themselves, and the
catalogue should be consumable by a person, by a tool, and by a model reading the file
directly. That is a different product from the one ADR-0007 described, and it needs the facts
to leave the database.

## Decision

**The catalogue's published releases are exported to `data/snapshot/` as newline-delimited
JSON, committed to Git, and loadable into any installation with `firmscout snapshot import`.**

One release per line, each record self-contained: the release, the products it applies to, and
the evidence that justifies it — source URL, retrieval time, collector, confidence, and the
bounded excerpt. A reader needs no schema and no join to understand a line, and a model can be
handed the file directly.

**Everything a snapshot references outside itself is named by slug, not by identifier.** Vendor,
product and source identifiers are minted by `registry sync`, so two installations that sync
the same committed registry hold different ones; a file carrying them would import only into
the database it came from. The release's own identifier is the single exception, preserved so
that re-importing is idempotent and an exported fact can be traced to the row it came from.

**Import resolves slugs against the local registry, runs in one transaction, and never
overwrites.** A snapshot naming a product this installation does not have fails before writing
anything, rather than silently dropping the record — an import that "succeeds" while losing
data is the worst available outcome. A release whose identifier is already present is skipped,
so a snapshot can never quietly undo what a local collection has since verified or withdrawn.

**What a snapshot deliberately excludes:** candidates, jobs, review items, source checks and
usage records, which describe what FirmScout was doing rather than what a vendor published; and
raw artifacts, which are hundreds of kilobytes of a manufacturer's own markup. What survives of
an artifact is its content hash and the bounded excerpt, which is enough to verify a claim and
not enough to republish a page — the line [licensing.md](../architecture/licensing.md) already
draws.

**The exported data is CC BY 4.0**, per ADR-0009, and the licence and a notice travel inside
the manifest so a copy separated from this repository still says what it is. The notice says
plainly that a snapshot records what a vendor published when FirmScout looked, and is not a
guarantee of what is current now.

## Consequences

A fresh clone becomes useful in three commands — `migrate up`, `registry sync`, `snapshot
import` — with no network access to any manufacturer. That is the point.

**Export is deterministic**, ordered by `(vendor slug, release id)` with timestamps pinned to
UTC, so re-exporting an unchanged catalogue produces a byte-identical file and Git records no
change. Without that property every re-export would be an unreviewable diff. This is verified
by a round-trip test rather than asserted.

**The snapshot is a second source of truth for facts, and it can go stale.** The database is
still where collection writes; the file is a periodic projection of it. A maintainer has to
re-export and commit, and nothing yet enforces that they do — a snapshot older than the
catalogue is a real failure mode this ADR does not solve.

**It changes the commercial shape ADR-0007 described.** The "delayed public snapshot, live paid
feed" boundary is not what this builds: the snapshot is current as of its export, and it is in
a public repository. ADR-0007's premise that history depth and automation are the paid boundary
survives; its assumption about *delay* does not. That is the owner's decision to make and it is
recorded here rather than left implicit.

**It needs legal review, and the review is narrower than it looks.** What is republished is
version strings, dates, channels, URLs and excerpts averaging under thirty characters — facts
and citations, not vendor prose. The questions worth a lawyer's time are whether the EU *sui
generis* database right attaches to the compilation (CC BY 4.0 is chosen precisely because it
addresses that right explicitly), and whether any vendor's terms restrict redistribution of
facts collected from their public pages. ADR-0018's per-source terms review already gates
collection; it does not yet gate *redistribution*, and those are different questions.

## Alternatives

**Keep facts database-only and publish snapshots elsewhere** — an S3 bucket, a release
artifact, a separate data repository. This is what the blueprint assumed. It was rejected
because it puts the data behind an availability dependency the code does not have: a clone
works offline, and a snapshot that lives somewhere else does not. It also loses the review
trail — a fact entering the catalogue becomes a diff somebody can read.

**Commit a SQL dump.** Rejected: unreviewable, merge-hostile, and it would carry the working
state and the artifacts along with the facts. NDJSON diffs line by line and a human can read a
record without a database.

**Mirror the tables, one file per table.** Rejected because it exports the schema rather than
the meaning: a consumer would have to join four files to learn one thing, and every migration
would change the file layout. One self-contained fact per line survives schema changes that do
not change what a release *is*.

**Derive release identifiers from the natural key** (vendor, product, version, channel) so that
two independent collections produce mergeable snapshots. Genuinely attractive, and rejected for
now as premature: it only matters once more than one installation exports into the same file,
which the single-repository model does not do. The format version in the manifest exists so
that changing this later is a version bump rather than a silent reinterpretation.
