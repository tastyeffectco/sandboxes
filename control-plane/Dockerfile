# syntax=docker/dockerfile:1.6
#
# sandboxd — the sandboxd control plane.
#
# Two stages: a CGO build (sandboxd uses mattn/go-sqlite3, which needs
# cgo + a C toolchain), then a slim runtime that carries the docker CLI
# (sandboxd shells out to `docker` against the mounted host socket) plus
# the small userland tools the workspace-provisioning path calls.
#
# Build context is the control-plane/ directory.

# --- Stage 1: build sandboxd (CGO) -----------------------------------
FROM golang:1.22-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -o /sandboxd ./cmd/sandboxd

# --- Stage 2: runtime ------------------------------------------------
FROM debian:stable-slim

# docker-ce-cli for the daemon shell-out; ca-certificates for any HTTPS;
# coreutils/cp already present. No docker daemon here — we talk to the
# host daemon over the bind-mounted /var/run/docker.sock.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
 && install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/debian/gpg \
      -o /etc/apt/keyrings/docker.asc \
 && chmod a+r /etc/apt/keyrings/docker.asc \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
      > /etc/apt/sources.list.d/docker.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends docker-ce-cli \
 && apt-get purge -y gnupg \
 && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*

COPY --from=builder /sandboxd /usr/local/bin/sandboxd
# Migrations are read at startup from this path (SANDBOXD_MIGRATIONS).
COPY migrations/ /usr/local/share/sandboxd/migrations/

EXPOSE 9000
ENTRYPOINT ["/usr/local/bin/sandboxd"]
