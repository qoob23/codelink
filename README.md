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

## Layout

| Path | What |
| --- | --- |
| `daemon/` | Go module, stdlib only. Subcommands `serve`, `build-manifest`, `doctor`. |
| `extension/` | Chromium MV3 extension, loaded unpacked (works in any Chromium build). |
| `launchd/` | The LaunchAgent plist, symlinked into `~/Library/LaunchAgents` by the installer. |
| `bootstrap.sh` | First-run setup: keypair + starter configs. Never overwrites. |
| `install.sh` | Build → codesign → generate manifest → (re)load the agent. Idempotent. |
| `skills/` | Agent skill for configuring and debugging codelink — see below. |
| `NEOVIM.md` | The Neovim half: the contract the daemon speaks, and a config to copy. |

The checkout can live anywhere — `bootstrap.sh` and `install.sh` resolve every
path from their own location. Two things do not follow automatically if you move
it: the `CODELINK_EXTENSION_DIR` literal in `launchd/*.plist` (launchd does not
expand `$HOME`), and the unpacked-extension path registered in each browser.

Generated, gitignored, and **not** yours to edit: `extension/manifest.json`,
`extension/hosts.gen.js` (both from `build-manifest`), `extension/token.gen.js`
(written by `serve` at startup).

Machine-local state lives in `~/.local/state/codelink/`: the Neovim instance
registry, the explicit nvim sockets, the shared token, `recent.json`, and the
daemon logs.

### Nothing here knows your hosts

The daemon, the extension and the Neovim module contain no reference to any
particular code-hosting site, VCS or repository layout — deliberately, so this
tree can be published or shared as-is. Every site-specific fact lives in two
untracked files under `~/.local/share/codelink/`:

| File | Read by | Contents |
| --- | --- | --- |
| `providers.json` | daemon | Hosts, URL patterns, checkout roots. See `providers.schema.md`. |
| `nvim.json` | Neovim module | `root_markers` — the directory names that mark a checkout root, e.g. `[".git", ".jj"]` plus whatever your VCS uses. |

The Neovim module itself is not tracked here either — it belongs in your own
`~/.config/nvim`. `NEOVIM.md` specifies the contract and carries a reference
implementation to copy.

`nvim.json` is optional; without it the module falls back to plain VCS markers
(`.git`, `.jj`, `.hg`, `.svn`). `vim.g.codelink_root_markers` overrides both.

## Install on a new machine

The repo alone is not enough: the keypair, `providers.json` and `nvim.json` are
untracked by design, so a fresh clone has none of them. Skipping this step
produces a confusing failure — with no `extensionId` the manifest gets no `key`,
Chromium then derives the extension ID from its install path, and every request
is rejected with an opaque 403 that looks like a CORS bug.

```sh
~/soft/codelink/bootstrap.sh    # keypair + starter configs; never overwrites
$EDITOR ~/.local/share/codelink/providers.json
~/soft/codelink/install.sh      # build, sign, generate manifest, load agent
```

`bootstrap.sh` prints the extension ID it derived. Then load the extension
unpacked in each browser, from the **real** path — a symlinked load path has
caused reload flakiness:

```sh
cd ~/soft/codelink/extension && pwd -P
```

Check the ID on the extension card matches the one `bootstrap.sh` printed. If it
doesn't, the manifest lost its `key` and nothing will work.

To keep the same ID as an existing machine, copy `codelink-ext.pem` across
(it's a private key — treat it as a secret) and re-run `bootstrap.sh`, which
regenerates `extension_key.txt` from it. Generating a fresh keypair is fine too,
as long as you update `extensionId` in `providers.json` to the new value.

The Neovim half is two files in your own config — see `NEOVIM.md`. Once
`plugin/codelink.lua` is in place it registers every instance you start;
instances started before that stay invisible until restarted.

## Upgrading an existing install

```sh
~/soft/codelink/install.sh
```

Idempotent — rebuilds, re-signs, regenerates the manifest and kickstarts the
agent. Reload the extension card afterwards if `content.js`, `background.js` or
the manifest changed; Chromium does not pick up file changes on its own.

## The agent skill

`skills/codelink-config/` is a Claude Code skill covering provider authoring and
validation, new-machine bootstrap, injection scope, Neovim root markers, and the
diagnosis table for every known failure mode. It ships with the repo so an agent
with a clone can install it on demand:

```sh
mkdir -p ~/.claude/skills
ln -sfn "$(cd ~/soft/codelink/skills/codelink-config && pwd -P)" \
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
