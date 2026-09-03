import { Search } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";

/**
 * A plain server-rendered `<form method="get">` — search works with no
 * JavaScript at all, because it navigates to /search?q=... like any link.
 */
export function SearchForm({
  defaultValue = "",
  size = "default",
}: {
  defaultValue?: string;
  size?: "default" | "lg";
}) {
  const inputHeight = size === "lg" ? "h-14 text-base" : "h-11 text-sm";
  return (
    <form action="/search" method="get" role="search" className="flex w-full gap-2">
      <div className="relative flex-1">
        <Search
          aria-hidden="true"
          className="pointer-events-none absolute left-3.5 top-1/2 h-4 w-4 -translate-y-1/2 text-ink-400"
        />
        <Input
          type="search"
          name="q"
          defaultValue={defaultValue}
          aria-label="Search vendors and products"
          placeholder="Search a vendor or product, e.g. MikroTik RouterOS"
          className={`pl-10 ${inputHeight}`}
          minLength={1}
          maxLength={200}
        />
      </div>
      <Button type="submit" size={size === "lg" ? "lg" : "default"}>
        Search
      </Button>
    </form>
  );
}
