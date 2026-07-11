# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o latch-relayer ./cmd/serve

# Runtime stage — minimal Alpine image
FROM alpine:3.21

# ca-certificates: needed for TLS calls to Horizon and Stellar RPC
RUN apk add --no-cache ca-certificates

# Run as non-root — principle of least privilege
RUN addgroup -S nonroot && adduser -S nonroot -G nonroot
USER nonroot

WORKDIR /app
COPY --from=builder --chown=nonroot:nonroot /app/latch-relayer .

EXPOSE 4000

ENTRYPOINT ["/app/latch-relayer"]
