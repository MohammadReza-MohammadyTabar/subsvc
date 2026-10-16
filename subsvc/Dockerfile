# syntax=docker/dockerfile:1

# ---- Build Stage ----
FROM golang:1.22-alpine AS builder

WORKDIR /build

# Copy source first (go.mod is optional too if no deps)
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /build/subsvc ./cmd/subsvc

# ---- Runtime Stage ----
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata curl unzip \
 && curl -fsSL -o /tmp/xray.zip https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-64.zip \
 && unzip -o /tmp/xray.zip xray -d /usr/local/bin/ \
 && rm /tmp/xray.zip \
 && chmod +x /usr/local/bin/xray

COPY --from=builder /build/subsvc /usr/local/bin/subsvc

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/subsvc"]