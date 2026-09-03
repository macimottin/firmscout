import Link from "next/link";

export function SiteFooter() {
  return (
    <footer className="border-t border-ink-200 bg-white">
      <div className="mx-auto max-w-5xl px-4 py-8 text-sm text-ink-500 sm:px-6">
        <p className="max-w-2xl">
          FirmScout is pre-alpha and under active construction. Catalogue coverage is
          intentionally small right now — see the{" "}
          <Link href="/status" className="underline underline-offset-2 hover:text-ink-800">
            system status page
          </Link>{" "}
          for what&apos;s built so far.
        </p>
        <nav aria-label="Footer" className="mt-4 flex flex-wrap gap-x-6 gap-y-2">
          <Link href="/vendors" className="hover:text-ink-800">
            Vendors
          </Link>
          <Link href="/status" className="hover:text-ink-800">
            System status
          </Link>
        </nav>
      </div>
    </footer>
  );
}
