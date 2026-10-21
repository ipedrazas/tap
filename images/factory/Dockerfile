# tap-factory: Node 22 (same version as runner-node) running pi-durable, with
# the tools the new-agent skill needs: tapctl, tap-runner (fixtures), Python
# for runner-python agents, curl, and setpriv to drop to the sandbox user.
FROM golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev COMMIT= DATE=
RUN for cmd in tapctl runner; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
        -ldflags="-s -w -X github.com/ipedrazas/tap/pkg/version.Version=${VERSION} -X github.com/ipedrazas/tap/pkg/version.Commit=${COMMIT} -X github.com/ipedrazas/tap/pkg/version.Date=${DATE}" \
        -o /out/$cmd ./cmd/$cmd || exit 1; \
    done && mv /out/runner /out/tap-runner

FROM node:22.23.3-trixie-slim@sha256:28d03803f09ba76560e273af33beb7eda4ed68fa0698bc9bd15e9a6e35693923
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl python3 util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && command -v setpriv
WORKDIR /app
COPY factory/package.json factory/package-lock.json ./
# Exact versions from the lockfile; no install scripts run.
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund \
 # No package managers at runtime: agent tools may use the standard library only.
 && rm -rf /root/.npm /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack \
      /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack
COPY --from=build /out/tapctl /out/tap-runner /usr/local/bin/
COPY images/factory/tap-test-local /usr/local/bin/tap-test-local
COPY factory/src ./src
ENV NODE_ENV=production
ENTRYPOINT ["node", "--experimental-strip-types", "--disable-warning=ExperimentalWarning", "/app/src/main.ts"]
