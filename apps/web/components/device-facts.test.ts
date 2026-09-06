/**
 * See official-sources-list.test.ts for why this is React.createElement +
 * renderToStaticMarkup in a plain .test.ts rather than JSX.
 */
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { DeviceFacts } from "./device-facts";
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

describe("DeviceFacts", () => {
  it("renders the model's product code and a real link to the operating system it runs", () => {
    const html = renderToStaticMarkup(createElement(DeviceFacts, { product: product() }));
    expect(html).toContain("CRS328-24P-4S+RM");
    expect(html).toContain('href="/products/mikrotik-routeros"');
    expect(html).toContain("RouterOS");
  });

  it("renders honest fallbacks for a product that is not a hardware model and runs nothing", () => {
    const html = renderToStaticMarkup(
      createElement(DeviceFacts, { product: product({ modelIdentifier: null, runs: [] }) }),
    );
    expect(html).toContain("Not a hardware model");
    expect(html).toContain("Runs no other cataloged product");
    expect(html).not.toContain("undefined");
  });
});
