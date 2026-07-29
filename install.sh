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

# --- first run --------------------------------------------------------------
# Without a keypair the manifest gets no "key", Chromium derives the extension
# id from the install path instead, and every request is rejected with an opaque
# 403. So bootstrap before building rather than leaving it to the reader — this
# script is what a plugin manager's `build` step runs, and it is the only hook
# a one-line install gets. bootstrap.sh never overwrites, so re-running is free.
if [[ ! -f "$HOME/.local/share/codelink/codelink-ext.pem" ]]; then
    echo ">>> first run: bootstrapping keypair and starter configs"
    "$REPO/bootstrap.sh"
fi

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

# Every value spliced in below is an absolute path off the user's own
# filesystem, and the two obvious ways to substitute one both mangle characters
# a path may legally contain:
#   sed "s|__BIN__|$BIN|"   — an unescaped & in the replacement means "the whole
#     match", so a checkout under /Users/x/A&B rendered as
#     /Users/x/A__EXTENSION_DIR__B with no error at all; a | in a path closes
#     the s|…| delimiter and aborts the run outright.
#   ${plist//__BIN__/$BIN}  — bash 5.2 gave & in the replacement that same
#     "whole match" meaning, so the identical path renders one way under
#     /bin/bash (3.2) and another under a Homebrew bash. Version-dependent
#     output is worse than the sed bug, not better.
# Hence an explicit literal splice. The values then land in XML text nodes, so
# & < > are escaped too — otherwise the plist is not well-formed and launchd
# rejects it.
replace_all() { # <haystack> <needle> <replacement>, all literal
    local hay=$1 needle=$2 repl=$3 out=''
    while [[ $hay == *"$needle"* ]]; do
        out+="${hay%%"$needle"*}$repl"
        hay=${hay#*"$needle"}
    done
    printf '%s' "$out$hay"
}

xml_escape() {
    local s=$1
    s=$(replace_all "$s" '&' '&amp;')
    s=$(replace_all "$s" '<' '&lt;')
    s=$(replace_all "$s" '>' '&gt;')
    printf '%s' "$s"
}

plist=$(<"$PLIST_TMPL")
render() { plist=$(replace_all "$plist" "$1" "$(xml_escape "$2")"); }

render __BIN__           "$BIN"
render __NVIM__          "$NVIM"
render __PATH__          "$(dirname "$NVIM"):/usr/bin:/bin:/usr/sbin:/sbin"
render __EXTENSION_DIR__ "$CODELINK_EXTENSION_DIR"
render __STATE_DIR__     "$STATE"

printf '%s\n' "$plist" >"$PLIST_DST"

# bootout is expected to fail on a first install; the daemon isn't loaded yet.
launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST_DST"
launchctl kickstart -k "gui/$(id -u)/$LABEL"

sleep 1
echo ">>> $(launchctl print "gui/$(id -u)/$LABEL" | grep -E '^\s+(state|pid) =' || true)"

cat <<EOF
>>> done. Check: $BIN doctor

    Load the extension unpacked from:
      $CODELINK_EXTENSION_DIR
    Chromium does not pick up file changes on its own — reload the card after
    an update that touched the extension.
EOF
