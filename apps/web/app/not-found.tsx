import Link from "next/link";
import { SearchForm } from "@/components/search-form";

export default function NotFound() {
  return (
    <div className="mx-auto max-w-xl px-4 py-24 text-center sm:px-6">
      <h1 className="text-3xl font-bold tracking-tight text-ink-950">Page not found</h1>
      <p className="mt-3 text-ink-600">
        We couldn&apos;t find what you were looking for. It may have moved, or it may not be in
        the catalogue yet.
      </p>
      <div className="mt-8">
        <SearchForm />
      </div>
      <p className="mt-6 text-sm text-ink-500">
        <Link href="/" className="underline-offset-2 hover:underline">
          Back to home
        </Link>
      </p>
    </div>
  );
}
