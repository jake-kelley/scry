#!/usr/bin/env bash
# build-app.sh — builds scry.app: the Go binary plus the bundle wrapper
# around it (design doc §7 "Ship an .app bundle").
#
#   scry.app/Contents/
#     MacOS/scry          go build -mod=vendor -o <this> ./cmd/scry
#     Info.plist           LSUIElement=true — no Dock icon, no app menu
#     Resources/icon.icns   generated from packaging/icon-source/
#
# Usage:
#   packaging/build-app.sh [output-dir] [version]
#
#   output-dir  defaults to packaging/build; scry.app is created inside it.
#   version     defaults to VERSION_DEFAULT below; overrides
#               CFBundleShortVersionString in the bundled Info.plist.
#
# Idempotent: removes any previous scry.app at the target path before
# rebuilding, so re-running always produces a clean bundle rather than
# accreting stale files. Works from any cwd — everything is resolved from
# this script's own location.
#
# Signing (design doc §9 gotcha 6): TCC grants for Documents/Desktop/
# Downloads are keyed to the code signing identity. Ad-hoc signing
# (`codesign -s -`) derives that identity from the binary's own hash, so
# every rebuild looks like a brand-new app and macOS re-prompts for every
# grant. If a "scry-codesign" identity exists in the login keychain (see
# packaging/SIGNING.md) this script signs with it, so the identity stays
# stable across rebuilds. Otherwise it falls back to ad-hoc signing and
# prints a warning — the app still runs, but expect repeat TCC prompts.
set -euo pipefail

VERSION_DEFAULT="0.1.0"
SIGN_IDENTITY_NAME="${SCRY_SIGN_IDENTITY:-scry-codesign}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

OUT_DIR="${1:-$SCRIPT_DIR/build}"
VERSION="${2:-$VERSION_DEFAULT}"

if [[ "$(uname -s)" != "Darwin" ]]; then
	echo "build-app.sh: must run on macOS (needs codesign, sips, iconutil)" >&2
	exit 1
fi

APP="$OUT_DIR/scry.app"

echo "build-app.sh: target $APP (version $VERSION)"

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

echo "build-app.sh: go build -mod=vendor ./cmd/scry"
( cd "$REPO_ROOT" && go build -mod=vendor -o "$APP/Contents/MacOS/scry" ./cmd/scry )

echo "build-app.sh: Info.plist"
cp "$SCRIPT_DIR/Info.plist" "$APP/Contents/Info.plist"
plutil -replace CFBundleShortVersionString -string "$VERSION" "$APP/Contents/Info.plist"
plutil -lint -s "$APP/Contents/Info.plist"

echo "build-app.sh: icon"
"$SCRIPT_DIR/make-icon.sh" "$APP/Contents/Resources/icon.icns"

echo "build-app.sh: signing"
IDENTITY_LINE="$(security find-identity -v -p codesigning login.keychain 2>/dev/null | grep -F "$SIGN_IDENTITY_NAME" || true)"
if [[ -n "$IDENTITY_LINE" ]]; then
	echo "build-app.sh: signing with identity \"$SIGN_IDENTITY_NAME\""
	codesign --force --deep --options runtime --sign "$SIGN_IDENTITY_NAME" "$APP"
else
	echo "build-app.sh: WARNING: no \"$SIGN_IDENTITY_NAME\" identity in the login keychain." >&2
	echo "build-app.sh: WARNING: falling back to ad-hoc signing (codesign -s -)." >&2
	echo "build-app.sh: WARNING: every rebuild will look like a new app to TCC and re-prompt" >&2
	echo "build-app.sh: WARNING: for Documents/Desktop/Downloads access. See packaging/SIGNING.md." >&2
	codesign --force --deep --sign - "$APP"
fi

codesign --verify --deep --strict "$APP"

echo "build-app.sh: done -> $APP"
