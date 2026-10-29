#!/usr/bin/env bash
# Builds dist/doremote-macos.zip containing a universal (arm64 + x86_64) doremote.app menu-bar
# app. Run on macOS with Go and Xcode command line tools.
#   VERSION=1.2.3 packaging/macos/build.sh
#
# Signing: SIGN_IDENTITY names a "Developer ID Application" identity in the keychain; unset, the
# first such identity found is used, and SIGN_IDENTITY=- (or none found) makes an ad-hoc build.
# Developer ID builds use the hardened runtime and a secure timestamp. Their designated
# requirement stays the same across versions, so Screen Recording/Accessibility grants survive
# updates, and only they pass the auto-updater's signature check.
#
# Notarization (Developer ID builds only): NOTARY_PROFILE names a `xcrun notarytool
# store-credentials` keychain profile, or APPLE_ID + APPLE_APP_PASSWORD (+ APPLE_TEAM_ID,
# default: the signing identity's team) are passed directly. NOTARIZE=required fails the build
# when neither is set.
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

if [ -z "${SIGN_IDENTITY:-}" ]; then
  SIGN_IDENTITY="$(security find-identity -v -p codesigning | sed -n 's/.*"\(Developer ID Application: .*\)"$/\1/p' | head -n1)"
  SIGN_IDENTITY="${SIGN_IDENTITY:--}"
fi

ZIP="$DIST/doremote-macos.zip"
rm -f "$ZIP"
if [ "$SIGN_IDENTITY" = "-" ]; then
  # Ad-hoc: runs on Apple silicon, but macOS treats every build as a new app (permissions are
  # asked again) and the updater rejects it.
  codesign --force --sign - --identifier dev.doremote.connector "$APP"
  ditto -c -k --keepParent "$APP" "$ZIP"
  echo "built $ZIP ($VERSION, ad-hoc signed)"
  exit 0
fi

codesign --force --sign "$SIGN_IDENTITY" --options runtime --timestamp "$APP"
codesign --verify --strict --verbose=2 "$APP"
ditto -c -k --keepParent "$APP" "$ZIP"

TEAM_ID="${APPLE_TEAM_ID:-$(echo "$SIGN_IDENTITY" | sed -n 's/.*(\([A-Z0-9]*\))$/\1/p')}"
if [ -n "${NOTARY_PROFILE:-}" ]; then
  NOTARY_AUTH=(--keychain-profile "$NOTARY_PROFILE")
elif [ -n "${APPLE_ID:-}" ] && [ -n "${APPLE_APP_PASSWORD:-}" ]; then
  NOTARY_AUTH=(--apple-id "$APPLE_ID" --password "$APPLE_APP_PASSWORD" --team-id "$TEAM_ID")
elif [ "${NOTARIZE:-}" = required ]; then
  echo "NOTARIZE=required but neither NOTARY_PROFILE nor APPLE_ID + APPLE_APP_PASSWORD is set" >&2
  exit 1
else
  echo "built $ZIP ($VERSION, signed by $SIGN_IDENTITY, not notarized)"
  exit 0
fi

# The zip goes to Apple; the ticket is stapled to the app so Gatekeeper accepts it offline,
# then the stapled app is zipped again.
xcrun notarytool submit "$ZIP" "${NOTARY_AUTH[@]}" --wait --timeout 30m
xcrun stapler staple "$APP"
rm -f "$ZIP"
ditto -c -k --keepParent "$APP" "$ZIP"
echo "built $ZIP ($VERSION, signed by $SIGN_IDENTITY, notarized)"
