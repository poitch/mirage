# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# CGO_ENABLED=0 keeps the binary static. The SQLite driver is pure Go precisely
# so this works, which is what makes cross-building for a Synology painless.
RUN CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/mirage ./cmd/mirage

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata

COPY --from=build /out/mirage /usr/local/bin/mirage

# Mirage deliberately runs as root. It must chown files it creates to each
# user's NAS uid/gid so DSM File Station and SMB see native ownership, and only
# root may chown to an arbitrary user. Tenant isolation does not rest on the
# process user: each account is confined to its own directory by an os.Root
# handle, which the kernel enforces. The compose file drops every capability
# except the three this actually needs.
VOLUME ["/var/lib/mirage"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/mirage"]
CMD ["serve"]
