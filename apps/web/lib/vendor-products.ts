/**
 * The vendor page's "Products" count (app/vendors/[slug]/page.tsx).
 *
 * `Vendor.productCount` (api.ts) is never sent by the vendor read model today, so
 * rendering it always produced the same fixed "Not recorded" string -- directly
 * above the list of products the page was, at that very moment, rendering.
 * "Not recorded" was true of the API response; it was still a defect, because a
 * page contradicting itself inside one viewport misleads regardless of which half
 * is technically accurate.
 *
 * There is no honest *aggregate* to substitute: `products` here is itself a
 * best-effort search match (see `findVendorProducts`'s doc comment), not a
 * verified total of everything this vendor has in the catalogue. So this
 * deliberately does not try to answer "how many products does this vendor have" --
 * it answers "how many rows are in the list below", which is the one count this
 * page can state without inventing anything, and the page's label says exactly
 * that rather than implying a verified total.
 */

export function productsListedCount(products: unknown[] | null): string {
  return products === null ? "Unavailable" : String(products.length);
}

/**
 * The vendor LIST page's per-card subtitle (app/vendors/page.tsx) -- the same
 * never-sent `Vendor.productCount` as above, in the other file that reads it.
 *
 * That card rendered `{vendor.productCount} product{s}` completely unguarded. Because
 * React renders `undefined` as nothing rather than as the word, the visible result was
 * a subtitle reading " products" on every card: a count claim with the count missing,
 * which reads as a rendering fault to anyone who notices and as "no products" to
 * anyone who does not. Returning `null` lets the card omit the line entirely, which is
 * the honest rendering of a number the API does not send -- and the moment the vendor
 * read model starts sending one, the count appears with no further change here.
 *
 * The vendor list has no products array to fall back on the way the detail page does:
 * `listVendors` returns vendors only. So there is genuinely nothing to count here, and
 * inventing a substitute -- "1+", the number of cards, the catalogue total -- would be
 * a fabricated aggregate, which is the one thing this codebase never does.
 */
export function vendorProductCountLabel(productCount: number | undefined): string | null {
  if (productCount === undefined) return null;
  return `${productCount} product${productCount === 1 ? "" : "s"}`;
}
