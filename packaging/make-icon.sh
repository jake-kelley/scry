#!/usr/bin/env bash
# make-icon.sh — build scry's .icns menu-bar icon from the checked-in source
# art (packaging/icon-source/icon-1024.png) using only macOS-native tools
# (sips, iconutil). Regenerate the source PNG itself with:
#
#   go run ./packaging/_icongen -out packaging/icon-source/icon-1024.png
#
# Usage: make-icon.sh <output-icns-path>
#
# macOS-only: sips and iconutil don't exist elsewhere. Called by
# build-app.sh; safe to run standalone too.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_PNG="$SCRIPT_DIR/icon-source/icon-1024.png"

if [[ $# -ne 1 ]]; then
	echo "usage: $(basename "$0") <output-icns-path>" >&2
	exit 1
fi
OUT_ICNS="$1"

if ! command -v sips >/dev/null 2>&1 || ! command -v iconutil >/dev/null 2>&1; then
	echo "make-icon.sh: requires macOS (sips, iconutil not found)" >&2
	exit 1
fi

if [[ ! -f "$SOURCE_PNG" ]]; then
	echo "make-icon.sh: missing source art at $SOURCE_PNG" >&2
	echo "  regenerate with: go run ./packaging/_icongen -out packaging/icon-source/icon-1024.png" >&2
	exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

ICONSET="$WORK/AppIcon.iconset"
mkdir -p "$ICONSET"

# Apple's required (size, filename) pairs for a full .iconset.
declare -a SPECS=(
	"16 icon_16x16.png"
	"32 icon_16x16@2x.png"
	"32 icon_32x32.png"
	"64 icon_32x32@2x.png"
	"128 icon_128x128.png"
	"256 icon_128x128@2x.png"
	"256 icon_256x256.png"
	"512 icon_256x256@2x.png"
	"512 icon_512x512.png"
	"1024 icon_512x512@2x.png"
)

for spec in "${SPECS[@]}"; do
	px="${spec%% *}"
	name="${spec#* }"
	sips -z "$px" "$px" "$SOURCE_PNG" --out "$ICONSET/$name" >/dev/null
done

mkdir -p "$(dirname "$OUT_ICNS")"
iconutil -c icns "$ICONSET" -o "$OUT_ICNS"

echo "make-icon.sh: wrote $OUT_ICNS"
