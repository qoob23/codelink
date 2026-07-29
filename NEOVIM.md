# The Neovim half

The daemon does not talk to Neovim over anything Neovim provides out of the box.
It needs your config to do two things: **advertise** each running instance in a
file registry, and **answer** a single remote function. Without them the button
appears, the daemon finds no instance, and every click falls through to spawning
a new window.

These files are deliberately not tracked in this repo — they belong to your own
`~/.config/nvim`, next to the rest of your config. What *is* fixed is the
contract below; the implementation at the end is a reference you can copy
verbatim.

## The contract

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

### 4. Root markers — `~/.local/share/codelink/nvim.json`

`root_markers` is a list of directory names that mark a checkout root, e.g.
`[".git", ".jj"]`. The file is optional; without it, fall back to `.git`, `.jj`,
`.hg`, `.svn`. `vim.g.codelink_root_markers` overrides both.

This lives outside the config on purpose — same directory the daemon reads
`providers.json` from — so no knowledge of a particular VCS or host ends up in a
file you would publish.

## Reference implementation

Two files. Paths assume a `lua/custom/util/` tree; adjust the `require` to match
yours.

### `~/.config/nvim/plugin/codelink.lua`

```lua
-- codelink: keep this nvim instance discoverable by the codelink daemon.
-- All the logic lives in custom.util.codelink, required lazily inside the
-- callbacks so startup stays fast and a broken module cannot break startup.
if vim.g.loaded_codelink then
    return
end
vim.g.loaded_codelink = true

local group = vim.api.nvim_create_augroup('codelink', {})

vim.api.nvim_create_autocmd('VimEnter', {
    group = group,
    callback = function()
        require('custom.util.codelink').register()
    end,
})

vim.api.nvim_create_autocmd({ 'DirChanged', 'FocusGained', 'VimResume' }, {
    group = group,
    callback = function()
        require('custom.util.codelink').touch()
    end,
})

vim.api.nvim_create_autocmd('VimLeavePre', {
    group = group,
    callback = function()
        require('custom.util.codelink').unregister()
    end,
})

vim.api.nvim_create_user_command('CodelinkStatus', function()
    local codelink = require('custom.util.codelink')
    local path = codelink.registry_path()
    local entry = codelink.entry()
    local lines = { 'codelink registry: ' .. path, 'on disk: ' .. (vim.uv.fs_stat(path) and 'yes' or 'no') }
    if entry then
        table.insert(lines, vim.inspect(entry))
    else
        table.insert(lines, 'not registered')
    end
    vim.notify(table.concat(lines, '\n'), vim.log.levels.INFO)
end, { desc = 'Show the codelink registry path and this instance entry' })
```

### `~/.config/nvim/lua/custom/util/codelink.lua`

