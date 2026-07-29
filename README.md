# codelink

Hover a link to a source file in the browser, get a small Neovim button. Click
it and the file opens at the right line in a Neovim you already have running.
Shift-click and it opens a new one in Ghostty.

```
browser page
  └─ content.js ── chrome.runtime.sendMessage ──▶ background.js ──fetch──▶ 127.0.0.1:47391
                                                                                │
                                                                     codelink (Go daemon)
                                                                      ├─ nvim --server --remote-expr
                                                                      └─ osascript → Ghostty
```

Nothing here knows about a specific code host. That lives in
`~/.local/share/codelink/providers.json` (see `providers.schema.md`), outside
this repo.

## Install

The repo is a Neovim plugin as well as the daemon and the extension it needs, so
your plugin manager can own the whole thing. With lazy.nvim:

```lua
{ 'qoob23/codelink', build = './install.sh' }
```

While the repo is private, add `url = 'git@github.com:qoob23/codelink.git'` —
lazy.nvim clones over HTTPS, which has no credentials to offer, and the failure
(`could not read Username for 'https://github.com'`) does not mention auth.

`build` compiles the daemon, signs it, generates the extension manifest and
loads the LaunchAgent. On a first install it also generates the extension
keypair and starter configs, then prints two things you have to act on:

1. **Edit `~/.local/share/codelink/providers.json`** — the starter file names
   `example.com`. Nothing resolves until it names your hosts and checkout roots.
   See `providers.schema.md`.
