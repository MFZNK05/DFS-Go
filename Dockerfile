# Stage 1: build the binary
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Download dependencies first (cached layer — only re-runs if go.mod/go.sum change)
COPY go.mod go.sum ./
RUN go mod download

# Copy source and compile to a single static binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o hermond .

# Stage 2: minimal runtime image (~15MB)
FROM alpine:3.20

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /app/hermond .

# P2P port (peers connect here)
EXPOSE 3000
# Health/metrics port (port + 1000)
EXPOSE 4000

# Default: start a daemon node on :3000
# Override with: docker run ... ./hermond start --port :3000 --peer HOST:3000
ENTRYPOINT ["./hermond"]
CMD ["start", "--port", ":3000", "--no-stun"]
