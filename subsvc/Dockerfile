# syntax=docker/dockerfile:1

# ---- Build Stage ----
FROM golang:1.22-alpine AS builder

WORKDIR /build
COPY . .

# Static binary, no cgo, no external Go dependencies
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /build/subsvc ./cmd/subsvc

# ---- Runtime Stage ----
FROM alpine:3.20

# XRAY_VERSION: "latest" or a release tag such as v26.3.27 (pin it for reproducible builds)
ARG XRAY_VERSION=latest

RUN apk --no-cache add ca-certificates tzdata su-exec curl unzip \
 && if [ "$XRAY_VERSION" = "latest" ]; then base="https://github.com/XTLS/Xray-core/releases/latest/download"; \
    else base="https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}"; fi \
 && curl -fsSL -o /tmp/xray.zip "$base/Xray-linux-64.zip" \
 && curl -fsSL -o /tmp/xray.dgst "$base/Xray-linux-64.zip.dgst" \
 && want="$(sed -n 's/^SHA2-256= *//p' /tmp/xray.dgst | tr -d '[:space:]')" \
 && have="$(sha256sum /tmp/xray.zip | cut -d' ' -f1)" \
 && [ -n "$want" ] && [ "$want" = "$have" ] || { echo "Xray checksum mismatch" >&2; exit 1; } \
 && unzip -o /tmp/xray.zip xray -d /usr/local/bin/ \
 && chmod +x /usr/local/bin/xray \
 && rm -f /tmp/xray.zip /tmp/xray.dgst \
 && apk del curl unzip \
 && adduser -D -H -u 10001 subsvc \
 && mkdir -p /var/lib/subsvc && chown 10001:10001 /var/lib/subsvc

COPY --from=builder /build/subsvc /usr/local/bin/subsvc
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# Deployment defaults (override with SUBSVC_* environment variables).
# Operational settings (timeouts, test URLs, concurrency, speed test, ...) are
# NOT environment variables: they are edited in the UI and stored in the data volume.
ENV SUBSVC_LISTEN=0.0.0.0:8080 \
    SUBSVC_DATA_DIR=/var/lib/subsvc \
    SUBSVC_CORE_PATH=/usr/local/bin/xray

VOLUME ["/var/lib/subsvc"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD sh -c 'wget -q -O /dev/null "http://127.0.0.1:${SUBSVC_LISTEN##*:}/healthz"'

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
