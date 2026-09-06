import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { ApplicabilityAlert } from "./applicability-alert";
import type { Product } from "@/lib/api";

function product(overrides: Partial<Product> = {}): Product {
  return {
    slug: "mikrotik-crs328-24p-4s-rm",
    name: "CRS328-24P-4S+RM",
    vendor: { slug: "mikrotik", name: "MikroTik" },
    family: { name: "ARM 32bit" },
    modelIdentifier: "CRS328-24P-4S+RM",
    aliases: ["CRS328-24P-4S+RM"],
    runs: [{ slug: "mikrotik-routeros", name: "RouterOS" }],
    firmwareApplicability: { verified: false, basis: "runs_os_unverified", ownReleases: { mapped: false, releaseCount: 0 } },
    category: "network-devices",
    releaseType: "",
    latestRelease: null,
    officialSources: [],
    lastVerifiedAt: "2026-09-06T00:46:42Z",
    hasSourceConflict: false,
    conflict: null,
    ...overrides,
  };
}

describe("ApplicabilityAlert", () => {
  it("renders the caveat, visibly, when applicability is unverified", () => {
    const html = renderToStaticMarkup(createElement(ApplicabilityAlert, { product: product() }));
    expect(html).not.toBe("");
    expect(html).toContain("RouterOS");
    expect(html.toLowerCase()).toMatch(/not verified|unverified/);
  });

  it("renders nothing when the product's own releases are the ones shown", () => {
    const html = renderToStaticMarkup(
      createElement(ApplicabilityAlert, {
        product: product({ firmwareApplicability: { verified: true, basis: "own_releases", ownReleases: { mapped: true, releaseCount: 12 } } }),
      }),
    );
    expect(html).toBe("");
  });
});
