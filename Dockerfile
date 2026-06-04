# Skill Market Service Dockerfile
#
# A lightweight standalone HTTP API for skill discovery and distribution.
# This service runs independently from the main SOI agent runtime.
#
# Build:
#   docker build -t soi-skill-market -f cmd/skill-market/Dockerfile .
#
# Run (SQLite — zero external dependencies):
#   docker run -p 9090:9090 \
#     -v ./data:/app/data \
#     -v ./skills:/app/skills \
#     soi-skill-market
#
# Run (Postgres):
#   docker run -p 9090:9090 \
#     -v ./skills:/app/skills \
#     -e SKILL_MARKET_DB="postgres://user:pass@host:5432/skillmarket?sslmode=disable" \
#     soi-skill-market
#
# Run with custom config:
#   docker run -p 9090:9090 \
#     -v ./skill-market.yaml:/app/skill-market.yaml \
#     -v ./skills:/app/skills \
#     soi-skill-market --config /app/skill-market.yaml

# ==============================================================================
# Stage 1: Build
# ==============================================================================
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

ENV GO111MODULE=on
ENV GOPROXY=https://goproxy.cn,direct

# Copy go.mod and download dependencies (layer cache)
COPY go.mod go.sum ./
RUN go mod download

# Copy source code (only the packages needed by skill-market)
COPY internal/skill/   ./internal/skill/
COPY internal/skillmarket/ ./internal/skillmarket/
COPY pkg/skill/        ./pkg/skill/
COPY cmd/skill-market/ ./cmd/skill-market/

# Build a statically-linked binary (excludes agent/WASM code)
RUN CGO_ENABLED=0 GOOS=linux go build -tags skillmarket -ldflags="-s -w" -o skill-market ./cmd/skill-market

# ==============================================================================
# Stage 2: Runtime
# ==============================================================================
FROM alpine:3.19

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Copy binary
COPY --from=builder /app/skill-market .

# Copy default config template (optional)
COPY configs/skill-market.yaml /app/configs/skill-market.yaml

# Create runtime directories
RUN mkdir -p /app/data /app/skills

# Expose port
EXPOSE 9090

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:9090/health || exit 1

# Run with built-in defaults; override via env/CLI/config
CMD ["./skill-market"]
