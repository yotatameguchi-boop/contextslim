#!/usr/bin/env bash
# Builds static ContextSlim binaries for every supported OS/CPU into dist/.
# Usage: scripts/build-release.sh [version]
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${1:-$(git describe --tags --always 2>/dev/null || echo dev)}"
PKG=github.com/yotatameguchi-boop/contextslim/internal/version
rm -rf dist && mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${target%/*}; arch=${target#*/}
  ext=""; [ "$os" = windows ] && ext=".exe"
  out="dist/contextslim_${VERSION}_${os}_${arch}${ext}"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X ${PKG}.Version=${VERSION}" -o "$out" ./cmd/contextslim
  echo "built $out"
done
(cd dist && sha256sum contextslim_* > SHA256SUMS)
