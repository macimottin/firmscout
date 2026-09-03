# syntax=docker/dockerfile:1.7
#
# Multi-stage build for the FirmScout Next.js public site (apps/web).
#
# Requires apps/web/next.config.{js,ts,mjs} to set `output: "standalone"` --
# without it, `.next/standalone` (copied into the runtime stage below) will
# not exist and this build will fail at the final COPY. apps/web currently
# only has a package.json; next.config.* and the rest of the app land
# separately.
#
# Build (context must be the repository root):
#   docker build -f infrastructure/docker/web.Dockerfile -t firmscout/web:dev .

ARG NODE_VERSION=22

# ---------------------------------------------------------------------------
# Stage 1: deps -- install dependencies with a cached, reproducible `npm ci`.
# ---------------------------------------------------------------------------
FROM node:${NODE_VERSION}-alpine AS deps

# libc6-compat is the standard addition for Next.js on Alpine: some native
# deps it or its own deps pull in (e.g. image optimisation libraries) expect
# glibc-compatible shims that musl alone doesn't provide.
RUN apk add --no-cache libc6-compat

WORKDIR /app
COPY apps/web/package.json apps/web/package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci

# ---------------------------------------------------------------------------
# Stage 2: builder -- compile the standalone Next.js server.
# ---------------------------------------------------------------------------
FROM node:${NODE_VERSION}-alpine AS builder

WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY apps/web/ ./

# The web app calls the API server-side (and, where needed, proxies to it)
# using this base URL. In Compose, `api` resolves via the service DNS name
# on the shared network; overridden per environment via --build-arg or a
# runtime env var where the framework supports runtime configuration.
ARG FIRMSCOUT_API_URL=http://api:8080
ENV FIRMSCOUT_API_URL=${FIRMSCOUT_API_URL}
ENV NEXT_TELEMETRY_DISABLED=1

RUN npm run build

# ---------------------------------------------------------------------------
# Stage 3: runner -- minimal non-root runtime image.
# ---------------------------------------------------------------------------
FROM node:${NODE_VERSION}-alpine AS runner

RUN apk add --no-cache libc6-compat

WORKDIR /app

ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
ENV PORT=3000
ENV HOSTNAME=0.0.0.0

# Non-root runtime user -- Alpine's node image does not ship one by default.
RUN addgroup -S -g 1001 nodejs \
    && adduser -S -u 1001 -G nodejs nextjs

# `next build` with `output: "standalone"` emits a minimal server bundle
# (node_modules pruned to only what's actually required at runtime) into
# .next/standalone, plus static assets that must be copied alongside it --
# see https://nextjs.org/docs/app/api-reference/config/next-config-js/output.
COPY --from=builder /app/public ./public
COPY --from=builder --chown=nextjs:nodejs /app/.next/standalone ./
COPY --from=builder --chown=nextjs:nodejs /app/.next/static ./.next/static

USER nextjs

EXPOSE 3000

# Unlike go.Dockerfile's distroless base, this runtime image is plain Alpine
# and has a shell, so a conventional CMD-SHELL-style HEALTHCHECK works here.
# wget is present in Alpine's busybox by default.
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 \
  CMD wget -q --spider http://127.0.0.1:3000/ || exit 1

CMD ["node", "server.js"]
