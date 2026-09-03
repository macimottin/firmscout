import type { Metadata } from "next";
import { SiteHeader } from "@/components/site-header";
import { SiteFooter } from "@/components/site-footer";
import "./globals.css";

const siteUrl = process.env.NEXT_PUBLIC_SITE_URL ?? "https://firmscout.dev";

export const metadata: Metadata = {
  metadataBase: new URL(siteUrl),
  title: {
    default: "FirmScout — firmware and version metadata catalogue",
    template: "%s | FirmScout",
  },
  description:
    "FirmScout is an open catalogue and continuous monitor for the version metadata of firmware, BIOS, BMC images, drivers, embedded operating systems, and appliance software — with evidence attached.",
  applicationName: "FirmScout",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body className="flex min-h-screen flex-col antialiased">
        <a href="#main-content" className="skip-link">
          Skip to content
        </a>
        <SiteHeader />
        <main id="main-content" className="flex-1">
          {children}
        </main>
        <SiteFooter />
      </body>
    </html>
  );
}
