#!/usr/bin/env sh
# Build Mirage for the NAS and write it to a file you can copy across.
#
# Use this when you would rather not involve a registry. The alternative is to
# let CI publish to ghcr.io and pull from Container Manager; both routes are in
# docs/deploy-synology.md.
#
# Usage:  scripts/export-image.sh [amd64|arm64] [output.tar.gz]
#
# Find the NAS architecture over SSH with `uname -m`:
#   x86_64  -> amd64   (most Synology models)
#   aarch64 -> arm64
#
# The architecture that matters is the NAS's, not the machine running this.
# Building for the wrong one produces an image that will not start, with an
# "exec format error" that says nothing about why.
set -eu

ARCH="${1:-amd64}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

case "$ARCH" in
    amd64|x86_64)  ARCH=amd64 ;;
    arm64|aarch64) ARCH=arm64 ;;
    *) echo "unsupported architecture: $ARCH (expected amd64 or arm64)" >&2; exit 1 ;;
esac

OUT="${2:-mirage-${ARCH}.tar.gz}"
VERSION="$(git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"

echo "Building mirage:${VERSION} for linux/${ARCH}..."
docker buildx build \
    --platform "linux/${ARCH}" \
    --file "${REPO_ROOT}/deploy/Dockerfile" \
    --build-arg "VERSION=${VERSION}" \
    --tag "mirage:${VERSION}" \
    --tag "mirage:latest" \
    --load \
    "${REPO_ROOT}"

echo "Writing ${OUT}..."
docker save "mirage:${VERSION}" "mirage:latest" | gzip > "${OUT}"

cat <<INSTRUCTIONS

Wrote ${OUT} ($(du -h "${OUT}" | cut -f1)) for linux/${ARCH}.

On the NAS:
  1. Copy it across, for example to /volume1/docker/mirage/
  2. Load it. In Container Manager: Image -> Add -> Add From File.
     Or over SSH:  sudo docker load < $(basename "${OUT}")
  3. In your compose file, change the image line to:  image: mirage:latest
     It points at ghcr.io by default, which a locally loaded image is not,
     and leaving it would make Docker try to pull instead of using yours.
INSTRUCTIONS
