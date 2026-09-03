"use client";

import { useEffect } from "react";
import { Button } from "@/components/ui/button";

export default function GlobalError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    // Log to the console so it's visible in server/browser logs; a real
    // deployment would wire this into the observability stack instead.
    console.error(error);
  }, [error]);

  return (
    <div className="mx-auto max-w-xl px-4 py-24 text-center sm:px-6">
      <h1 className="text-3xl font-bold tracking-tight text-ink-950">Something went wrong</h1>
      <p className="mt-3 text-ink-600">
        An unexpected error occurred while rendering this page. This has been logged; please try
        again.
      </p>
      <div className="mt-8">
        <Button onClick={() => reset()}>Try again</Button>
      </div>
    </div>
  );
}
