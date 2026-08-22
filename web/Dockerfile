# Conduit dashboard — multi-stage build.
#
# Next.js `output: "standalone"` traces the modules actually imported and emits
# a self-contained server, so the runtime image carries neither the full
# node_modules tree nor the build toolchain.

# --- dependencies -----------------------------------------------------------
FROM node:22-alpine AS deps
WORKDIR /app
COPY package.json package-lock.json ./
# npm ci, not npm install: it installs exactly the lockfile and fails if the two
# have drifted, so a build cannot silently pick up a different dependency tree
# than the one that was tested.
RUN npm ci

# --- build ------------------------------------------------------------------
FROM node:22-alpine AS build
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .

ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

# --- runtime ----------------------------------------------------------------
FROM node:22-alpine AS runtime
WORKDIR /app

ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
ENV PORT=3000
ENV HOSTNAME=0.0.0.0

# The standalone output ships its own minimal node_modules; static assets are
# served from disk and are not part of it.
COPY --from=build /app/.next/standalone ./
COPY --from=build /app/.next/static ./.next/static
COPY --from=build /app/public ./public

# node:alpine provides this user; running as root would be gratuitous for a
# process that only reads from one HTTP API.
USER node

EXPOSE 3000

CMD ["node", "server.js"]
