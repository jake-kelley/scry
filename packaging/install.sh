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

# Refusing sudo is not fussiness. scry installs a per-user LaunchAgent into
# gui/$UID, and root has no GUI session to install one into: under sudo this
# script gets all the way to the end and then fails with the distinctly
# unhelpful "Bootstrap failed: 125: Domain does not support specified
# action", having already dropped a root-owned scry.app in /Applications and
# a plist in /var/root that the non-sudo re-run then trips over.
if [[ "$(id -u)" -eq 0 ]]; then
	cat >&2 <<-'EOF'
		install.sh: do not run this with sudo -- run it as your normal user.
		install.sh: scry installs a per-user LaunchAgent (gui/$UID). root has no
		install.sh: GUI session, so launchctl cannot load it there.
		install.sh: If a previous sudo run left artefacts, clear them first:
		install.sh:   sudo rm -rf /Applications/scry.app
		install.sh:   sudo rm -f /var/root/Library/LaunchAgents/com.scry.app.plist
	EOF
	exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LABEL="com.scry.app"
LAUNCH_AGENTS_DIR="$HOME/Library/LaunchAgents"
AGENT_PLIST="$LAUNCH_AGENTS_DIR/$LABEL.plist"
LOG_DIR="$HOME/Library/Logs/scry"

# Labels this app has used before now. An install that only boots out $LABEL
# would leave an older agent loaded and relaunching under KeepAlive, so two
# daemons would race for one socket -- the second to start fails to bind,
# exits, and launchd restarts it forever. Boot out and delete every legacy
# agent before installing the current one. Safe on a machine that never had
# them: bootout on an unknown label is a no-op and rm -f does not care.
LEGACY_LABELS=(com.jakekelley.scry)

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

# The bundled binary is also the CLI, but nothing about installing an .app puts
# it on PATH -- and the first thing anyone does after installing is
# `scry root add <dir>`. Symlink it. /usr/local/bin is the conventional spot but
# is root-owned on a stock macOS and this script refuses sudo, so fall back to
# ~/.local/bin, which is this user's to create. A symlink (not a copy) means a
# re-install updates the CLI for free.
echo "install.sh: linking the scry CLI onto PATH"
BIN_LINK=""
for d in /usr/local/bin "$HOME/.local/bin"; do
	mkdir -p "$d" 2>/dev/null || true
	if [[ -w "$d" ]]; then
		ln -sf "$SCRY_BIN" "$d/scry"
		BIN_LINK="$d/scry"
		break
	fi
done
if [[ -z "$BIN_LINK" ]]; then
	echo "install.sh: WARNING: could not write to /usr/local/bin or ~/.local/bin;" >&2
	echo "install.sh: WARNING: the menu bar app still works, but the scry CLI is only at" >&2
	echo "install.sh: WARNING:   $SCRY_BIN" >&2
fi

echo "install.sh: writing LaunchAgent"
mkdir -p "$LAUNCH_AGENTS_DIR" "$LOG_DIR"
sed -e "s|__SCRY_BIN__|$SCRY_BIN|g" \
	-e "s|__SCRY_LOG_DIR__|$LOG_DIR|g" \
	"$SCRIPT_DIR/$LABEL.plist" > "$AGENT_PLIST"
plutil -lint -s "$AGENT_PLIST"

echo "install.sh: loading LaunchAgent"
UID_GUI="gui/$(id -u)"

for legacy in "${LEGACY_LABELS[@]}"; do
	legacy_plist="$LAUNCH_AGENTS_DIR/$legacy.plist"
	if launchctl print "$UID_GUI/$legacy" >/dev/null 2>&1 || [[ -f "$legacy_plist" ]]; then
		echo "install.sh: removing the legacy $legacy agent"
		launchctl bootout "$UID_GUI/$legacy" >/dev/null 2>&1 || true
		rm -f "$legacy_plist"
	fi
done

# bootout returns before launchd has finished tearing the job down, so on a
# re-install the bootstrap below can land while the domain still holds the
# dying job and fail with "Bootstrap failed: 5: Input/output error". Wait for
# the job to actually disappear, then retry the bootstrap regardless: the
# unload is the common cause of a briefly-busy domain but not the only one.
launchctl bootout "$UID_GUI/$LABEL" >/dev/null 2>&1 || true
for _ in $(seq 1 20); do
	launchctl print "$UID_GUI/$LABEL" >/dev/null 2>&1 || break
	sleep 0.25
done

for attempt in $(seq 1 20); do
	if launchctl bootstrap "$UID_GUI" "$AGENT_PLIST" 2>/dev/null; then
		break
	fi
	if [[ "$attempt" -eq 20 ]]; then
		echo "install.sh: launchctl bootstrap kept failing; re-running it to show the error:" >&2
		launchctl bootstrap "$UID_GUI" "$AGENT_PLIST"
		exit 1
	fi
	sleep 0.25
done
launchctl enable "$UID_GUI/$LABEL"

echo "install.sh: done"
echo "install.sh:   app     -> $APPDIR/scry.app"
echo "install.sh:   agent   -> $AGENT_PLIST"
echo "install.sh:   logs    -> $LOG_DIR"
echo "install.sh:   status  -> launchctl print $UID_GUI/$LABEL"
if [[ -n "$BIN_LINK" ]]; then
	echo "install.sh:   cli     -> $BIN_LINK"
	BIN_DIR="$(dirname "$BIN_LINK")"
	case ":$PATH:" in
	*":$BIN_DIR:"*) ;;
	*)
		echo "install.sh: note: $BIN_DIR is not on your PATH. Add it to use \`scry\` by name:" >&2
		echo "install.sh:   echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.zshrc && exec zsh" >&2
		;;
	esac
fi