```lua
-- codelink: register this nvim instance in a file registry so an external
-- daemon can find it and remotely open a file at a line.
--
-- Registry entry: ~/.local/state/codelink/instances/<pid>.json
-- Explicit socket: ~/.local/state/codelink/sock/<pid>.sock
-- Remote entrypoint: _G.__codelink_rpc(json_string) -> json_string

local M = {}

local ns = vim.api.nvim_create_namespace('codelink')

local state_dir = vim.fn.expand('~/.local/state/codelink')
local instances_dir = state_dir .. '/instances'
local sock_dir = state_dir .. '/sock'

-- Machine-local settings, deliberately outside this (versioned) config so no
-- knowledge of a particular VCS or host lives here. Same directory the daemon
-- reads its providers from. Absent file is fine: the defaults below apply.
local local_config_path = vim.fn.expand('~/.local/share/codelink/nvim.json')

local pid = vim.fn.getpid()

--- Read the machine-local settings file. Cached: it is read once per session.
---@return table
local read_local_config = (function()
    local cached = nil
    return function()
        if cached then
            return cached
        end
        cached = {}
        local ok, lines = pcall(vim.fn.readfile, local_config_path)
        if ok and type(lines) == 'table' and #lines > 0 then
            local decoded_ok, decoded = pcall(vim.json.decode, table.concat(lines, '\n'))
            if decoded_ok and type(decoded) == 'table' then
                cached = decoded
            end
        end
        return cached
    end
end)()

-- The registry entry for this instance, kept in memory so that touch() can
-- rewrite it without recomputing the immutable fields.
local entry = nil

M.state_dir = state_dir

--- Path of this instance's registry file.
---@return string
function M.registry_path()
    return instances_dir .. '/' .. pid .. '.json'
end

--- Write `data` to `path` atomically: full write to a temp file, then rename.
--- The daemon reads these files on every hover, a torn write breaks it.
---@param path string
---@param data string
---@return boolean ok
local function write_atomic(path, data)
    local tmp = path .. '.tmp'
    local fd = vim.uv.fs_open(tmp, 'w', tonumber('644', 8))
    if not fd then
        return false
    end
    local written = vim.uv.fs_write(fd, data)
    vim.uv.fs_close(fd)
    if not written then
        vim.uv.fs_unlink(tmp)
        return false
    end
    if not vim.uv.fs_rename(tmp, path) then
        vim.uv.fs_unlink(tmp)
        return false
    end
    return true
end

--- Walk up from `start` looking for a project marker. Stops at $HOME or /.
---@param start string
---@return string
local function find_root(start)
    -- Precedence: explicit vim.g override, then the machine-local settings file,
    -- then plain VCS markers. Anything site- or VCS-specific belongs in the
    -- local file, never here.
    local markers = vim.g.codelink_root_markers or read_local_config().root_markers or { '.git', '.jj', '.hg', '.svn' }
    local home = vim.fs.normalize(vim.env.HOME or vim.fn.expand('~'))
    local dir = vim.fs.normalize(start)
    while true do
        for _, marker in ipairs(markers) do
            if vim.uv.fs_stat(dir .. '/' .. marker) then
                return dir
            end
        end
        if dir == home or dir == '/' or dir == '' then
            break
        end
        local parent = vim.fs.dirname(dir)
        if not parent or parent == dir then
            break
        end
        dir = parent
    end
    return vim.fs.normalize(start)
end

--- Serialize the in-memory entry to disk.
---@return boolean ok
local function flush()
    if not entry then
        return false
    end
    local ok, encoded = pcall(vim.json.encode, entry)
    if not ok then
        return false
    end
    return write_atomic(M.registry_path(), encoded)
end

--- Start the explicit socket and write the registry entry. Called on VimEnter.
function M.register()
    vim.fn.mkdir(instances_dir, 'p')
    vim.fn.mkdir(sock_dir, 'p')

    -- A leftover socket from a hard-killed nvim with a recycled pid would make
    -- serverstart fail, so drop it first.
    local sock = sock_dir .. '/' .. pid .. '.sock'
    if vim.uv.fs_stat(sock) then
        vim.uv.fs_unlink(sock)
    end

    -- This listens in addition to the automatic socket (vim.v.servername).
    local servername = nil
    local ok, res = pcall(vim.fn.serverstart, sock)
    if ok and type(res) == 'string' and res ~= '' then
        servername = res
    else
        vim.notify('codelink: serverstart failed for ' .. sock .. ': ' .. tostring(res), vim.log.levels.DEBUG)
    end

    local cwd = vim.fs.normalize(vim.fn.getcwd())
    local root = find_root(cwd)
    local now = os.time()

    entry = {
        v = 1,
        pid = pid,
        servername = servername or vim.NIL,
        auto_servername = vim.v.servername,
        cwd = cwd,
        -- Join key for matching a terminal's OSC 7 cwd, never updated.
        launch_cwd = cwd,
        root = root,
        label = vim.fs.basename(root),
        spawn_id = vim.env.CODELINK_SPAWN_ID or vim.NIL,
        started_at = now,
        last_focused = now,
    }

    return flush()
end

--- Refresh cwd / last_focused. Called on DirChanged, FocusGained, VimResume.
function M.touch()
    if not entry then
        return false
    end
    local cwd = vim.fs.normalize(vim.fn.getcwd())
    entry.cwd = cwd
    -- Recomputed so that a :cd into another project does not leave the daemon
    -- with a stale root/label. launch_cwd, started_at and spawn_id are immutable.
    entry.root = find_root(cwd)
    entry.label = vim.fs.basename(entry.root)
    entry.last_focused = os.time()
    return flush()
end

--- Drop the registry entry. Called on VimLeavePre. nvim removes its own sockets.
function M.unregister()
    local path = M.registry_path()
    vim.uv.fs_unlink(path .. '.tmp')
    vim.uv.fs_unlink(path)
    entry = nil
end

--- The current in-memory entry (nil before register()).
---@return table|nil
function M.entry()
    return entry
end

--- Jump to `path` at `line`, selecting through `end_line` when it is a range.
--- Runs inside vim.schedule, never on the RPC caller's stack.
---@param path string
---@param line integer|nil
---@param end_line integer|nil
local function open(path, line, end_line)
    -- A previous range jump leaves the user in visual mode, entering another
    -- jump from there would extend that selection instead of replacing it.
    if vim.fn.mode():find('^[vV\22]') then
        vim.cmd('normal! \27')
    end

    -- Find an existing buffer by EXACT name, vim.fn.bufnr() substring-matches.
    local target = nil
    for _, b in ipairs(vim.api.nvim_list_bufs()) do
        if vim.api.nvim_buf_is_loaded(b) and vim.fs.normalize(vim.api.nvim_buf_get_name(b)) == path then
            target = b
            break
        end
    end

    local win = target and vim.fn.win_findbuf(target)[1]
    if win then
        vim.api.nvim_set_current_win(win)
    else
        vim.cmd.edit(vim.fn.fnameescape(path))
    end

    local last = vim.api.nvim_buf_line_count(0)
    -- '#L0' occurs in real URLs, clamp it.
    local l = math.max(1, math.min(line or 1, last))
    local e = math.max(l, math.min(end_line or l, last))

    vim.api.nvim_win_set_cursor(0, { l, 0 })
    vim.cmd('normal! zz')

    if e > l then
        -- A real range: leave the user in a linewise visual selection, it
        -- persists and gv restores it.
        vim.cmd(('normal! %dGV%dG'):format(l, e))
    else
        -- Single line: flash only, a 1-line visual selection would strand the
        -- user in visual mode.
        vim.hl.range(0, ns, 'IncSearch', { l - 1, 0 }, { l - 1, -1 }, { regtype = 'V', inclusive = true, timeout = 1200 })
    end
end

--- Validate a decoded request synchronously and schedule the buffer work.
---@param req any
---@return table response
local function handle(req)
    if type(req) ~= 'table' then
        return { ok = false, error = 'invalid request: expected an object' }
    end

    local path = req.path
    if type(path) ~= 'string' or path == '' then
        return { ok = false, error = 'invalid request: missing path' }
    end
    path = vim.fs.normalize(vim.fn.fnamemodify(path, ':p'))
    if vim.fn.filereadable(path) ~= 1 then
        return { ok = false, error = 'not readable: ' .. path }
    end

    -- tonumber() yields nil for both absent and JSON null (vim.NIL).
    local line = tonumber(req.line)
    local end_line = tonumber(req.end_line)

    -- The buffer mutation must not block the RPC on nvim's UI state, e.g. a
    -- modal prompt or operator-pending mode.
    vim.schedule(function()
        local ok, err = pcall(open, path, line, end_line)
        if not ok then
            vim.notify('codelink: ' .. tostring(err), vim.log.levels.ERROR)
        end
    end)

    return { ok = true }
end

--- Remote entrypoint. Called by the daemon as:
---   nvim --server <sock> --remote-expr "luaeval('_G.__codelink_rpc(_A)', '<json>')"
--- Always returns a JSON string, never errors.
---@param payload string
---@return string
function _G.__codelink_rpc(payload)
    local ok, res = pcall(function()
        return handle(vim.json.decode(payload))
    end)
    if not ok then
        res = { ok = false, error = tostring(res) }
    end
    local encoded_ok, encoded = pcall(vim.json.encode, res)
    if not encoded_ok then
        return '{"ok":false,"error":"failed to encode response"}'
    end
    return encoded
end

return M
```

Requires Neovim 0.11+ (`vim.uv`, `vim.hl.range`).

## Checking it works

```vim
:CodelinkStatus
```

should print the registry path, `on disk: yes`, and the entry. Then, from a
shell:

```sh
codelink doctor            # lists live instances with their labels and sockets
ls ~/.local/state/codelink/instances/
```

An instance started before the config was in place stays invisible until it is
restarted.

## Security note

The daemon's root allowlist is not cosmetic. `mode:"new"` targets must sit
inside a root enumerated from `providers.json`, compared after resolving
symlinks. If your config sets `opt.exrc = true`, launching Neovim in an
arbitrary directory that happens to contain a `.nvim.lua` would be remote code
execution — the allowlist is what prevents a link from choosing that directory.
