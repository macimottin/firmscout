## What does this PR do?

<!-- One or two sentences. If it fixes an issue, reference it: "Fixes #123". -->

## Type of change

<!-- Check all that apply. -->

- [ ] New vendor / product / alias
- [ ] New source
- [ ] Collector config (YAML)
- [ ] Code-based collector
- [ ] Release correction (see `docs/collectors/dataset-correction-policy.md`)
- [ ] Parser / extraction fix
- [ ] Test fixtures
- [ ] Security advisory mapping
- [ ] Application / platform code
- [ ] Documentation
- [ ] Other (describe below)

## Checklist

- [ ] **Tests pass locally** (`go test ./...`, and `npm run lint && npm run typecheck` for `apps/web` changes).
- [ ] **Fixtures include provenance**, if this PR adds or changes a collector: source URL, retrieval timestamp, and content hash recorded in a `README` alongside the fixture (see `CONTRIBUTING.md`). No test in this PR depends on a live vendor site.
- [ ] **Schema validation passes**, if this PR touches `dataset/` or `collectors/config/` YAML (validated against `packages/schemas/`).
- [ ] **Compliance reviewed**, if this PR registers a new source: `robots_policy_status` and `terms_review_status` are filled in honestly, not assumed, and the source stays `enabled: false` unless both are actually satisfied (see `DATA_SOURCES.md`).
- [ ] **Docs updated**, if this PR changes behavior, adds a capability, or changes a policy this documentation set describes.
- [ ] **Diagrams updated**, if this PR changes anything the Mermaid diagrams in `docs/diagrams/` depict — and `bash scripts/check-mermaid.sh` passes.
- [ ] **Commits are signed off** (DCO): every commit includes a `Signed-off-by:` trailer (`git commit -s`). See `CONTRIBUTING.md#developer-certificate-of-origin-not-a-cla`.

## Anything reviewers should pay extra attention to?

<!-- Ambiguous judgment calls, assumptions you made, or anything you're not
     fully confident about. It's fine to say "I wasn't sure about X." -->
