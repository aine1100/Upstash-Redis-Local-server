# Build stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git make

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.Version=docker" -o upstash-redis-local

# Runtime stage
FROM alpine:3.19

# Add labels for better discoverability
LABEL org.opencontainers.image.title="upstash-redis-local"
LABEL org.opencontainers.image.description="A local server that mimics upstash-redis for local testing"
LABEL org.opencontainers.image.source="https://github.com/aine1100/Upstash-Redis-Local-server"

# Install ca-certificates for HTTPS requests
RUN apk add --no-cache ca-certificates

# Copy binary from builder
COPY --from=builder /app/upstash-redis-local /usr/local/bin/

# Expose the default port
EXPOSE 8000

# Run as non-root user
RUN adduser -D -g '' appuser
USER appuser

ENTRYPOINT ["upstash-redis-local"]
CMD ["--addr", ":8000"]
