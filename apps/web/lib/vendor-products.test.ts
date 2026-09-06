import { describe, expect, it } from "vitest";
import { productsListedCount, vendorProductCountLabel } from "./vendor-products";

describe("productsListedCount", () => {
  it("reports the real length of the list the page is about to render", () => {
    expect(productsListedCount([{ a: 1 }, { a: 2 }, { a: 3 }])).toBe("3");
  });

  it("reports zero honestly rather than a placeholder, when the search returned no rows", () => {
    expect(productsListedCount([])).toBe("0");
  });

  it("reports Unavailable, not a stale or fabricated count, when the list itself failed to load", () => {
    // This is the branch that used to render "Not recorded" directly above a
    // rendered list of products -- a page contradicting itself in one viewport.
    // `null` here mirrors findVendorProducts's own "could not load" signal, so
    // the count and the list beneath it always agree on whether data loaded.
    expect(productsListedCount(null)).toBe("Unavailable");
    expect(productsListedCount(null)).not.toBe("Not recorded");
  });
});

describe("vendorProductCountLabel", () => {
  it("omits the line entirely when the API sends no count", () => {
    // The live case: `productCount` is never sent, and the card used to render a bare
    // " products" because React prints `undefined` as nothing.
    expect(vendorProductCountLabel(undefined)).toBeNull();
  });

  it("renders the real count, correctly pluralised, once the API sends one", () => {
    expect(vendorProductCountLabel(1)).toBe("1 product");
    expect(vendorProductCountLabel(7)).toBe("7 products");
  });

  it("renders an honest zero rather than treating it as absent", () => {
    expect(vendorProductCountLabel(0)).toBe("0 products");
  });
});
