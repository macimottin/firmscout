import { SearchForm } from "@/components/search-form";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";

export default function HomePage() {
  return (
    <div className="mx-auto max-w-3xl px-4 py-16 sm:px-6 sm:py-24">
      <div className="text-center">
        <h1 className="text-3xl font-bold tracking-tight text-ink-950 sm:text-4xl">
          What version is that firmware really on — and when did it actually ship?
        </h1>
        <p className="mx-auto mt-4 max-w-xl text-lg text-ink-600">
          FirmScout is a free, evidence-backed catalogue of firmware, BIOS, BMC, driver, and
          embedded OS versions, sourced directly from official vendor publications.
        </p>
      </div>

      <div className="mt-10">
        <SearchForm size="lg" />
      </div>

      <div className="mt-12">
        <Alert variant="info">
          <AlertTitle>Early development</AlertTitle>
          <AlertDescription>
            FirmScout is pre-alpha. The catalogue currently covers a small, growing set of
            vendors and products while the collection pipeline is built out — coverage will be
            thin for now, but every fact shown carries an official source and a verification
            timestamp. See the{" "}
            <a href="/status" className="underline underline-offset-2 hover:text-ink-950">
              status page
            </a>{" "}
            for what&apos;s live today.
          </AlertDescription>
        </Alert>
      </div>

      <dl className="mt-14 grid grid-cols-1 gap-8 border-t border-ink-200 pt-10 sm:grid-cols-3">
        <div>
          <dt className="text-sm font-semibold text-ink-950">Evidence, not guesses</dt>
          <dd className="mt-1 text-sm text-ink-600">
            Every published fact links back to the official source it came from and the moment
            it was last verified.
          </dd>
        </div>
        <div>
          <dt className="text-sm font-semibold text-ink-950">No invented dates</dt>
          <dd className="mt-1 text-sm text-ink-600">
            A release dated only &ldquo;February 2026&rdquo; is shown as exactly that — never
            padded into a fabricated day.
          </dd>
        </div>
        <div>
          <dt className="text-sm font-semibold text-ink-950">Free, indexable, honest</dt>
          <dd className="mt-1 text-sm text-ink-600">
            The public catalogue is never crippled to push a paid tier — the free/paid line is
            history depth and automation, not correctness.
          </dd>
        </div>
      </dl>
    </div>
  );
}
