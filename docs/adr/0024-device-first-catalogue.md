# ADR-0024: A hardware model is a Product, and per-model firmware applicability is an explicit unknown

- **Status:** Accepted
- **Date:** 2026-09-05
- **Deciders:** founding team
- **Requires qualified legal review:** no
- **Related:** ADR-0002, ADR-0016, ADR-0017, ADR-0018, ADR-0020

## Context

FirmScout catalogues RouterOS as one product and has no idea which devices run it. That is a
defensible model of a *version stream* and a useless model of the *thing a customer owns*.

A company managing a fleet holds an inventory of model numbers: 40x `CRS328-24P-4S+RM`, 12x
`hAP ax³`, 8x `PowerEdge R750`. The model number is the primary key of their world — it is
stamped on the chassis, it is what the asset register stores, and it is the only identifier they
can look up without already knowing the answer. They do not search for "RouterOS". Today, pasting
`CRS328-24P-4S+RM` into FirmScout returns nothing at all, which means the product does not yet
solve the problem it exists to solve. `blueprint.md` line 231 lists inventory upload and
comparison as a paid capability; a customer uploading forty model numbers has nothing to match
against.

The machinery to fix this is almost entirely already built and unused. `domain.Product` has
carried `ModelIdentifier` and `ProductFamilyID` since the first migration; `product_families` is a
table; the registry loader already dispatches `kind: ProductFamily`; `SyncRegistry` already has a
families pass; `release_product_mappings` already targets either a product or a family, enforced
by a `CHECK`, and `docs/architecture/data-model.md` §4 already states that this "lets a single
release row apply to one exact model, several models, a whole family, one hardware revision, or
one region". Nothing has ever populated any of it. What is genuinely missing from 31 tables is a
product-to-product relationship of any kind: nothing links a device to the operating system whose
releases the operator is actually looking for.

The design that suggested itself was: a device is a Product with a model identifier, families
group devices by the axis that decides firmware applicability, and that axis is **architecture**.
Before building it, MikroTik's published pages were measured. The measurement corrected the
premise on the point that matters most, and refuted a second, comfortable hypothesis outright.

**Architecture decides which file, not which version.** Across 18 device pages fetched on
2026-09-06 (00:43Z-00:47Z, all HTTP 200), every one of the 17 that publishes an `Architecture`
specification links to an npk whose architecture token matches that field exactly — `ARM 64bit`
→ `arm64`, `ARM 32bit` → `arm`, `MIPSBE` → `mipsbe`, `MMIPS` → `mmips`, `SMIPS` → `smips`, `PPC`
→ `ppc`. Zero mismatches. But every one of those architectures is offered the *same version*,
RouterOS 7.24.2. There is no per-architecture version stream. And architecture is not even
sufficient for the full package set: `hAP ax lite` and `hAP be lite` are both `ARM 32bit` and take
`wifi-qcom-7.24.2-arm.npk` and `wifi-mediatek-7.24.2-arm.npk` respectively, while several devices
additionally take a per-SoC RouterBOOT `.fwf` (`ar7100`, `ar9344`, `p2020`, `mt7621L`).

**The "old hardware is held on v6" hypothesis is not supported by anything MikroTik publishes.**
A live conflict sits in the development database: two official MikroTik sources report 6.49.18 and
7.24.2. The attractive explanation was that both are true of different hardware generations, and
that the catalogue calls them contradictory only because it has nowhere to say "latest *for which
device*". The recon deliberately hunted for a v6-only device to make that story demonstrable and
found none. Zero of 18 device pages mention any 6.x version — every `6.49` byte sequence in the
raw HTML is an SVG path coordinate, and a stricter `6\.49\.[0-9]+` match returns zero hits on a
page where `7.24.2` appears 189 times. A 32 MB-RAM `hAP lite` (SMIPS) and a 2007-era `RB433`
(MIPSBE, 64 MB) are both offered 7.24.2 built for their own architecture. MikroTik's own
documentation defines long-term as a maturity *promotion* of stable and frames v6→v7 as an upgrade
path through an intermediate 7.12.1, never as a hardware limit; a keyword scan of that page found
"RAM" zero times. The actual cause of the conflict is mundane and mechanical:
`mikrotik.com/download/changelogs` defaults to the long-term channel — its own embedded state
reads `channelFilter":"longTerm` — so it structurally cannot show a stable release, and FirmScout
ingested a long-term page and labelled it `stable`.

