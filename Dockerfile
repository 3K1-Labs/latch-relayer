# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o latch-relayer ./cmd/serve

# Runtime stage — minimal Alpine image (~10 MB total)
FROM alpine:3.19

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app
COPY --from=builder /app/latch-relayer .

EXPOSE 4000

ENTRYPOINT ["./latch-relayer"]
