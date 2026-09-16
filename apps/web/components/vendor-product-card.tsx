import Link from "next/link";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  productKindForCategory,
  secondaryModelLine,
} from "@/lib/product-kind";

export interface VendorProductCardProps {
  slug: string;
  name: string;
  category?: string;
  modelIdentifier?: string | null;
}

export function VendorProductCard({
  slug,
  name,
  category,
  modelIdentifier,
}: VendorProductCardProps) {
  const kind = productKindForCategory(category);
  const Icon = kind.icon;
  const modelLine = secondaryModelLine(name, modelIdentifier);

  return (
    <Link href={`/products/${slug}`} className="block h-full">
      <Card className="h-full transition-colors hover:border-ink-400 hover:bg-ink-50/60">
        <CardHeader className="flex flex-row items-start gap-3 space-y-0">
          <span
            className="flex h-10 w-10 shrink-0 items-center justify-center rounded-md bg-ink-100 text-ink-700"
            aria-hidden="true"
          >
            <Icon className="h-5 w-5" strokeWidth={1.75} />
          </span>
          <div className="min-w-0 flex-1">
            <p className="text-xs font-medium uppercase tracking-wide text-ink-500">
              {kind.label}
            </p>
            <CardTitle className="mt-0.5 truncate">{name}</CardTitle>
            {modelLine ? (
              <p className="mt-0.5 truncate font-mono text-xs text-ink-500">
                {modelLine}
              </p>
            ) : null}
          </div>
        </CardHeader>
        <CardContent>
          <p className="text-sm leading-snug text-ink-600">{kind.description}</p>
        </CardContent>
      </Card>
    </Link>
  );
}