So the honest position had to be settled before any code was written: FirmScout can make a device
findable, and can say what operating system it runs, but it cannot yet say which release of that
operating system belongs on that model, because no page it fetched says so.

## Decision

**A device is a `Product` whose `model_identifier` is set. There is no devices table, no device
kind, and no device flag.**

A hardware model is a row in `products` like any other, and therefore inherits aliases, categories,
the `product_summaries` projection and full-text search unchanged. It differs from a software
product in what is filled in, not in what it is: `model_identifier` carries the vendor's published
product code verbatim, `default_release_type` is empty because the device publishes no version
stream of its own, and it has zero `release_product_mappings` rows. `domain.Product.IsHardwareModel()`
is the one place that question is answered. The test is deliberately **not** exclusive with having a
release stream: a rack server is a device *and* publishes its own BIOS versions, so a product can
be both, and a boolean forcing that choice would be wrong on the first server vendor onboarded.

**Model-number search needs no new machinery, and is guaranteed by an alias rather than a column.**
`product_summaries.search_vector` already weights `aliases_text` — built from the *raw* alias text,
not the normalised form — at B, and `SummaryRepo.Search` already ORs full-text against a `pg_trgm`
similarity. Every device document must therefore carry an alias of kind `model_number` equal to its
model identifier, and a registry test refuses to let one ship without it. The `model_identifier`
column added to `product_summaries` is for display only.

**One new table, `product_relationships`, carries the navigation edge.** It is directed
(`from_product_id` → `to_product_id`), has a one-member vocabulary (`runs_os`), cascades on delete
of the device and *restricts* on delete of the operating system, and is provenanced the way every
other registry row is — by the reviewable YAML named in `registry_path` and by Git history — rather
than by an `evidence_id`. ADR-0016 already draws that line: registry facts are provenanced by Git,
observed facts by `evidence`, and `evidence` rows are collector artefacts carrying a content hash
and a collector version that a human's pull request does not have.

**Product families are keyed on architecture, and their claim is narrowed to what was measured:
these devices are offered the same base `routeros-<version>-<arch>.npk` image file.** That is the
literal reading of `domain.ProductFamily`'s own doc comment ("products that share a release
stream… all run the same firmware image"), and it is the 17-of-17 measurement above. It is *not* a
version-stream partition and it is not the full package set. **This phase therefore writes no
family-targeted `release_product_mappings` row, and none may be added without new evidence.** Only
architectures with a registered member are declared; TILE and MMIPS families are not created for
devices nobody catalogued.

**"The latest firmware for this device" resolves to nothing, and the API says so in a key that is
always present.** A device product has no mappings, so `/products/{slug}/latest` answers 404 and
`/products/{slug}/releases` answers an empty page — both correct and both unchanged. The product
response separates three claims that have different evidence and different confidence:
`modelIdentifier` (this device exists and is this vendor's model), `runs` (it runs this operating
system), and `firmwareApplicability` (`{verified, basis}` — see the 2026-09-06 amendment
below, which adds a third member `ownReleases`), whose basis for a device is
`runs_os_unverified`. That object carries no `omitempty` and is never an omitted key, for exactly
the reason `hasSourceConflict` is always present: an absent key reads as "applies", and "we have
not verified which image this model takes" is a different claim from "every release of its
operating system applies".

**A device extends `GET /api/v1/products/{slug}`; there is no device route.** A second route would
force a caller to know which kind a slug is before fetching it, which is precisely the knowledge a
fleet manager does not have.

