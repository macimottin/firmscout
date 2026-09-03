# Alias resolution

This diagram answers: when an incoming product name or model string arrives — from a collector's extracted candidate, or from a search/lookup API call — how does FirmScout decide which catalogue product it identifies, and when does it refuse to guess?

```mermaid
flowchart TD
    incoming["Incoming product/model string<br/>(from a source extraction<br/>or an API query)"]

    incoming --> exact_slug["Exact slug match<br/>(string equals a product.slug<br/>after case-fold only)"]

    exact_slug --> slug_hit{"Match found?"}
    slug_hit -->|"yes"| unique_result

    slug_hit -->|"no"| alias_table["Alias table match<br/>(exact lookup in<br/>product_aliases)"]

    alias_table --> alias_hit{"Match found?"}
    alias_hit -->|"yes, exactly one product"| unique_result
    alias_hit -->|"yes, multiple products<br/>share this alias"| ambiguous
    alias_hit -->|"no"| normalized_match

    normalized_match["Normalised-form match<br/>(lowercase, strip punctuation,<br/>collapse whitespace, unify<br/>common separators: '-', '_', '/')"]

    normalized_match --> norm_hit{"Match found?"}
    norm_hit -->|"yes, exactly one product"| unique_result
    norm_hit -->|"yes, multiple products"| ambiguous
    norm_hit -->|"no"| trigram_match

    trigram_match["Trigram fuzzy match<br/>(pg_trgm similarity over<br/>slugs + aliases + normalised names,<br/>above similarity threshold)"]

    trigram_match --> trigram_hit{"Candidates above<br/>threshold?"}
    trigram_hit -->|"none"| no_match
    trigram_hit -->|"exactly one, with a clear<br/>similarity margin over the next-best"| ambiguity_check
    trigram_hit -->|"multiple, similarity scores<br/>close to each other"| ambiguous

    ambiguity_check["Ambiguity check<br/>(is the top match's margin<br/>over the runner-up large enough<br/>to trust automatically?)"]

    ambiguity_check --> margin{"Margin sufficient?"}
    margin -->|"yes"| unique_result["Outcome: unique match<br/>(product identity resolved)"]
    margin -->|"no — scores too close to call"| ambiguous

    ambiguous["Outcome: ambiguous match<br/>ALWAYS human review.<br/>Never a guess, never<br/>auto-resolved by highest score alone."]

    no_match["Outcome: no match"]

    unique_result --> proceed["Proceeds to the next<br/>pipeline stage (validation,<br/>API response, etc.)<br/>with the resolved product id"]

    ambiguous --> review_item["Creates a review item<br/>for maintainer disambiguation"]

    no_match --> review_item2["Creates a review item<br/>(candidate: new product<br/>or unmapped alias)"]
    no_match --> missing_analytics["Feeds 'missing product'<br/>analytics<br/>(tracks demand for products<br/>not yet catalogued)"]
```

## What this shows

Resolution proceeds through increasingly permissive match strategies — exact slug, exact alias, normalised-form, then trigram fuzzy — each one tried only after the stricter ones fail, and each one capable of independently producing an ambiguous result if more than one product qualifies. The pipeline has exactly three terminal outcomes: a unique match that can proceed automatically, a no-match that both opens a review item and feeds product-demand analytics, and an ambiguous match that is never resolved by picking the highest-scoring candidate — it always goes to a human.

## Assumptions

- The alias table (`product_aliases`) is registry data synchronised from Git, so adding a known alternate name or model string is a reviewable pull request, not a runtime side effect of resolution.
- Trigram similarity thresholds and the ambiguity margin are tuned parameters, not domain constants — they live in configuration and are expected to be revisited as false-positive/false-negative rates are measured (T3-style validation).
- A resolution performed for an API search query and a resolution performed for a collector's extracted candidate share the same pipeline; the only difference is what happens on `ambiguous` (a search response can surface disambiguation choices to the caller, while a candidate's ambiguous match always creates a `review_items` row).
- "No match" and "ambiguous match" are semantically different: no match means the catalogue plausibly lacks this product; ambiguous match means the catalogue plausibly has it, but resolution cannot tell which entry.

## Failure modes

- Two genuinely distinct products with near-identical model strings (a common pattern in rebadged OEM hardware) will legitimately and repeatedly hit `ambiguous`. This is intended, not a bug to silently work around — an accumulation of the same ambiguous pair is itself a signal that the alias table needs a disambiguating rule (e.g. a hardware-revision-qualified alias).
- A trigram threshold set too low turns near-misses into false unique matches (wrong product silently resolved); set too high, it turns real matches into no-match noise. Both directions are why the ambiguity-margin check exists as a second, independent guard on top of the raw similarity threshold.
- A collector normalisation bug could feed a string with the vendor name embedded (e.g. "MikroTik RouterOS RB750" instead of the catalogued model token), which the normalised-form pass may or may not catch depending on the separator rules — a resolution near-miss here is symptomatically similar to an alias table gap and is triaged the same way.

## Related ADRs

- [ADR-0016 — Hybrid dataset](../adr/0016-hybrid-dataset.md)

## Implementing code

**Partially implemented.**

- `internal/adapters/postgres/product_repo.go` — slug and alias matching
- `internal/application/ingest.go` — the ambiguity branch
- `internal/domain/slug.go` — `NormalizeAlias`

Trigram fuzzy matching is used by search but not by product resolution.

See [the architecture consistency report](../architecture/consistency-report.md) for what is verified by execution and what is not.
