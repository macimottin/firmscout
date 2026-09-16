/**
 * Presentation helpers for a product's category slug on browse surfaces
 * (vendor product cards, and anything else that needs a type label + icon).
 *
 * Labels and descriptions are the controlled vocabulary from
 * `dataset/categories.yaml` — they are not invented per product. Icons are a
 * UI affordance only: they never appear on the wire and never change a fact.
 */

import type { LucideIcon } from "lucide-react";
import { Cpu, Network, Router, Server, Shield, Wifi } from "lucide-react";

export interface ProductKind {
  /** Short label shown next to the icon. */
  label: string;
  /** One-sentence type description from the category vocabulary. */
  description: string;
  icon: LucideIcon;
}

const KINDS: Record<string, ProductKind> = {
  "network-operating-system": {
    label: "Network operating system",
    description:
      "Operating system shipped on networking hardware, released as its own version stream.",
    icon: Cpu,
  },
  // Test fixtures and older summaries used an underscore form; treat it the same.
  network_device_os: {
    label: "Network operating system",
    description:
      "Operating system shipped on networking hardware, released as its own version stream.",
    icon: Cpu,
  },
  "network-devices": {
    label: "Network device",
    description:
      "Physical networking hardware catalogued by model, so a fleet inventory can look it up directly.",
    icon: Server,
  },
  switches: {
    label: "Switch",
    description: "Ethernet switch, as classified by its manufacturer.",
    // Lucide has no dedicated switch glyph; Network is the least misleading stand-in.
    icon: Network,
  },
  routers: {
    label: "Router",
    description: "Ethernet router, as classified by its manufacturer.",
    icon: Router,
  },
  "wireless-devices": {
    label: "Wireless device",
    description:
      "Device whose manufacturer classifies it by its wireless function.",
    icon: Wifi,
  },
  firewalls: {
    label: "Firewall / security appliance",
    description: "Perimeter or inline security appliance, physical or virtual.",
    icon: Shield,
  },
  networking: {
    label: "Networking",
    description:
      "Routers, switches, wireless access points and the software that runs on them.",
    icon: Network,
  },
  servers: {
    label: "Server",
    description: "Rack, tower or blade server.",
    icon: Server,
  },
  storage: {
    label: "Storage",
    description: "Storage array, NAS device or storage operating system.",
    icon: Server,
  },
  "collaboration-devices": {
    label: "Collaboration device",
    description: "Video bar, codec, room controller or related platform software.",
    icon: Wifi,
  },
  "power-and-ups": {
    label: "Power / UPS",
    description: "UPS, PDU or management card.",
    icon: Shield,
  },
};

const UNKNOWN: ProductKind = {
  label: "Product",
  description: "Catalogue entry — type not recorded for this product.",
  icon: Server,
};

/**
 * Resolve a category slug from the API into a display kind. Unknown slugs fall
 * back to a generic label rather than inventing a type the registry never set.
 */
export function productKindForCategory(category: string | undefined | null): ProductKind {
  if (!category) return UNKNOWN;
  return KINDS[category] ?? {
    label: category.replace(/-/g, " "),
    description: "Classification recorded in the catalogue for this product.",
    icon: Server,
  };
}

/** Prefer showing the vendor product code when it differs from the marketing name. */
export function secondaryModelLine(
  name: string,
  modelIdentifier: string | null | undefined,
): string | null {
  const code = modelIdentifier?.trim();
  if (!code) return null;
  if (code === name.trim()) return null;
  return code;
}