**The open 6.49.18/7.24.2 conflict is not touched by this change.** It is a channel misparse in a
source definition, not the missing device dimension, and correcting it is a separate change with
its own review and its own evidence. `dataset/sources/**` is out of scope for this work. ADR-0020
says a conflict is a recorded finding, never an auto-resolved guess; a conflict that evaporates
because somebody changed a schema is the same silent arbitration wearing a different hat.

**One correctness hole is closed because this change is what makes it reachable.** 00001's
`release_mappings_latest_idx` is partial on `product_id IS NOT NULL`, so family-targeted mappings
fall outside its predicate entirely and any number of them may claim to be latest for the same
family and channel; `ClearLatestFlag` cannot address one either. That has been harmless only
because the registry contained zero families. This change registers the first five, so migration
00005 adds the mirror partial unique index — the same reasoning 00003 used for review items.

## Consequences

### Positive

- A fleet manager can paste the string stamped on the chassis and land on a page that names the
  device, echoes the model code back, and links to the operating system whose releases they need.
  That is the product's stated problem, and it was previously unanswerable.
- The expensive machinery is reused rather than duplicated. Search, aliases, categories, summaries
  and the whole read path work on a device with no change, because a device is a product.
- The device/product boundary stays blurry in the data, which is honest: a Dell PowerEdge R750 is a
  device and a BIOS version stream, and this model can hold both without a discriminator that
  forces a false choice.
- The applicability gap is visible rather than implied. A page that shows a device and no version
  cannot be mistaken for a page that shows a device and its version, because the response carries
  an explicit `verified: false` and the site renders a sentence saying which claim was not made.
- Registry sync now refreshes the summary of every product it upserts, which fixes a defect that
  predates devices: a registry-only product had no `product_summaries` row at all, and was
  therefore a 404 on the public API and absent from search.
- The family half of `release_product_mappings` finally has an index that polices it, so the day
  somebody writes a family-targeted latest flag, a bug is a constraint violation rather than an
  invisible duplicate.
- The measurement is recorded with its provenance. Every device fact in the dataset carries the URL,
  the fetch timestamp and the verbatim excerpt behind it, so the next contributor can check the
  claim rather than trust it.

### Negative

- **The catalogue now contains six devices and cannot tell any of them which firmware to install.**
  That is a page a customer can reach and be disappointed by, and calling the gap explicit does not
  make it small. The next stage — a device-page collector that resolves per-model applicability —
  is not optional follow-up work; it is the half of the promise this decision defers.
- Six hand-authored devices out of roughly 210-225 real ones in MikroTik's sitemap is a catalogue
  that looks thin next to a search box. Padding it was refused, but a visitor cannot tell a
  deliberately small evidenced catalogue from an abandoned one.
- The families are a grouping nobody can currently use. Five rows exist, they are correct, and
  nothing reads them except a "Family: ARM 32bit" line on a page. A reader may reasonably ask why
  they exist at all, and the answer — that the architecture value is measured and would otherwise
  have to be refetched — is an argument about future work, not present value.
- `product_relationships` is a table with one relation kind, six rows and no reverse API. If the
  next stage discovers the edge should have carried applicability attributes (a region, a hardware
  revision, a minimum version), those columns land on a table that already has rows in it.
- A device with a model identifier but no `model_number` alias is silently unfindable. A registry
  test now prevents that in the committed dataset, but nothing prevents it for a product created
  any other way, because the invariant lives in a test rather than in the schema.
- The `runs` edge is asserted by a human reading a page, not observed by a collector, so it has
  weaker provenance than a release. It will drift when a vendor changes what a model runs, and
  nothing detects that drift.
- `SyncRegistry` now depends on a summary refresher, which couples the registry sync to the read
  projection. That is a real coupling accepted for a real reason: without it, everything this ADR
  describes returns 404.

### Neutral

- No new `Source` document ships, so ADR-0018 and `TestEverySourceShipsPendingTermsReview` are
  untouched. Cloning this repository and running its Quick Start still causes no request to any
  manufacturer's server; a `Product` or `ProductFamily` document is inert.
- ADR-0017 is unaffected. Nothing here compares or orders version strings, and no device fact has a
  date, so no `PartialDate` precision question arises.
