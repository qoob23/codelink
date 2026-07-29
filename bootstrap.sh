#!/usr/bin/env bash
# First-run setup for a new machine.
#
# Everything this creates lives OUTSIDE the versioned config, in
# ~/.local/share/codelink/, because it is either machine-specific or a secret:
#
#   codelink-ext.pem    the extension keypair (private key, 0600)
#   extension_key.txt   its public half, injected into manifest.json as "key"
#   providers.json      which sites to act on and where their checkouts are
#   nvim.json           which directory names mark a checkout root
#
# The keypair is what pins the extension ID. Chromium derives an unpacked
# extension's ID from its install path unless the manifest carries a "key", and
# the daemon's CORS allowlist accepts exactly one chrome-extension:// origin —
# so without a stable key every request is rejected with an opaque 403.
#
# Idempotent: existing files are never overwritten.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
SHARE="$HOME/.local/share/codelink"
PEM="$SHARE/codelink-ext.pem"
PUB="$SHARE/extension_key.txt"
PROVIDERS="$SHARE/providers.json"
NVIMCFG="$SHARE/nvim.json"

mkdir -p "$SHARE" "$HOME/.local/state/codelink"/{instances,sock} "$HOME/.local/bin"

# --- extension keypair ------------------------------------------------------
if [[ -f "$PEM" ]]; then
    echo ">>> keypair exists, keeping it"
else
    echo ">>> generating extension keypair"
    openssl genrsa -out "$PEM" 2048 2>/dev/null
    chmod 600 "$PEM"
fi

openssl rsa -in "$PEM" -pubout -outform DER 2>/dev/null | openssl base64 -A >"$PUB"

# Chromium's extension ID: SHA-256 of the DER public key, first 16 bytes, each
# nibble mapped 0-f -> a-p.
EXT_ID=$(openssl rsa -in "$PEM" -pubout -outform DER 2>/dev/null |
    openssl dgst -sha256 -binary | xxd -p -c 32 | head -c 32 | tr '0-9a-f' 'a-p')
echo ">>> extension id: $EXT_ID"

# --- providers.json ---------------------------------------------------------
if [[ -f "$PROVIDERS" ]]; then
    echo ">>> providers.json exists, leaving it alone"
    echo "    (if you regenerated the keypair, set extensionId to $EXT_ID)"
else
    echo ">>> writing a starter providers.json — EDIT IT before first use"
    cat >"$PROVIDERS" <<EOF
{
  "version": 1,
  "extensionId": "$EXT_ID",
  "providers": [
    {
      "id": "example",
      "hosts": ["*.example.com"],
      "match": [
        { "path": "^/repo/(?P<repoPath>.+)\$" }
      ],
      "hash": "^L(?P<line>\\\\d+)(?:-L?(?P<endLine>\\\\d+))?\$",
      "refParam": "rev",
      "defaultRef": "main",
      "projectMarkers": ["lib", "src", "test", "bin"],
      "roots": [
        { "glob": "~/checkouts/*" },
        { "path": "~/main-checkout", "label": "main" }
      ]
    }
  ]
}
EOF
fi

# --- nvim.json --------------------------------------------------------------
if [[ -f "$NVIMCFG" ]]; then
    echo ">>> nvim.json exists, leaving it alone"
else
    echo ">>> writing a starter nvim.json"
    printf '{\n  "root_markers": [".git", ".jj", ".hg"]\n}\n' >"$NVIMCFG"
fi

cat <<EOF

Next:
  1. Edit $PROVIDERS   (schema: $REPO/providers.schema.md)
  2. Edit $NVIMCFG     if your checkouts are marked by something other than .git
  3. Put the Neovim half in place — see $REPO/NEOVIM.md
  4. $REPO/install.sh
  5. Load the extension unpacked in each browser from:
       $REPO/extension
     It must appear under id $EXT_ID — if it does not, the manifest lost its
     "key" and the daemon will reject it.
EOF
