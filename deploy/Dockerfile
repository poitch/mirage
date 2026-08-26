# syntax=docker/dockerfile:1

# Pinned to the build platform so the toolchain runs natively and cross-compiles
# to the target, rather than the whole build running under QEMU emulation. This
# only works because the binary is pure Go with cgo disabled - which is also why
# the SQLite driver is a pure-Go one.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/mirage ./cmd/mirage

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata

COPY --from=build /out/mirage /usr/local/bin/mirage

# Mirage deliberately runs as root. It must chown files it creates to each
# user's NAS uid/gid so DSM File Station and SMB see native ownership, and only
# root may chown to an arbitrary user. Tenant isolation does not rest on the
# process user: each account is confined to its own directory by an os.Root
# handle, which the kernel enforces. The compose files drop every capability
# except the few this actually needs.
VOLUME ["/var/lib/mirage"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/mirage"]
CMD ["serve"]