- The product family on a product summary is now rendered as plain text rather than a link. There
  is no family page and family slugs are unique per vendor rather than globally, so a link was a
  promise the API could not keep — the site had one anyway, resolving to `/products/undefined`,
  harmless only because no family existed.
- The choice of `lifecycle_status: unknown` for every device is a deliberate refusal to interpret a
  sales badge as a firmware lifecycle. It costs the catalogue a signal it could have guessed at.

## Amendment (2026-09-06): `firmwareApplicability` carries three members, not two

The Decision above writes the object as **`firmwareApplicability` (`{verified, basis}`)**. It
now carries a third, always-present member — `ownReleases` (`{mapped, releaseCount}`) — and the
reason is a shape this ADR itself named and then failed to encode.

The Decision's own text says the hardware test is *"deliberately **not** exclusive with having a
release stream: a rack server is a device *and* publishes its own BIOS versions, so a product can
be both, and a boolean forcing that choice would be wrong on the first server vendor onboarded."*
`{verified, basis}` was that boolean wearing a different name. A single scalar has room for the
winner of a test and nothing else, and the presenter tested "has its own releases" first, so a
product that was both reported `{"verified": true, "basis": "own_releases"}` and dropped the
`runs_os` caveat entirely. Its own BIOS-shaped release vouched for an operating-system
applicability nobody had checked — precisely the substitution the *"Let the device page inherit
the operating system's latest release"* alternative was rejected to prevent, arriving through the
response shape rather than through the release lookup.

No product in the pilot dataset is currently both, so nothing served today was wrong. The defect
was that the contract could not express the case the ADR had already committed to supporting.

**What changed.** `verified` and `basis` keep their names and `basis` keeps its exact three
members, so a consumer reading only those two is unaffected. They now answer one question —
*which of ANOTHER product's releases apply to this exact model* — and `ownReleases` answers the
other — *are the releases in this response this product's own*. The runs edge is tested first, so
a mapping row can no longer retract the caveat; a caveat may only be retracted by a verification,
and until a collector resolves per-model applicability (see *Revisit when*) there is no input that
retracts it. A product that is both gets both members filled in, and its page legitimately shows a
`latestRelease` **and** the caveat: the release is its own, the caveat is about the operating
system's.

**The alternative rejected** was a fourth `basis` member meaning "own releases *and* an unverified
runs edge". It encodes a pair of claims as a product of them, so each further claim doubles the
vocabulary, and every consumer switching on `basis` mishandles the new member on the day it ships
— including this project's own web client, whose unrecognised-basis fallback would have printed
the cautious sentence for a case that needed a specific one.

**Consequence for the *Revisit when* entry below.** The first bullet says the `basis` vocabulary
grows a member when per-model applicability becomes establishable. That still holds, and this
amendment narrows it: the new member describes *how the verification was established*, and must
not describe a combination of claims. Anything that is a second claim gets a second member, the
way `ownReleases` did.

## Alternatives considered

### A first-class `devices` table

Rejected. It duplicates the read path wholesale: aliases, normalised alias matching, the search
vector, `product_summaries`, category membership and `RefreshProductSummary` would each need a
second parallel implementation, and every one of them already works on a product row today. The
measured cost of the chosen model is one table and two display columns; the measured cost of this
one is a second copy of the catalogue. It is also wrong about the domain: a rack server is a device
*and* a BIOS version stream, so the boundary a separate table draws does not exist in the world it
is modelling, and the first server vendor onboarded would need rows in both tables describing the
same object.

### Families keyed on marketing series (`hAP`, `CRS`, `CCR`)

Rejected on the measurement. `hAP ax lite` and `hAP be lite` share a marketing prefix and take
different wireless driver packages; `CRS328-24P-4S+RM` and `hAP be lite` share nothing in marketing
and take the same base image. A marketing family would group by the one axis measurement proves
irrelevant to firmware, and it contradicts `domain.ProductFamily`'s own doc comment, which is about
sharing a firmware image and not about sharing a brand.

### No families at all

