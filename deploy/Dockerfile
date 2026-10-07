# Builds the Linux server edition into a minimal runtime image.
#
#   docker build -f deploy/Dockerfile -t trading-agent .
#   docker run --rm -p 8080:8080 -v ta-data:/data trading-agent
#   docker logs trading-agent          # prints the token and the URL to open
#
# The reports and the access token live on the /data volume, so they survive
# a container replacement.

# --- build -----------------------------------------------------------------
FROM golang:1.24-alpine AS build
WORKDIR /src

# Dependencies first: this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO is off so the binary is fully static and runs on a scratch-like base.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
        -o /out/trading-agent-server ./cmd/trading-agent-server

# --- runtime ---------------------------------------------------------------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 trading

COPY --from=build /out/trading-agent-server /usr/local/bin/trading-agent-server
COPY config.yaml /etc/trading-agent/config.yaml

RUN mkdir -p /data/reports && chown -R trading:trading /data
USER trading
WORKDIR /data

ENV TA_ADDR=0.0.0.0:8080 \
    TA_CONFIG=/etc/trading-agent/config.yaml \
    TA_OUTPUT=/data/reports \
    TA_TOKEN_FILE=/data/token \
    TZ=UTC

EXPOSE 8080

# The dashboard needs outbound HTTPS to reach Binance, so this checks the
# process itself rather than a dependency.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- --header="Authorization: Bearer $(cat /data/token)" \
        "http://127.0.0.1:8080/healthz" >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/trading-agent-server"]
