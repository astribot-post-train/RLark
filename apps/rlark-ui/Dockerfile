# -- Base image arguments --------------------------------------------------
ARG NODE_IMAGE=node:22.20.0-bookworm-slim
ARG BASE_IMAGE=nginx:1.29.1

# -- Build stage: the console is built from the commit, not from a host's node_modules --
FROM ${NODE_IMAGE} AS build
ARG NPM_REGISTRY=https://registry.npmjs.org/
WORKDIR /src/apps/rlark-ui
COPY apps/rlark-ui/package.json apps/rlark-ui/package-lock.json ./
RUN npm ci --no-audit --no-fund --registry=${NPM_REGISTRY}
COPY apps/rlark-ui/ ./
RUN npm run build

# -- Runtime stage ---------------------------------------------------------
FROM ${BASE_IMAGE}

# Where /api/ goes. The default is the in-cluster Service rlarkadm creates; a compose
# deployment sets it to the gateway container (deploy/control-plane/rlark.compose.yaml).
ENV RLARK_GATEWAY_URL=http://rlark-gateway.rlark-system.svc:8090
COPY apps/rlark-ui/nginx.conf /etc/nginx/templates/default.conf.template
COPY --from=build /src/apps/rlark-ui/dist /usr/share/nginx/html

EXPOSE 80