Seriously considered, and rejected narrowly. It costs no Go code either way — the loader, the sync
pass and the repository methods have existed since 00001 and have never been used — so the whole
question is whether the architecture value is worth recording before anything reads it. It is: the
value is measured, verbatim-quotable and 17-of-17 consistent, and discarding it means refetching
six pages later to record a fact already in hand. The risk that a family is later mistaken for an
applicability partition is answered by narrowing its stated claim and by forbidding
family-targeted mappings in this phase, in the dataset comments and here.

### Let the device page inherit the operating system's latest release

Rejected, and it is the most dangerous alternative on this list because it is the one that looks
like a feature. A device page showing "latest: 7.24.2" with no qualification tells an operator that
this is the image for that box. FirmScout measured that MikroTik offers 7.24.2 for every
architecture *today*, and could not distinguish "this page states a per-device version" from "this
page templates the current stable release" from a single day's observation — the two are identical
until a device page is seen after the next stable release. Publishing the inherited version would
be exactly the fabricated device specification this project's rules exist to prevent, dressed up as
a convenience.

### Model the v6/v7 split as a hardware dimension on the conflict

Rejected because the evidence went the other way. The hypothesis was attractive and would have
turned an embarrassing contradiction into a feature. It did not survive contact with the pages:
no device page names a 6.x version, the lowest-memory and oldest devices in the catalogue are both
offered 7.24.2 built for their architecture, and MikroTik's documentation describes long-term as a
maturity promotion rather than a hardware boundary. Building a schema dimension to express a split
nobody publishes would have encoded a guess as structure, and — worse — would have closed an open
`source_conflicts` row as a side effect of a schema change nobody reviewed, which ADR-0020 exists
to forbid. The conflict is left open, and its real cause (a page that defaults to the long-term
channel) is recorded for a separate change.

### Add `model_identifier` to the generated search vector

Rejected. It requires dropping and recreating a `GENERATED ALWAYS` column in order to index a
string that a `model_number` alias already puts into `aliases_text` at weight B. The alias route
also has a property the column does not: a device usually has more than one string a fleet
inventory might hold — a product code, a marketing name, a keyboard-typeable spelling of a name
containing a superscript — and the alias table already models all of them.

### Give devices their own route, `GET /api/v1/devices/{slug}`

Rejected. It forks every consumer's code path and requires a caller to know whether a slug names a
device before it can fetch it, which is exactly the knowledge the caller lacks — they have a model
number, they search, they follow a link. One route serving both kinds, with additive always-present
fields, keeps a search result and a product page interchangeable.

## Revisit when

- A device-page collector resolves per-model applicability from a source that publishes it, at
  which point `firmwareApplicability.verified` becomes reachable as `true` for a device and the
  `basis` vocabulary grows a member describing how it was established.
- A device page is observed after the next RouterOS stable release, which is the observation that
  distinguishes "this page states a per-device version" from "this page templates the current
  stable" — and therefore decides whether the device-to-version edge is a fact worth storing at all.
- A vendor is onboarded whose devices genuinely take different *versions* by hardware generation,
  with published evidence. That is the case families were originally imagined for, and it would
  make a family-targeted `release_product_mappings` row meaningful for the first time — at which
  point the mirror latest-flag index added in 00005 stops being defensive and starts being load-
  bearing, and `ClearLatestFlag` needs a family sibling.
- A device is registered that runs more than one operating system FirmScout catalogues. The
  `CRS328-24P-4S+RM` already publishes `RouterOS / SwitchOS` and links images for both; only the
  RouterOS edge is recorded, because SwitchOS is not a registered product. Registering it is the
  first real test of the many-to-many shape chosen here.
- The catalogue passes a few dozen devices, at which point hand-authored YAML per model stops being
  the right authoring surface and the registry needs either a generator or a collector that writes
  candidate device documents for review.
- Inventory upload is built, which is the first consumer that needs to match an arbitrary customer
  string against `model_identifier` at scale, and will say whether the alias-driven match is
  precise enough or whether the column needs an index of its own.
