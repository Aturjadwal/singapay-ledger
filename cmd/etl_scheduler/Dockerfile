# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder
WORKDIR /src

# Install CA certificates for dependency fetches.
RUN apk add --no-cache ca-certificates

# Cache modules first.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build scheduler binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/etl_scheduler ./cmd/etl_scheduler

FROM alpine:3.22
WORKDIR /app

# Runtime certs are required for HTTPS calls (e.g. the Singapay API).
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -s /sbin/nologin appuser

COPY --from=builder /out/etl_scheduler /usr/local/bin/etl_scheduler

USER appuser
ENTRYPOINT ["/usr/local/bin/etl_scheduler"]
