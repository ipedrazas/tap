FROM golang:1.27.1-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/tap-harness ./cmd/harness

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/tap-harness /usr/local/bin/tap-harness
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/tap-harness"]
