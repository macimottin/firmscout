import { describe, expect, it } from "vitest";
import {
  productKindForCategory,
  secondaryModelLine,
} from "./product-kind";

describe("productKindForCategory", () => {
  it("maps leaf device categories to the registry vocabulary", () => {
    expect(productKindForCategory("switches").label).toBe("Switch");
    expect(productKindForCategory("wireless-devices").label).toBe("Wireless device");
    expect(productKindForCategory("routers").label).toBe("Router");
    expect(productKindForCategory("network-operating-system").label).toBe(
      "Network operating system",
    );
  });

  it("does not invent a type when the API sent no category", () => {
    expect(productKindForCategory(undefined).label).toBe("Product");
    expect(productKindForCategory(null).label).toBe("Product");
  });

  it("falls back without crashing on an unrecognised slug", () => {
    const kind = productKindForCategory("something-new");
    expect(kind.label).toBe("something new");
    expect(kind.description.length).toBeGreaterThan(0);
  });
});

describe("secondaryModelLine", () => {
  it("hides the code when it is already the product name", () => {
    expect(secondaryModelLine("CRS328-24P-4S+RM", "CRS328-24P-4S+RM")).toBeNull();
  });

  it("shows the code when it differs from the marketing name", () => {
    expect(secondaryModelLine("hAP ax³", "C53UiG+5HPaxD2HPaxD")).toBe(
      "C53UiG+5HPaxD2HPaxD",
    );
  });

  it("returns null when there is no model identifier", () => {
    expect(secondaryModelLine("RouterOS", null)).toBeNull();
    expect(secondaryModelLine("RouterOS", undefined)).toBeNull();
  });
});
