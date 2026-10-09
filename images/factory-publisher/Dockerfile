FROM golang:1.27.1-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev COMMIT= DATE=
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags="-s -w -X github.com/ipedrazas/tap/pkg/version.Version=${VERSION} -X github.com/ipedrazas/tap/pkg/version.Commit=${COMMIT} -X github.com/ipedrazas/tap/pkg/version.Date=${DATE}" \
    -o /out/tap-factory-publisher ./cmd/factory-publisher

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/tap-factory-publisher /usr/local/bin/tap-factory-publisher
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/tap-factory-publisher"]
