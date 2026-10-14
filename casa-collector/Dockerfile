# Build from this directory: docker build -t casa-collector casa-collector
FROM golang:1.27.1-trixie AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/casa-collector .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/casa-collector /usr/local/bin/casa-collector
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/casa-collector"]
