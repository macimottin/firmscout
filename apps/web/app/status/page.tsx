import type { Metadata } from "next";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { checkHealth } from "@/lib/api";

export const metadata: Metadata = {
  title: "System status",
  description: "Current status of the FirmScout API and public website.",
  alternates: { canonical: "/status" },
};

export const revalidate = 0;

const statusCopy: Record<
  "ok" | "degraded" | "unavailable",
  { label: string; variant: "success" | "warning" | "danger"; detail: string }
> = {
  ok: {
    label: "Operational",
    variant: "success",
    detail: "The catalogue API is reachable and responding.",
  },
  degraded: {
    label: "Degraded",
    variant: "warning",
    detail: "The catalogue API is reachable but reporting a degraded state.",
  },
  unavailable: {
    label: "Unavailable",
    variant: "danger",
    detail:
      "The catalogue API could not be reached. Pages fall back to a clear unavailable state rather than showing stale or fabricated data.",
  },
};

export default async function StatusPage() {
  const health = await checkHealth();
  const api = statusCopy[health.status];

  return (
    <div className="mx-auto max-w-2xl px-4 py-12 sm:px-6">
      <h1 className="text-2xl font-bold tracking-tight text-ink-950">System status</h1>
      <p className="mt-2 text-sm text-ink-500">
        FirmScout is pre-alpha. This page reflects a live check of the catalogue API, not a
        historical uptime record.
      </p>

      <div className="mt-8 space-y-4">
        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0">
            <CardTitle>Catalogue API</CardTitle>
            <Badge variant={api.variant}>{api.label}</Badge>
          </CardHeader>
          <CardContent className="text-sm text-ink-600">{api.detail}</CardContent>
        </Card>

        <Card>
          <CardHeader className="flex flex-row items-center justify-between space-y-0">
            <CardTitle>Public website</CardTitle>
            <Badge variant="success">Operational</Badge>
          </CardHeader>
          <CardContent className="text-sm text-ink-600">
            If you can see this page, the website is serving requests.
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
