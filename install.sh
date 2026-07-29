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
STATE="$HOME/.local/state/codelink"
PLIST_TMPL="$REPO/launchd/$LABEL.plist.template"
PLIST_DST="$HOME/Library/LaunchAgents/$LABEL.plist"

# The plist needs an absolute nvim, and a PATH that can reach both nvim and
# ghostty — a launchd agent inherits neither.
NVIM="${CODELINK_NVIM:-$(command -v nvim || true)}"
if [[ -z "$NVIM" ]]; then
    echo ">>> ERROR: no nvim on PATH; set CODELINK_NVIM to its absolute path." >&2
    exit 1
fi

# build-manifest writes manifest.json and hosts.gen.js into this checkout's
# extension/, wherever the checkout happens to live.
export CODELINK_EXTENSION_DIR="$REPO/extension"

# --- build ------------------------------------------------------------------
mkdir -p "$HOME/.local/bin" "$STATE"/{instances,sock} "$HOME/.local/share/codelink"
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
# Rendered rather than symlinked: launchd expands neither ~ nor $HOME, so the
# plist has to carry absolute paths, which is exactly what would otherwise pin
# the checkout to one machine.
mkdir -p "$HOME/Library/LaunchAgents"
# Earlier versions symlinked the plist here. Redirecting onto a symlink writes
# THROUGH it, i.e. straight back into the checkout, so unlink first.
rm -f "$PLIST_DST"
sed -e "s|__BIN__|$BIN|g" \
    -e "s|__NVIM__|$NVIM|g" \
    -e "s|__PATH__|$(dirname "$NVIM"):/usr/bin:/bin:/usr/sbin:/sbin|g" \
    -e "s|__EXTENSION_DIR__|$CODELINK_EXTENSION_DIR|g" \
    -e "s|__STATE_DIR__|$STATE|g" \
    "$PLIST_TMPL" >"$PLIST_DST"

# bootout is expected to fail on a first install; the daemon isn't loaded yet.
launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST_DST"
launchctl kickstart -k "gui/$(id -u)/$LABEL"

sleep 1
echo ">>> $(launchctl print "gui/$(id -u)/$LABEL" | grep -E '^\s+(state|pid) =' || true)"
echo ">>> done. Check: $BIN doctor"
