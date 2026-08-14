#!/usr/bin/env bash
# uninstall.sh — reverse install.sh completely: unload the LaunchAgent,
# remove its plist, and remove scry.app.
#
# Usage: packaging/uninstall.sh [--purge]
#
#   --purge  also delete the index, cache, and config
#            (~/.cache/scry, ~/.config/scry, ~/Library/Application Support/scry).
#            Without this flag, none of that is touched — printed explicitly
#            below so it's never a silent choice.
set -euo pipefail

PURGE=0
for arg in "$@"; do
	case "$arg" in
		--purge) PURGE=1 ;;
		*) echo "uninstall.sh: unknown argument: $arg" >&2; exit 1 ;;
	esac
done

if [[ "$(uname -s)" != "Darwin" ]]; then
	echo "uninstall.sh: must run on macOS" >&2
	exit 1
fi

LABEL="com.jakekelley.scry"
LAUNCH_AGENTS_DIR="$HOME/Library/LaunchAgents"
AGENT_PLIST="$LAUNCH_AGENTS_DIR/$LABEL.plist"
UID_GUI="gui/$(id -u)"

echo "uninstall.sh: unloading LaunchAgent"
launchctl bootout "$UID_GUI/$LABEL" >/dev/null 2>&1 || true

if [[ -f "$AGENT_PLIST" ]]; then
	echo "uninstall.sh: removing $AGENT_PLIST"
	rm -f "$AGENT_PLIST"
else
	echo "uninstall.sh: no LaunchAgent plist found at $AGENT_PLIST (already removed)"
fi

REMOVED_APP=0
for candidate in "/Applications/scry.app" "$HOME/Applications/scry.app"; do
	if [[ -d "$candidate" ]]; then
		echo "uninstall.sh: removing $candidate"
		rm -rf "$candidate"
		REMOVED_APP=1
	fi
done
if [[ "$REMOVED_APP" -eq 0 ]]; then
	echo "uninstall.sh: no scry.app found in /Applications or ~/Applications"
fi

if [[ "$PURGE" -eq 1 ]]; then
	echo "uninstall.sh: --purge: removing index, cache, and config"
	rm -rf "$HOME/.cache/scry" "$HOME/.config/scry" "$HOME/Library/Application Support/scry"
else
	echo "uninstall.sh: leaving index/config in place (~/.cache/scry, ~/.config/scry," \
		"~/Library/Application Support/scry) — rerun with --purge to remove them"
fi

echo "uninstall.sh: done"
