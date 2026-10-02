#!/usr/bin/env bash
# Builds the release archives for a version into dist/: one per platform,
# each holding the binary, README.md and docs, plus SHA256SUMS.
#
# Usage: scripts/release.sh v1.2.3
set -euo pipefail

VERSION="${1:?usage: scripts/release.sh <version>}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
PLATFORMS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

rm -rf "$DIST"
mkdir -p "$DIST"
cd "$ROOT"
for p in $PLATFORMS; do
  os="${p%/*}" arch="${p#*/}"
  name="intagent_${VERSION#v}_${os}_${arch}"
  stage="$DIST/$name"
  mkdir -p "$stage"
  bin=intagent
  [ "$os" = windows ] && bin=intagent.exe
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$stage/$bin" ./cmd/intagent
  cp -r README.md docs "$stage/"
  if [ "$os" = windows ]; then
    (cd "$DIST" && zip -qr "$name.zip" "$name")
  else
    tar -C "$DIST" -czf "$DIST/$name.tar.gz" "$name"
  fi
  rm -rf "$stage"
done
(cd "$DIST" && sha256sum intagent_* >SHA256SUMS)
ls -1 "$DIST"
