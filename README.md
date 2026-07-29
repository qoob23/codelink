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

## For agents

Use the skill. `skills/codelink-config/` covers provider authoring and
validation, new-machine bootstrap, injection scope, Neovim root markers, and a
diagnosis table for every known failure mode.

```sh
mkdir -p ~/.claude/skills
ln -sfn "$(cd <checkout>/skills/codelink-config && pwd -P)" \
        ~/.claude/skills/codelink-config
```

Then ask for it by name (`/codelink-config`), or read
`skills/codelink-config/SKILL.md` directly if your harness has no skill
mechanism. Like everything else tracked here it is host-agnostic: it teaches the
*shape* of a provider entry and how to prove one works, never which sites you
use.

## Install

The repo is a Neovim plugin as well as the daemon and the extension it needs, so
your plugin manager can own the whole thing. With lazy.nvim:

```lua
{ 'qoob23/codelink', build = './install.sh' }
```

`build` compiles the daemon, signs it, generates the extension manifest and
loads the LaunchAgent. On a first install it also generates the extension
keypair and starter configs, then prints two things you have to act on:

1. **Edit `~/.local/share/codelink/providers.json`** — the starter file names
   `example.com`, so nothing resolves until it names your hosts and checkout
   roots. `providers.schema.md` documents every field and opens with a
   ready-to-paste **GitHub** provider.
2. **Load the extension unpacked**, from the path `install.sh` prints (your
   plugin manager's clone, e.g. `~/.local/share/nvim/lazy/codelink/extension`).
   Check the id on the card matches the one printed — if it doesn't, the
   manifest lost its `key` and every request will 403.

Two costs are worth knowing before you paste that line, because both are larger
than a plugin spec looks.

**`build` installs a background daemon, and re-runs on every update.** That one
line means a `:Lazy sync` compiles a Go binary, codesigns it, and
`launchctl bootstrap`s a LaunchAgent that starts at login and stays up —
triggered by a plugin update, with no further prompt. That is the whole point
(one line installs all three parts), but if you would rather a plugin manager
not do it unattended, drop the `build` key and run `./install.sh` yourself; see
*Installing without a plugin manager* below.

**The extension reads every page.** Its content script is declared on
`<all_urls>`, which Chromium's card renders as *"Read and change all your data
on all websites"*. It is the honest cost of the design: a button has to be able
to appear on any page that merely *mentions* a repo link — a local HTML report,
a ticket, a chat log — and a content script scoped to the code hosts cannot do
that. The provider `inject` field narrows injection to match patterns you pick
(`providers.schema.md`); the price of narrowing is that links on a page you left
out get no button and no error saying why.

### Requirements

| | |
| --- | --- |
| **macOS** | The daemon runs under launchd and drives Ghostty over AppleScript. Nothing else is portable today. |
| **Go 1.22+** | To build the daemon. Stdlib only — no module downloads. |
| **Neovim 0.11+** | The plugin uses `vim.uv` and `vim.hl.range`. |
| **A Chromium browser** | Any build that can load an unpacked extension. |

Ghostty is only needed for shift-click (open in a *new* editor). Clicking into
an already-running Neovim works without it.

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
after cleaning. What it buys is exactly one thing: a link never chooses the
directory an editor is launched in.

That is worth having if your config sets `opt.exrc = true`, but it is not what
stands between you and code execution, and this file used to claim otherwise.
Neovim has not sourced a `.nvim.lua` on sight since 0.9 — `'exrc'` executes
`.nvim.lua`, `.nvimrc` or `.exrc` **only if the file is already in the trust
list** (`:help trust`), keyed on a hash of its contents, and prompts for
anything else. What the allowlist actually denies an attacker is the choice of
which of your **already-trusted** project configs gets sourced, and the choice
of where an editor you are about to type into is rooted. Both are worth denying;
neither is arbitrary RCE.

Neovim **0.12** additionally searches every parent directory, not just the cwd,
which widens the first of those — a spawn deep inside a trusted checkout picks
up that checkout's exrc regardless of how far down it lands. That does not apply
on the 0.11 floor above, so nothing here rests on it.

## Credits

- The **Neovim mark** used for the button and the extension icons is by
  [Jason Long](https://github.com/neovim/neovim.github.io), CC BY 3.0, modified.
  Full attribution and a statement of the modifications:
  [`extension/LICENSES.md`](extension/LICENSES.md).
- The daemon is **stdlib-only Go** and the extension has **no runtime
  dependencies**. The only third-party package anywhere is
  [jsdom](https://github.com/jsdom/jsdom) (MIT), a devDependency of the
  extension's test suite.
- Built with [Claude Code](https://claude.com/claude-code) — see
  `WORKJOURNAL.md` for how, including what that got wrong.

## License

MIT — see [`LICENSE`](LICENSE).
