#!/usr/bin/env bash
# install.sh — build scry.app, install it, and load the LaunchAgent so scry
# starts at login (design doc §7).
#
# Usage: packaging/install.sh
#
# Env overrides:
#   SCRY_APPDIR   install location for scry.app. Defaults to /Applications
#                 if writable, else ~/Applications.
#   SCRY_SIGN_IDENTITY  passed through to build-app.sh (default scry-codesign).
#
# Idempotent: safe to re-run. Re-running rebuilds the app, reinstalls it in
# place, and reloads the LaunchAgent (bootout + bootstrap) rather than
# erroring on "already installed".
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
	echo "install.sh: must run on macOS" >&2
	exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LABEL="com.jakekelley.scry"
LAUNCH_AGENTS_DIR="$HOME/Library/LaunchAgents"
AGENT_PLIST="$LAUNCH_AGENTS_DIR/$LABEL.plist"
LOG_DIR="$HOME/Library/Logs/scry"

if [[ -n "${SCRY_APPDIR:-}" ]]; then
	APPDIR="$SCRY_APPDIR"
elif [[ -w /Applications ]]; then
	APPDIR="/Applications"
else
	APPDIR="$HOME/Applications"
fi

echo "install.sh: building scry.app"
BUILD_OUT="$(mktemp -d)"
trap 'rm -rf "$BUILD_OUT"' EXIT
"$SCRIPT_DIR/build-app.sh" "$BUILD_OUT"

echo "install.sh: installing to $APPDIR/scry.app"
mkdir -p "$APPDIR"
rm -rf "$APPDIR/scry.app"
cp -R "$BUILD_OUT/scry.app" "$APPDIR/scry.app"
SCRY_BIN="$APPDIR/scry.app/Contents/MacOS/scry"

echo "install.sh: writing LaunchAgent"
mkdir -p "$LAUNCH_AGENTS_DIR" "$LOG_DIR"
sed -e "s|__SCRY_BIN__|$SCRY_BIN|g" \
	-e "s|__SCRY_LOG_DIR__|$LOG_DIR|g" \
	"$SCRIPT_DIR/$LABEL.plist" > "$AGENT_PLIST"
plutil -lint -s "$AGENT_PLIST"

echo "install.sh: loading LaunchAgent"
UID_GUI="gui/$(id -u)"
launchctl bootout "$UID_GUI/$LABEL" >/dev/null 2>&1 || true
launchctl bootstrap "$UID_GUI" "$AGENT_PLIST"
launchctl enable "$UID_GUI/$LABEL"

echo "install.sh: done"
echo "install.sh:   app     -> $APPDIR/scry.app"
echo "install.sh:   agent   -> $AGENT_PLIST"
echo "install.sh:   logs    -> $LOG_DIR"
echo "install.sh:   status  -> launchctl print $UID_GUI/$LABEL"
