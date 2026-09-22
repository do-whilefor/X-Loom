FROM golang:1.26-bookworm AS source
RUN apt-get update && apt-get install -y --no-install-recommends ripgrep \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM source AS test
# Dependencies are downloaded above; checks must not contact external services.
RUN --network=none go test -race -count=1 ./... && go vet ./...

FROM test AS build
RUN --network=none CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/xloom ./cmd/xloom

FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl bash ripgrep \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/xloom /usr/local/bin/xloom
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/xloom"]
CMD ["serve", "--host", "0.0.0.0", "--db-path", "/data/xloom.db"]
