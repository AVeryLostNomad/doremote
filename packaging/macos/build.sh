#!/usr/bin/env bash
# Builds dist/doremote-macos.zip containing a universal (arm64 + x86_64), ad-hoc signed
# doremote.app menu-bar app. Run on macOS with Go and Xcode command line tools.
#   VERSION=1.2.3 packaging/macos/build.sh
set -euo pipefail

VERSION="${VERSION:-0.0.0-dev}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DIST="$ROOT/dist"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

APP="$WORK/doremote.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources" "$DIST"

export CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=13.0
LDFLAGS="-s -w -X main.version=$VERSION"
for arch in arm64 amd64; do
  clang_arch=$([ "$arch" = amd64 ] && echo x86_64 || echo arm64)
  (cd "$ROOT/connector" && GOOS=darwin GOARCH=$arch CC="clang -arch $clang_arch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$WORK/doremote-$arch" .)
done
lipo -create -output "$APP/Contents/MacOS/doremote" "$WORK/doremote-arm64" "$WORK/doremote-amd64"

sed "s/__VERSION__/$VERSION/g" "$ROOT/packaging/macos/Info.plist" > "$APP/Contents/Info.plist"

ICONSET="$WORK/doremote.iconset"
mkdir -p "$ICONSET"
SRC="$ROOT/connector/assets/icon.png"
for size in 16 32 128 256 512; do
  sips -z $size $size "$SRC" --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
  sips -z $((size * 2)) $((size * 2)) "$SRC" --out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/doremote.icns"

# Ad-hoc signature: required to run on Apple silicon. Without a Developer ID + notarization,
# first launch needs right-click → Open (or System Settings → Privacy & Security → Open Anyway).
codesign --force --deep --sign - --identifier dev.doremote.connector "$APP"

rm -f "$DIST/doremote-macos.zip"
ditto -c -k --keepParent "$APP" "$DIST/doremote-macos.zip"
echo "built $DIST/doremote-macos.zip ($VERSION)"
