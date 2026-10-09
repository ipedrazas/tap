FROM golang:1.27.1-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev COMMIT= DATE=
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags="-s -w -X github.com/ipedrazas/tap/pkg/version.Version=${VERSION} -X github.com/ipedrazas/tap/pkg/version.Commit=${COMMIT} -X github.com/ipedrazas/tap/pkg/version.Date=${DATE}" \
    -o /out/tap-runner ./cmd/runner

FROM python:3.13.16-slim-trixie
# Package managers are removed: dependencies are vendored into bundles at
# build time and nothing is installed at runtime.
RUN rm -rf /usr/local/bin/pip* /usr/local/lib/python3.13/site-packages/pip* /usr/local/lib/python3.13/ensurepip
RUN mkdir -p /etc/tap && printf '%s\n' python3 > /etc/tap/interpreters
COPY --from=build /out/tap-runner /usr/local/bin/tap-runner
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/tap-runner"]
CMD ["serve"]
