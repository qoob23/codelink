#!/usr/bin/env bash
# Build and (re)install the codelink daemon. Idempotent — re-run after any change.
set -euo pipefail

LABEL="com.qoob23.codelink"
BIN="$HOME/.local/bin/codelink"
IDENTITY="codelink-dev"

# Everything is resolved from the checkout this script lives in, so the repo can
# sit anywhere. pwd -P because Go tooling wants a real path, not a symlink.
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
SRC="$REPO/daemon"
PLIST_SRC="$REPO/launchd/$LABEL.plist"
PLIST_DST="$HOME/Library/LaunchAgents/$LABEL.plist"

# build-manifest writes manifest.json and hosts.gen.js into this checkout's
# extension/, wherever the checkout happens to live.
export CODELINK_EXTENSION_DIR="$REPO/extension"

# --- build ------------------------------------------------------------------
mkdir -p "$HOME/.local/bin" "$HOME/.local/state/codelink"/{instances,sock} "$HOME/.local/share/codelink"
echo ">>> building $SRC -> $BIN"
(cd "$SRC" && go build -trimpath -o "$BIN" .)

# --- codesign ---------------------------------------------------------------
# TCC (Automation → Ghostty) keys an UNSIGNED binary by its cdhash, so every
# rebuild would re-prompt. Signing with a stable self-signed identity makes TCC
# key on the Designated Requirement instead, and rebuilds stay silent.
# Create the identity once: Keychain Access → Certificate Assistant →
#   Create a Certificate… → name "codelink-dev", type "Code Signing", self-signed.
if security find-identity -v -p codesigning 2>/dev/null | grep -q "$IDENTITY"; then
    codesign -s "$IDENTITY" --force --options runtime "$BIN"
    echo ">>> signed as $IDENTITY"
else
    codesign -s - --force "$BIN" 2>/dev/null || true
    echo ">>> WARNING: no '$IDENTITY' code-signing identity; ad-hoc signed."
    echo "    Ghostty automation will re-prompt after every rebuild."
    echo "    See the README for how to create the identity."
fi

# --- extension manifest -----------------------------------------------------
"$BIN" build-manifest || echo ">>> build-manifest skipped (template not present yet)"

# --- launchd ----------------------------------------------------------------
mkdir -p "$HOME/Library/LaunchAgents"
ln -sfn "$PLIST_SRC" "$PLIST_DST"

# bootout is expected to fail on a first install; the daemon isn't loaded yet.
launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST_DST"
launchctl kickstart -k "gui/$(id -u)/$LABEL"

sleep 1
echo ">>> $(launchctl print "gui/$(id -u)/$LABEL" | grep -E '^\s+(state|pid) =' || true)"
echo ">>> done. Check: $BIN doctor"
