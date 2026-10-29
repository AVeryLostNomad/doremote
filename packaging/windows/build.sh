#!/usr/bin/env bash
# Cross-compiles dist/doremote-windows-{amd64,arm64}.exe: tray apps with the doremote icon,
# version info and a manifest. Pure Go, so it runs on any OS with Go installed.
#   VERSION=1.2.3 packaging/windows/build.sh
# CONSOLE=1 builds console-subsystem binaries instead (for -selftest output in CI).
set -euo pipefail

VERSION="${VERSION:-0.0.0-dev}"
NUMERIC="$(echo "$VERSION" | sed -E 's/^v//; s/[^0-9.].*$//')"
NUMERIC="${NUMERIC:-0.0.0}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DIST="$ROOT/dist"
mkdir -p "$DIST"
cd "$ROOT/connector"
trap 'rm -f rsrc_windows_*.syso' EXIT

go run github.com/tc-hib/go-winres@v0.3.3 simply \
  --arch amd64,arm64 --out rsrc \
  --icon assets/icon.png --manifest gui \
  --product-name doremote --file-description "doremote connector" \
  --product-version "$NUMERIC" --file-version "$NUMERIC" \
  --original-filename doremote.exe

SUBSYSTEM="-H=windowsgui"
SUFFIX=""
if [ "${CONSOLE:-}" = 1 ]; then
  SUBSYSTEM=""
  SUFFIX="-console"
fi
for arch in amd64 arm64; do
  GOOS=windows GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w $SUBSYSTEM -X main.version=$VERSION" \
    -o "$DIST/doremote-windows-$arch$SUFFIX.exe" .
done
echo "built $DIST/doremote-windows-{amd64,arm64}$SUFFIX.exe ($VERSION)"
