import { describe, expect, it } from "vitest";
import { aliasesLabel } from "./aliases";

describe("aliasesLabel", () => {
  it("joins real aliases when the field is present and non-empty", () => {
    expect(aliasesLabel(["A42G-HbeP", "hAP be lite"])).toBe(
      "A42G-HbeP, hAP be lite",
    );
  });

  it("renders a single alias without a trailing separator", () => {
    expect(aliasesLabel(["RB4011"])).toBe("RB4011");
  });

  it("states a fact about the RESPONSE, not the catalogue, when the field is absent", () => {
    // This is the defect this function exists to prevent: the API omitting
    // `aliases` today does not mean the catalogue has none recorded for this
    // device -- hap-be-lite has two (dataset/products/mikrotik/devices/hap-be-lite.yaml)
    // that a real search hit reaches via alias match. The label must not claim
    // "no aliases recorded" for a fact that is actually just "not sent here".
    const label = aliasesLabel(undefined);
    expect(label).not.toBe("No aliases recorded");
    expect(label).toBe("Not returned by this response");
  });

  it("states a fact about the CATALOGUE when the API sends an explicit empty list", () => {
    // A genuinely empty array is the API asserting zero aliases, a real claim
    // distinct from the field being absent -- this is the one case where
    // "No aliases recorded" is actually true.
    expect(aliasesLabel([])).toBe("No aliases recorded");
  });
});
