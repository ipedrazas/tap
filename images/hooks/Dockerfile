FROM golang:1.27.1-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev COMMIT= DATE=
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags="-s -w -X github.com/ipedrazas/tap/pkg/version.Version=${VERSION} -X github.com/ipedrazas/tap/pkg/version.Commit=${COMMIT} -X github.com/ipedrazas/tap/pkg/version.Date=${DATE}" \
    -o /out/tap-hooks ./cmd/hooks

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/tap-hooks /usr/local/bin/tap-hooks
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/tap-hooks"]
