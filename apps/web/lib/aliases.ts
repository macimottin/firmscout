/**
 * The product page's "Aliases" row (`Product.aliases`, api.ts).
 *
 * The contract now guarantees the key: `aliases` is in `Product.required` in
 * `docs/api/openapi.yaml`, the presenter builds a non-nil slice for every product,
 * and `api.ts` types it `string[]` rather than `string[] | undefined`. This function
 * still takes an optional argument, and that is deliberate rather than left over: a
 * JSON response is unvalidated at the boundary, and the honest reading of a key that
 * did not arrive is different from the honest reading of a key that arrived empty.
 *
 * An absent key says nothing about the catalogue. It is silent about whether this
 * product has aliases -- only that this particular response did not carry the field.
 * A key sent as `[]` is the API affirmatively asserting the catalogue holds none.
 * Rendering "No aliases recorded" for the first case would assert a fact about the
 * CATALOGUE ("no aliases exist") when the true fact is about the RESPONSE ("this
 * reply omitted them") -- the same false-precision defect the sibling "Source
 * quality: Not yet assessed" and "Last verified: Not verified by a source" rows are
 * already careful to avoid. It mattered concretely: `hAP be lite` really has two
 * aliases, one of which is the model-number search hit that led the reader here, and
 * the page told them none were recorded.
 *
 * Kept as its own tested function, rather than an inline ternary, because that
 * distinction is exactly the kind of thing a later edit quietly collapses into one
 * branch.
 */

import type { Product } from "./api";

export function aliasesLabel(aliases: Product["aliases"] | undefined): string {
  if (aliases === undefined) return "Not returned by this response";
  if (aliases.length === 0) return "No aliases recorded";
  return aliases.join(", ");
}
