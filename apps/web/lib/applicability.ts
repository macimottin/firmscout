/**
 * Turns a `Product`'s `firmwareApplicability` (ADR-0024) into the sentence the
 * product page and search page render, mirroring `lib/conflict.ts`: the API ships
 * a closed, machine-readable vocabulary (`basis`), and the one place that
 * vocabulary becomes prose is here, tested directly, rather than a string baked
 * into a JSON response nobody can unit test.
 *
 * The stakes are the reason this file exists at all: a fleet manager acting on an
 * unverified applicability claim is the harm FirmScout's whole "never invent a
 * fact" discipline exists to prevent, so every branch below fails toward showing
 * the caveat rather than staying silent -- including for a `basis` this client
 * does not yet recognise.
 */

import type { OwnReleases, Product } from "./api";

export interface ApplicabilityNotice {
  title: string;
  description: string;
}

const UNVERIFIED_NO_OS_NOTICE: ApplicabilityNotice = {
  title: "Firmware applicability not verified",
  description:
    "This product publishes no release stream of its own, and FirmScout has not established " +
    "which operating system's releases apply to it, or which of those releases fit this exact model.",
};

/**
 * The same case as UNVERIFIED_NO_OS_NOTICE, for a product that DOES publish releases
 * of its own. The sentence above opens "This product publishes no release stream of
 * its own", which is simply false for such a product, so it cannot be reused: the
 * caveat has to be stated without denying the releases shown further down the page.
 */
const UNVERIFIED_NO_OS_WITH_OWN_NOTICE: ApplicabilityNotice = {
  title: "Firmware applicability not verified",
  description:
    "The releases shown here are this product's own. FirmScout has not established which " +
    "operating system's releases also apply to it, or which of those fit this exact model.",
};

/** "1 release" / "2 releases", so a sentence can state the count it is talking about. */
function releaseCountPhrase(n: number): string {
  return `${n} release${n === 1 ? "" : "s"}`;
}

const NONE_RECORDED_NOTICE: ApplicabilityNotice = {
  title: "No firmware recorded for this product",
  description:
    "FirmScout has no release mapped to this product and no operating system recorded for it " +
    "either, so there is nothing yet to say applies here.",
};

/**
 * `null` means "no notice" -- `basis === "own_releases"` is the one case where
 * today's page is already correct, because the releases shown ARE mapped to this
 * product. Every other basis renders a notice, including one this client does not
 * recognise: an absent notice reads as "verified", and staying silent on an
 * unrecognised value would make exactly that false claim.
 */
export function describeApplicability(product: Product): ApplicabilityNotice | null {
  const basis = product.firmwareApplicability.basis;

  if (basis === "own_releases") return null;

  if (basis === "runs_os_unverified") {
    const os = product.runs[0]?.name;
    // `ownReleases` is required by the contract, but a response is unvalidated JSON at
    // this boundary and this file's job is to fail toward the caveat, never to throw on
    // a page a fleet manager is reading. An absent member is read as "not mapped": the
    // sentence that results states less, and stating less is the safe direction here.
    const own = product.firmwareApplicability.ownReleases as OwnReleases | undefined;
    const ownCount = own?.mapped === true ? own.releaseCount : 0;

    // A summary written before the `runs` edge landed can carry this basis with
    // an empty `runs` array -- fall back to a version naming no OS rather than
    // print "undefined" or an empty sentence.
    if (!os) return ownCount > 0 ? UNVERIFIED_NO_OS_WITH_OWN_NOTICE : UNVERIFIED_NO_OS_NOTICE;

    // A product can be BOTH a hardware model and its own release stream -- ADR-0024
    // names a rack server that publishes its own BIOS versions as exactly that shape.
    // Such a product legitimately shows a `latestRelease` AND this caveat: the release
    // is its own, and the caveat is about the operating system's. Saying "runs {os}"
    // alone next to a version the page is already displaying invites the reader to
    // conclude that version came from {os}, which is the claim nobody has verified.
    if (ownCount > 0) {
      return {
        title: `Firmware is published against ${os}`,
        description:
          `${product.name} publishes its own firmware (${releaseCountPhrase(ownCount)}), and it also ` +
          `runs ${os}. FirmScout has not verified which of ${os}'s releases apply to this exact ` +
          `model -- the version shown here is this product's own, not an answer to that question.`,
      };
    }

    return {
      title: `Firmware is published against ${os}`,
      description:
        `${product.name} runs ${os}. FirmScout has not verified which of ${os}'s releases apply ` +
        `to this exact model -- see the ${os} page for its release history.`,
    };
  }

  // "none_recorded", and any future or unrecognised basis: the cautious fallback.
  return NONE_RECORDED_NOTICE;
}

/**
 * The search page's "Latest version" cell for one product hit: the real latest
 * version when one is known, an em dash otherwise.
 *
 * This used to fall back to the name of the operating system a device runs (e.g.
 * "RouterOS") when the device had no version of its own -- meant to say "the
 * firmware lives over there", but rendered in the same monospace styling as a real
 * version string, in the column headed "Latest version", on the first screen a
 * fleet manager reaches after pasting a model number. That is a non-version
 * presented as a version: indistinguishable at a glance from "the version to
 * flash". The em dash is the same "nothing to show in this cell" convention every
 * other column in this table already uses for a missing value (vendor, release
 * date, release type, last verified). The OS-pointer information this used to
 * smuggle in here belongs in `searchApplicabilityCaveat` below instead, not
 * disguised as a version.
 */
export function searchVersionCellLabel(latestVersion: string | undefined): string {
  return latestVersion || "—";
}

/**
 * Whether a search result row must carry a visible "applicability not verified"
 * signal (ADR-0024) -- the search page is the first screen a fleet manager reaches
 * after pasting a model number, so a device whose firmware fit has not been
 * verified must say so here, not only after they click through. True only when
 * the full product detail actually loaded and says so; a product this page could
 * not enrich (the per-row detail fetch failed) stays silent rather than guess
 * either way -- the device page it links to is where that product's own honest
 * state is shown.
 */
export function searchApplicabilityCaveat(product: Product | null): boolean {
  return product !== null && product.firmwareApplicability.verified === false;
}
