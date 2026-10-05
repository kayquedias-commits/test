# Multi-stage build: the node stage produces the SPA bundle, nginx serves it as
# a stateless static origin. TLS termination belongs to the ingress/LB, so nginx
# listens on 80 (matching nginx.conf). No writeable volume and no shared state, so
# replicas can be scaled horizontally behind the load balancer.

##################################
# Stage 1 — build the SPA bundle
##################################
FROM node:18-alpine AS build
WORKDIR /app

# Baked into the bundle via Vite (import.meta.env reads VITE_API_BASE_URL). It must
# be the versioned gateway origin (e.g. https://api.example.org/v1) and must never
# carry credentials/secrets — anything passed here ends up inside image layers.
ARG VITE_API_BASE_URL=/v1
ENV VITE_API_BASE_URL=$VITE_API_BASE_URL

# No committed lockfile exists yet, so use npm install (which generates one) rather
# than npm ci, which hard-fails when package-lock.json is absent.
COPY package.json package-lock.json* ./
COPY . .

# npm install must run before build so tsc/vite (devDependencies) are present.
RUN npm install --no-audit --no-fund && npm run build

#################################
# Stage 2 — static SPA runtime
#################################
FROM nginx:1.25-alpine

# Runtime image contains only the immutable dist/ artifact plus the server block;
# no build toolchain, no development dependencies.
COPY --from=build /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/nginx.conf

# Non-root master process; the nginx base image already runs as the 'nginx' user.
# Binding the privileged port 80 therefore requires the orchestrator to grant
# NET_BIND_SERVICE (or set net.ipv4.ip_unprivileged_port_start=0) — never run as root.
USER nginx
EXPOSE 80

# Liveness probe of the static origin only; this is NOT a system-health/readiness
# authority — the backend /health endpoint owns dependency readiness. 'wget' in the
# alpine base is busybox wget and runs under the nginx user context.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost/ || exit 1