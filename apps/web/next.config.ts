import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  reactStrictMode: true,
  // Required by infrastructure/docker/web.Dockerfile, which copies
  // .next/standalone into a slim runtime stage carrying only the traced
  // dependencies rather than the whole node_modules tree. Without it Next emits
  // no standalone directory at all and the image build fails at the COPY with
  // "/app/.next/standalone: not found".
  //
  // The Dockerfile has stated this requirement in a comment since it was written;
  // nothing applied it here, and nothing caught that, because the Compose stack had
  // never been built. `npm run build` outside Docker is unaffected either way --
  // standalone is an additional output, not a replacement for .next.
  output: "standalone",
};

export default nextConfig;
