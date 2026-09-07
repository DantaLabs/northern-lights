# syntax=docker/dockerfile:1

# Stage 1: build the static Go binary.
FROM golang:1.27-alpine AS builder
WORKDIR /src

# Install git for fetching private module dependencies (if any) and ca-certificates
# for the final stage fallback path.
RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build a fully static binary. -s -w strips debug info to shrink the image.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags='-s -w' -o /workiva-mcp ./cmd/workiva-mcp

# Stage 2: minimal runtime image.
FROM gcr.io/distroless/static-debian12

# distroless/static includes ca-certificates; no need to copy them explicitly.
COPY --from=builder /workiva-mcp /workiva-mcp

# Persistent SQLite mapping store, audit log, and config file live here.
VOLUME ["/data"]
ENV NL_DB_PATH=/data/northern-lights.db

EXPOSE 8080

ENTRYPOINT ["/workiva-mcp", "-config", "/data/config.yaml"]
