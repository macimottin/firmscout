import Link from "next/link";

export function SiteHeader() {
  return (
    <header className="border-b border-ink-200 bg-white">
      <div className="mx-auto flex h-16 max-w-5xl items-center justify-between px-4 sm:px-6">
        <Link href="/" className="flex items-center gap-2 font-semibold text-ink-950">
          <span
            aria-hidden="true"
            className="flex h-7 w-7 items-center justify-center rounded bg-ink-950 text-xs font-bold text-white"
          >
            FS
          </span>
          <span>FirmScout</span>
        </Link>
        <nav aria-label="Primary" className="flex items-center gap-6 text-sm font-medium text-ink-600">
          <Link href="/vendors" className="hover:text-ink-950">
            Vendors
          </Link>
          <Link href="/search" className="hover:text-ink-950">
            Search
          </Link>
          <Link href="/status" className="hover:text-ink-950">
            Status
          </Link>
        </nav>
      </div>
    </header>
  );
}
