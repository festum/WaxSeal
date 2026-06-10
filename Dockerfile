# syntax=docker/dockerfile:1
#
# WaxSeal: a real-browser PO-token service. The image bundles a pinned Chromium,
# which it drives via go-rod. It runs as a non-root user; harden the container at
# run time (compose.yaml: cap_drop, no-new-privileges). That container boundary is
# the isolation, since headless Chromium runs with --no-sandbox inside it.

# --- build stage -----------------------------------------------------------
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Version stamping: pass `--build-arg VERSION=1.2.3`. ARG must be declared in this
# stage for the RUN to see it.
ARG VERSION=docker
# CGO off: the binary is pure Go (the JS bundle is go:embed-ed). It still needs a
# system Chromium at run time.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/waxseal ./cmd/waxseal

# --- runtime stage ---------------------------------------------------------
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      chromium fonts-liberation ca-certificates tini \
 && rm -rf /var/lib/apt/lists/*

# Non-root user with a writable HOME (the browser profile lives under $HOME).
RUN useradd --create-home --uid 10001 waxseal
COPY --from=build /out/waxseal /usr/local/bin/waxseal

ENV WAXSEAL_CHROME_BIN=/usr/bin/chromium \
    HOME=/home/waxseal
USER waxseal
EXPOSE 4416

# tini reaps the many short-lived Chromium child processes (PID-1 zombie reaping).
ENTRYPOINT ["/usr/bin/tini", "--", "waxseal"]
CMD ["server", "--host", "0.0.0.0"]

# Health: the binary's own curl-free probe. start-period covers the browser warm-up;
# timeout covers a lazy attestation. (Multi-tenant deployments: add `--key <key>`.)
HEALTHCHECK --interval=30s --timeout=110s --start-period=120s --retries=3 \
  CMD ["waxseal", "ping", "--addr", "127.0.0.1:4416"]
