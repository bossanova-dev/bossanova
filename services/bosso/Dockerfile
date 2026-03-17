# Build from repo root: docker build -f services/bosso/Dockerfile -t bosso .
FROM golang:1.25-bookworm AS builder

WORKDIR /src

# Copy workspace and module files for dependency caching.
COPY go.work go.work.sum ./
COPY lib/bossalib/go.mod lib/bossalib/go.sum ./lib/bossalib/
COPY services/bosso/go.mod services/bosso/go.sum ./services/bosso/

# Create stub go.mod for boss/bossd (referenced by go.work but not built).
RUN mkdir -p services/boss services/bossd \
    && echo 'module github.com/recurser/boss\ngo 1.25.0' > services/boss/go.mod \
    && echo 'module github.com/recurser/bossd\ngo 1.25.0' > services/bossd/go.mod

RUN go mod download

# Copy source for bossalib and bosso.
COPY lib/bossalib/ ./lib/bossalib/
COPY services/bosso/ ./services/bosso/

RUN CGO_ENABLED=0 go build -o /usr/local/bin/bosso ./services/bosso/cmd

# Litestream for SQLite replication to R2.
FROM litestream/litestream:0.3 AS litestream

# Final image.
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

COPY --from=builder /usr/local/bin/bosso /usr/local/bin/bosso
COPY --from=litestream /usr/local/bin/litestream /usr/local/bin/litestream
COPY services/bosso/litestream.yml /etc/litestream.yml

EXPOSE 8080

ENTRYPOINT ["litestream", "replicate", "-exec", "bosso"]
