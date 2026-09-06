import { describe, expect, it } from "vitest";
import {
  describeApplicability,
  searchApplicabilityCaveat,
  searchVersionCellLabel,
} from "./applicability";
import type { Product } from "./api";

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

describe("describeApplicability", () => {
  it("names the OS for a device with an unverified applicability basis", () => {
    const notice = describeApplicability(product());
    expect(notice).not.toBeNull();
    expect(notice?.title).toContain("RouterOS");
    expect(notice?.description).toContain("RouterOS");
    expect(notice?.description.toLowerCase()).toMatch(/not verified|unverified/);
  });

  it("returns null for a product whose own releases are the ones shown", () => {
    const notice = describeApplicability(
      product({ firmwareApplicability: { verified: true, basis: "own_releases", ownReleases: { mapped: true, releaseCount: 12 } } }),
    );
    expect(notice).toBeNull();
  });

  it("never prints undefined when a device's runs edge is empty despite the unverified basis", () => {
    const notice = describeApplicability(
      product({ firmwareApplicability: { verified: false, basis: "runs_os_unverified", ownReleases: { mapped: false, releaseCount: 0 } }, runs: [] }),
    );
    expect(notice).not.toBeNull();
    expect(notice?.title).not.toContain("undefined");
    expect(notice?.description).not.toContain("undefined");
  });

  it("never prints undefined for an unrecognised basis, and still shows a caveat", () => {
    const notice = describeApplicability(
      product({ firmwareApplicability: { verified: false, basis: "something_new", ownReleases: { mapped: false, releaseCount: 0 } } }),
    );
    expect(notice).not.toBeNull();
    expect(notice?.title).not.toContain("undefined");
    expect(notice?.description).not.toContain("undefined");
  });

  // The shape ADR-0024 names explicitly: "a rack server is a device AND publishes its
  // own BIOS versions, so a product can be both". While `firmwareApplicability` was a
  // single scalar this product reported {verified: true, basis: "own_releases"} and the
  // runs_os caveat vanished. The API now reports both claims; these two tests are the
  // client half -- the sentence must state the caveat WITHOUT denying the releases the
  // page is displaying right beside it.
  it("keeps the caveat and does not deny its own releases for a product that is both", () => {
    const notice = describeApplicability(
      product({
        name: "hAP be lite",
        firmwareApplicability: {
          verified: false,
          basis: "runs_os_unverified",
          ownReleases: { mapped: true, releaseCount: 1 },
        },
      }),
    );
    expect(notice).not.toBeNull();
    expect(notice?.description).not.toContain("publishes no release stream of its own");
    expect(notice?.description).toContain("1 release");
    expect(notice?.description).toContain("RouterOS");
    expect(notice?.description.toLowerCase()).toMatch(/not verified|unverified/);
    expect(notice?.description).not.toContain("undefined");
  });

  it("does not deny its own releases when the runs edge is empty either", () => {
    const notice = describeApplicability(
      product({
        runs: [],
        firmwareApplicability: {
          verified: false,
          basis: "runs_os_unverified",
          ownReleases: { mapped: true, releaseCount: 3 },
        },
      }),
    );
    expect(notice).not.toBeNull();
    expect(notice?.description).not.toContain("publishes no release stream of its own");
    expect(notice?.description).not.toContain("undefined");
  });

  it("still says the product publishes no stream of its own when it genuinely does not", () => {
    const notice = describeApplicability(
      product({
        runs: [],
        firmwareApplicability: {
          verified: false,
          basis: "runs_os_unverified",
          ownReleases: { mapped: false, releaseCount: 0 },
        },
      }),
    );
    expect(notice?.description).toContain("publishes no release stream of its own");
  });

  it("falls back to the cautious notice for none_recorded", () => {
    const notice = describeApplicability(
      product({ firmwareApplicability: { verified: false, basis: "none_recorded", ownReleases: { mapped: false, releaseCount: 0 } }, runs: [] }),
    );
    expect(notice).not.toBeNull();
    expect(notice?.description).not.toContain("undefined");
  });
});

describe("searchVersionCellLabel", () => {
  it("renders the real latest version when one is known", () => {
    expect(searchVersionCellLabel("7.24.2")).toBe("7.24.2");
  });

  it("renders an em dash, never the OS name, for a device with no version of its own", () => {
    // DEFECT 12: this cell used to render "RouterOS" here -- an operating system's
    // name, styled exactly like a real version, in the column a hurried fleet
    // manager reads as "the version to flash". It must never present a non-version
    // as a version, regardless of what product or basis backs the row.
    expect(searchVersionCellLabel(undefined)).toBe("—");
    expect(searchVersionCellLabel(undefined)).not.toBe("RouterOS");
  });

  it("renders an em dash when there is no version, regardless of the empty string edge case", () => {
    expect(searchVersionCellLabel("")).toBe("—");
  });
});

describe("searchApplicabilityCaveat", () => {
  it("is true for a device whose firmware applicability is unverified", () => {
    expect(searchApplicabilityCaveat(product())).toBe(true);
  });

  it("is false once a product's own releases make applicability verified", () => {
    expect(
      searchApplicabilityCaveat(
        product({ firmwareApplicability: { verified: true, basis: "own_releases", ownReleases: { mapped: true, releaseCount: 12 } } }),
      ),
    ).toBe(false);
  });

  it("is false, not a guess, when the row's product detail could not be loaded", () => {
    expect(searchApplicabilityCaveat(null)).toBe(false);
  });
});
