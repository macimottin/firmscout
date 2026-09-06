import Link from "next/link";
import type { Product } from "@/lib/api";

/**
 * The two rows a device Product adds to the product page's definition list: its
 * vendor-published product code, and the operating system(s) it runs (ADR-0024).
 * Both fields are always present on every product, hardware or not, so an
 * operating-system product renders the same honest fallbacks the Aliases row
 * already uses rather than an absent row -- "Not a hardware model" and "Runs no
 * other cataloged product" are real, checkable claims about that product, not
 * placeholders. Extracted out of the page component so it can be exercised
 * directly against a fixture (device-facts.test.ts) without a running API.
 */
export function DeviceFacts({ product }: { product: Product }) {
  return (
    <>
      <div>
        <dt className="text-ink-500">Product code</dt>
        <dd className="mt-0.5 font-mono text-ink-900">
          {product.modelIdentifier ?? "Not a hardware model"}
        </dd>
      </div>
      <div>
        <dt className="text-ink-500">Runs</dt>
        <dd className="mt-0.5 text-ink-900">
          {product.runs.length > 0 ? (
            <ul className="space-y-0.5">
              {product.runs.map((r) => (
                <li key={r.slug}>
                  <Link href={`/products/${r.slug}`} className="hover:underline">
                    {r.name}
                  </Link>
                </li>
              ))}
            </ul>
          ) : (
            "Runs no other cataloged product"
          )}
        </dd>
      </div>
    </>
  );
}