2. **Load the extension unpacked**, from the path `install.sh` prints (your
   plugin manager's clone, e.g. `~/.local/share/nvim/lazy/codelink/extension`).
   Check the id on the card matches the one printed — if it doesn't, the
   manifest lost its `key` and every request will 403.

Requires Go, Neovim 0.11+, and macOS for the Ghostty/launchd half.

Re-run the build step (`:Lazy build codelink`) after changing `providers.json`,
and reload the extension card when `content.js`, `background.js` or the manifest
changed — Chromium does not pick up file changes on its own.

## Layout

| Path | What |
| --- | --- |
| `plugin/`, `lua/` | The Neovim plugin. At the root so the repo *is* the plugin — see `CONTRACT.md`. |
| `daemon/` | Go module, stdlib only. Subcommands `serve`, `build-manifest`, `doctor`. |
| `extension/` | Chromium MV3 extension, loaded unpacked (works in any Chromium build). |
| `launchd/` | LaunchAgent plist **template**, rendered into `~/Library/LaunchAgents` by the installer. |
| `bootstrap.sh` | First-run setup: keypair + starter configs. Never overwrites. |
| `install.sh` | Bootstrap → build → codesign → generate manifest → render plist → (re)load the agent. Idempotent. |
| `skills/` | Agent skill for configuring and debugging codelink — see below. |

Three parts, one contract each: the extension decides which links get a button,
the daemon turns a URL into a path and picks an instance, the plugin makes an
instance findable and opens the buffer. They live in one repo because the
contracts between them are what would drift if they did not — and because it
lets one `build` line install all three.

The checkout can live anywhere, which is what makes a plugin manager's clone a
valid install: `bootstrap.sh` and `install.sh` resolve from their own location,
the daemon takes `extension/` from `$CODELINK_EXTENSION_DIR`, and the plist —
which must carry absolute paths, since launchd expands neither `~` nor `$HOME` —
is a template rendered at install time. Move the checkout and re-run
`install.sh`; the one thing that does not follow is the unpacked-extension path
registered in each browser, which has to be re-pointed by hand.

Hacking on it: point lazy.nvim at your own clone with its `dev` option
(`{ 'qoob23/codelink', dev = true }` plus `dev = { path = '~/where/you/clone' }`
in your lazy setup) so there is one copy rather than two competing installs.

Generated, gitignored, and **not** yours to edit: `extension/manifest.json`,
`extension/hosts.gen.js` (both from `build-manifest`), `extension/token.gen.js`
(written by `serve` at startup).

Machine-local state lives in `~/.local/state/codelink/`: the Neovim instance
registry, the explicit nvim sockets, the shared token, `recent.json`, and the
daemon logs.

### Nothing here knows your hosts

The daemon, the extension and the Neovim plugin contain no reference to any
particular code-hosting site, VCS or repository layout — deliberately, so this
tree can be published or shared as-is. Every site-specific fact lives in two
untracked files under `~/.local/share/codelink/`:

| File | Read by | Contents |
| --- | --- | --- |
| `providers.json` | daemon | Hosts, URL patterns, checkout roots. See `providers.schema.md`. |
| `nvim.json` | Neovim plugin | `root_markers` — the directory names that mark a checkout root, e.g. `[".git", ".jj"]` plus whatever your VCS uses. |

`nvim.json` is optional; without it the plugin falls back to plain VCS markers
(`.git`, `.jj`, `.hg`, `.svn`). `vim.g.codelink_root_markers` overrides both.

## Installing without a plugin manager

The two scripts are the whole install; they resolve everything from their own
location, so no path here is a choice you have to reproduce.

```sh
cd <checkout>
./bootstrap.sh                  # keypair + starter configs; never overwrites
$EDITOR ~/.local/share/codelink/providers.json
./install.sh                    # build, sign, generate manifest, load agent
```

Then load the extension unpacked from `<checkout>/extension` — use the **real**
path, a symlinked load path has caused reload flakiness — and add the checkout
to your runtimepath:

```lua
vim.opt.runtimepath:append(vim.fn.expand('<checkout>'))
```

The plugin registers every instance you start; instances started before it was
in place stay invisible until restarted.

### Keeping one extension id across machines

Copy `codelink-ext.pem` across (it's a private key — treat it as a secret) and
re-run `bootstrap.sh`, which regenerates `extension_key.txt` from it. Generating
a fresh keypair is fine too, as long as you update `extensionId` in
`providers.json` to the new value.

## Upgrading an existing install

`:Lazy build codelink`, or `<checkout>/install.sh` directly. Idempotent —
rebuilds, re-signs, re-renders the LaunchAgent plist, regenerates the manifest
and kickstarts the agent. Run it again after moving the checkout: the plist's
absolute paths are the only thing that does not follow by itself.

## The agent skill

`skills/codelink-config/` is a Claude Code skill covering provider authoring and
validation, new-machine bootstrap, injection scope, Neovim root markers, and the
diagnosis table for every known failure mode. It ships with the repo so an agent
with a clone can install it on demand:

```sh
mkdir -p ~/.claude/skills
ln -sfn "$(cd <checkout>/skills/codelink-config && pwd -P)" \
        ~/.claude/skills/codelink-config
```

Then ask for it by name (`/codelink-config`) or just describe the task — the
description covers the usual triggers.

Like everything else tracked here it is deliberately host-agnostic: it teaches
the *shape* of a provider entry and how to prove one works, never which sites
you use.

## The code-signing identity

Worth doing once. macOS gates Apple Events behind TCC, and an **unsigned**
binary is identified by its cdhash — so every rebuild of the daemon would
re-trigger the *"codelink wants to control Ghostty"* prompt. Signing with a
stable self-signed identity makes TCC key on the Designated Requirement instead,
and rebuilds stay silent.

Keychain Access → Certificate Assistant → *Create a Certificate…*
→ name `codelink-dev`, Identity Type *Self Signed Root*, Certificate Type
*Code Signing*. `install.sh` picks it up automatically on the next run.

Without it everything still works — you just re-approve after each rebuild, and
if you deny outright, codelink falls back to `open -a Ghostty` (app focus rather
than exact-tab focus, and a second Ghostty process for new windows).

To reset the grant: System Settings → Privacy & Security → Automation.

## Operating it

```sh
codelink doctor                                        # providers, roots, mounts, live instances
launchctl kickstart -k gui/$(id -u)/com.qoob23.codelink # restart after a rebuild
launchctl print      gui/$(id -u)/com.qoob23.codelink   # state and pid
launchctl bootout    gui/$(id -u)/com.qoob23.codelink   # stop
tail -f ~/.local/state/codelink/daemon.err.log
```

`install.sh` re-runs all of the above, so after editing the daemon just run it
again.

## Security model

The daemon binds `127.0.0.1` only and requires two headers on every request:
`X-Codelink-Client: ext` and `X-Codelink-Token`. The custom header forces a CORS
preflight on any cross-origin request, and preflight is answered **only** for the
pinned `chrome-extension://` origin — so no web page can reach the daemon, not
even the code host's own pages.

That stops web content. It does not stop anything running locally as you: a
local `curl` can send any header it likes, and the token only raises that to
"can read a 0600 file in your home directory".

The control that still has teeth is the **root allowlist**: `mode:"new"` targets
must be one of the roots enumerated from `providers.json`, compared after
`filepath.EvalSymlinks`, and every resolved path must sit inside an allowed root
after cleaning. This matters concretely because the nvim config sets
`opt.exrc = true` — launching nvim in an arbitrary directory that happens to
contain a `.nvim.lua` would be remote code execution.
