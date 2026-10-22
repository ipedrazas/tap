FROM golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev COMMIT= DATE=
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags="-s -w -X github.com/ipedrazas/tap/pkg/version.Version=${VERSION} -X github.com/ipedrazas/tap/pkg/version.Commit=${COMMIT} -X github.com/ipedrazas/tap/pkg/version.Date=${DATE}" \
    -o /out/tap-runner ./cmd/runner

FROM node:22.23.3-trixie-slim@sha256:28d03803f09ba76560e273af33beb7eda4ed68fa0698bc9bd15e9a6e35693923
# npm, corepack and yarn are removed: dependencies are vendored into bundles
# at build time. apt and dpkg remain, but the root filesystem is read-only and
# tools run as unprivileged uids, so nothing can be installed at runtime.
RUN rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack /opt/yarn* /usr/local/bin/yarn /usr/local/bin/yarnpkg
RUN mkdir -p /etc/tap && printf '%s\n' node > /etc/tap/interpreters
COPY --from=build /out/tap-runner /usr/local/bin/tap-runner
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/tap-runner"]
CMD ["serve"]
