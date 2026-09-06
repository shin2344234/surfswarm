#!/usr/bin/env bash
# Cross-compile agent and server for every supported platform into dist/.
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${VERSION:-0.1.0}"
# Stamp the build time (UTC) and, when the tree is under git, the short commit,
# so an installed agent can be told apart from an older build of the same version.
STAMP="$(date -u +%Y%m%d-%H%M%S)"
if commit="$(git rev-parse --short HEAD 2>/dev/null)"; then
  STAMP="${STAMP}-${commit}"
fi
FULL="${VERSION}+${STAMP}"
LDFLAGS="-s -w -X main.version=${FULL}"
echo "version ${FULL}"
mkdir -p dist

targets="linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64 windows/amd64"
for target in $targets; do
  os="${target%/*}"
  arch="${target#*/}"
  ext=""
  [ "$os" = "windows" ] && ext=".exe"
  goarm=""
  [ "$arch" = "arm" ] && goarm="7"
  echo "building ${target}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" GOARM="$goarm" \
    go build -trimpath -ldflags "$LDFLAGS" -o "dist/surfswarm-agent-${os}-${arch}${ext}" ./cmd/agent
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" GOARM="$goarm" \
    go build -trimpath -ldflags "$LDFLAGS" -o "dist/surfswarm-server-${os}-${arch}${ext}" ./cmd/server
done
ls -la dist/
