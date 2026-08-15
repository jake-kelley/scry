#!/usr/bin/env bash
# uninstall.sh — reverse install.sh completely: unload the LaunchAgent,
# remove its plist, and remove scry.app.
#
# Usage: packaging/uninstall.sh [--purge]
#
#   --purge  also delete the index, cache, and config. Every location scry
#            can use is removed, not just the ones this machine happens to
#            use: internal/config picks ~/Library/Caches/scry on darwin but
#            ~/.cache/scry elsewhere, and the config is ~/.config/scry unless
#            that directory is absent, in which case it is
#            ~/Library/Application Support/scry. Listing only some of them is
#            how a "purge" quietly leaves the whole index behind.
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

LABEL="com.scry.app"
# Labels this app has used before now, so uninstalling an older install with a
# newer checkout still removes the agent it actually loaded. See install.sh.
LEGACY_LABELS=(com.jakekelley.scry)
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

for legacy in "${LEGACY_LABELS[@]}"; do
	legacy_plist="$LAUNCH_AGENTS_DIR/$legacy.plist"
	if launchctl print "$UID_GUI/$legacy" >/dev/null 2>&1 || [[ -f "$legacy_plist" ]]; then
		echo "uninstall.sh: removing the legacy $legacy agent"
		launchctl bootout "$UID_GUI/$legacy" >/dev/null 2>&1 || true
		rm -f "$legacy_plist"
	fi
done

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

SCRY_STATE_DIRS=(
	"$HOME/Library/Caches/scry"             # config.CacheDir on darwin: the index lives here
	"$HOME/.cache/scry"                     # config.CacheDir elsewhere
	"$HOME/.config/scry"                    # config.ConfigPath, preferred
	"$HOME/Library/Application Support/scry" # config.ConfigPath fallback when ~/.config is absent
	"$HOME/Library/Logs/scry"               # StandardOutPath/StandardErrorPath from the plist
)

if [[ "$PURGE" -eq 1 ]]; then
	echo "uninstall.sh: --purge: removing index, cache, config, and logs"
	rm -rf "${SCRY_STATE_DIRS[@]}"
else
	echo "uninstall.sh: leaving index/config/logs in place — rerun with --purge to remove them:"
	# `[[ -e ... ]] && echo` as the loop's last command would make the loop
	# exit nonzero when the final directory is absent, and set -e would take
	# that as failure. An if is not stylistic here.
	for d in "${SCRY_STATE_DIRS[@]}"; do
		if [[ -e "$d" ]]; then
			echo "uninstall.sh:   $d"
		fi
	done
fi

echo "uninstall.sh: done"
