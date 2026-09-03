# Dataset and collector JSON Schemas

These schemas are the machine-checkable contract for everything a contributor writes by
hand. They run in CI on every pull request that touches `dataset/` or
`collectors/config/`, so a malformed registry entry fails review before a maintainer
reads it.

| Schema | Validates |
| --- | --- |
| `vendor.schema.json` | `dataset/vendors/*.yaml` |
| `product.schema.json` | `dataset/products/**/*.yaml` |
| `source.schema.json` | `dataset/sources/**/*.yaml` |
| `collector-config.schema.json` | `collectors/config/**/*.yaml` |

Three constraints in these files are worth reading before contributing, because they
encode product promises rather than mere formatting:

**A source defaults to `enabled: false` and `termsReviewStatus: pending`.** A newly
contributed source is therefore never collected until a maintainer has read the site's
terms and changed both fields deliberately. Compliance is a gate before collection, not
an audit after it. See [ADR-0018](../../docs/adr/0018-source-compliance-policy.md).

**A collector field declares its date `precision`.** A config that can only determine a
month must say `month_only`, and the collector will store a month-precision date rather
than inventing a day. See [ADR-0017](../../docs/adr/0017-version-strings-and-date-precision.md).

**`releaseType` has no default.** A collector either determines the type or emits
`unknown` and routes to classification. This is what stops every release being labelled
firmware when it is really a BIOS, a driver, or an appliance operating system.

Run the validation locally:

```bash
pip install check-jsonschema
check-jsonschema --schemafile packages/schemas/vendor.schema.json 'dataset/vendors/*.yaml'
check-jsonschema --schemafile packages/schemas/source.schema.json 'dataset/sources/**/*.yaml'
```
