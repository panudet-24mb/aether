# Production image for the Aether web front end.
#
# Two things matter here and are easy to get wrong:
#   1. VITE_AETHER_API_ORIGIN is inlined into the browser bundle at BUILD time. In production the
#      front end and the API share one origin behind Caddy, so it must be EMPTY: the app then calls
#      /api/v1 and wss://<host>/ws relative to the page. .dockerignore keeps the developer's
#      frontend/.env.local (which points at localhost) out of the build context.
#   2. `vinext start` only needs dist/ plus the vinext runtime. The full dev tree is ~1 GB, so the
#      runtime closure is installed separately from the exact version pinned in package.json.
FROM node:22.22-alpine AS build
WORKDIR /src
ENV npm_config_audit=false npm_config_fund=false npm_config_update_notifier=false
# Dependency layer: only the manifests, so application edits do not re-run npm ci.
COPY package.json package-lock.json .npmrc ./
COPY scripts ./scripts
RUN npm run install:ci
COPY . .
# Same-origin deployment: no API origin is baked into the bundle.
ENV VITE_AETHER_API_ORIGIN="" NODE_ENV=production
RUN npm run build
# Runtime closure: vinext at exactly the version this build used, without the dev toolchain.
RUN mkdir -p /runtime && cd /runtime \
 && printf '{"name":"aether-web","private":true,"type":"module"}\n' > package.json \
 && npm install --omit=dev --no-audit --no-fund --no-save \
      "vinext@$(node -p "require('/src/package.json').devDependencies.vinext")"

FROM node:22.22-alpine AS runtime
WORKDIR /app
ENV NODE_ENV=production PORT=3000
COPY --from=build --chown=root:root /runtime/package.json ./package.json
COPY --from=build --chown=root:root /runtime/node_modules ./node_modules
COPY --from=build --chown=root:root /src/dist ./dist
# Nothing in /app is written at runtime; the image also runs with a read-only root filesystem.
USER node
EXPOSE 3000
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=5 \
  CMD node -e "fetch('http://127.0.0.1:'+(process.env.PORT||3000)+'/').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"
ENTRYPOINT ["node", "node_modules/vinext/dist/cli.js", "start"]
CMD ["--port", "3000", "--hostname", "0.0.0.0"]
