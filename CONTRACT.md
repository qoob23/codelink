# The editor half

The Neovim plugin makes this instance discoverable by the daemon and answers its
open requests. Without it the browser button still appears, the daemon finds no
instance, and every click falls through to spawning a new window.

```
plugin/codelink.lua      autocmds + :CodelinkStatus
lua/codelink/init.lua    registry, socket, RPC entrypoint
```

They sit at the repo root rather than under a `nvim/` subdirectory so the repo
*is* the plugin — `{ 'qoob23/codelink' }` is a complete lazy.nvim spec, no `dir`
and no path to fill in. See the README for the install line.

No `setup()` call, no options table — it registers on `VimEnter` and needs
nothing from you. Requires Neovim 0.11+ (`vim.uv`, `vim.hl.range`).

The plugin is versioned with the daemon on purpose: the registry entry below is
a contract with one reader and three writers, and keeping both sides in one
commit is what stops them drifting.

## Configuration

One setting, and it is deliberately not in Lua: `root_markers`, the directory
names that mark a checkout root.

```jsonc
// ~/.local/share/codelink/nvim.json
{ "root_markers": [".git", ".jj"] }
```

It lives in the same untracked directory the daemon reads `providers.json` from,
so no knowledge of a particular VCS or host ends up in a file you would publish.
The file is optional — without it the fallback is `.git`, `.jj`, `.hg`, `.svn`.
`vim.g.codelink_root_markers` overrides both, for a one-off session.

## Checking it works

```vim
:CodelinkStatus
```

prints the registry path, `on disk: yes`, and the entry for this instance. From
a shell:

```sh
codelink doctor            # lists live instances with their labels and sockets
ls ~/.local/state/codelink/instances/
```

An instance started before the plugin was installed stays invisible until it is
restarted.

## The contract

This is what the daemon relies on. It is written down so the plugin can be
rewritten — or ported to another editor — without reading the Go side.

### 1. Registry entry — `~/.local/state/codelink/instances/<pid>.json`

Written on `VimEnter`, refreshed on `DirChanged` / `FocusGained` / `VimResume`,
deleted on `VimLeavePre`. **Write it atomically** (temp file + rename): the
daemon reads these on every hover and a torn write breaks the parse.

| Field | Type | Meaning |
| --- | --- | --- |
| `v` | int | Schema version. `1`. |
| `pid` | int | `getpid()`. The daemon drops entries whose process is gone. |
| `servername` | string \| null | The explicit socket this instance listens on (below). |
| `auto_servername` | string \| null | `vim.v.servername`, used as a fallback socket. |
| `cwd` | string | Current `getcwd()`, **physical** (symlink-resolved). |
| `launch_cwd` | string | `cwd` at `VimEnter`, never updated. Join key for the terminal's OSC 7 cwd. |
| `root` | string | Checkout root found by walking up from `cwd`. |
| `label` | string | `basename(root)`. Shown in the browser button's menu. |
| `spawn_id` | string \| null | `$CODELINK_SPAWN_ID`, set by the daemon on instances it spawned. |
| `started_at` | int | Unix seconds. |
| `last_focused` | int | Unix seconds, bumped on every touch. Drives "most recent instance". |

The daemon tries `servername` first, then `auto_servername`, taking the first
socket that exists on disk; an entry with neither is pruned.

The entry must be a **regular file of at most 64 KiB** — a symlink, a FIFO or an
oversized file is skipped, silently and without being deleted. A port that
symlinks its entry into place would simply never be seen, so write the real file
(the temp-file-plus-rename above already does).

`cwd` and `launch_cwd` must be physical paths — `vim.fn.getcwd()` already is.
The daemon canonicalises both sides before comparing, because a shell's OSC 7
`$PWD` is logical and checkout roots are routinely symlinked.

### 2. Explicit socket — `~/.local/state/codelink/sock/<pid>.sock`

`vim.fn.serverstart(sock)` in addition to the automatic socket. The predictable
path is what lets the daemon clean up after a `SIGKILL`ed instance — Neovim does
not unlink its listen socket when it dies, so the daemon removes any socket in
this directory whose owning pid is gone. Unlink a leftover socket before calling
`serverstart`, or a recycled pid makes it fail.

### 3. Remote entrypoint — `_G.__codelink_rpc(payload) -> string`

Invoked as:

```sh
nvim --server <sock> --remote-expr "luaeval('_G.__codelink_rpc(_A)', '<json>')"
```

Request: `{"path": "<absolute path>", "line": 12, "end_line": 20}` — `line` and
`end_line` optional.
Response: `{"ok": true}` or `{"ok": false, "error": "..."}`, as a JSON **string**.

It must never raise: `--remote-expr` turns a Lua error into an opaque non-zero
exit the daemon cannot report usefully.

Do the buffer work inside `vim.schedule`. Answering on the RPC caller's stack
blocks on Neovim's UI state — a modal prompt or operator-pending mode deadlocks
the hover.

## Security note

The daemon's root allowlist is not cosmetic. `mode:"new"` targets must sit
inside a root enumerated from `providers.json`, compared after resolving
symlinks, so a link never chooses the directory Neovim is launched in.

Be precise about what that buys, because an earlier version of this note was
not. With `opt.exrc = true`, an attacker-chosen spawn directory is *not*
arbitrary code execution: since 0.9 Neovim sources `.nvim.lua`, `.nvimrc` or
`.exrc` only when the file is already in the trust list (`:help trust`), keyed
on a hash of its contents. So the allowlist is not the thing keeping an unvetted
`.nvim.lua` from running. It is what stops a link picking which of your
already-trusted project configs gets sourced, and where the editor you are about
to type into is rooted.

On 0.12 the search also walks every parent directory rather than the cwd alone,
so the boundary is looser there than one directory. It does not on 0.11, the
floor this plugin declares, so no claim above depends on it.
