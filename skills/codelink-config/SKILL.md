---
name: codelink-config
description: Use when configuring, validating or debugging codelink — the daemon + browser extension that opens a code-hosting link in a local Neovim. Covers adding and validating a provider in providers.json, bootstrapping on a new machine, tuning injection scope and Neovim root markers, and diagnosing failures (no button, button does nothing, 403, spawn problems). Trigger on "codelink", "providers.json", "codelink doctor", "add a provider", "open in neovim from the browser", or any codelink button/daemon misbehaviour.
---

# codelink-config

## The one thing to understand first

Every site-specific fact lives in **untracked** files. The daemon, extension and
Neovim module contain none of it, by design.

| File | Read by | Holds |
| --- | --- | --- |
| `~/.local/share/codelink/providers.json` | daemon | hosts, URL patterns, checkout roots, extension id, injection scope |
| `~/.local/share/codelink/nvim.json` | Neovim module | `root_markers` |
| `~/.local/share/codelink/codelink-ext.pem` | `bootstrap.sh` | extension keypair (**secret**) |

Never "fix" a problem by putting a hostname or repo path into the daemon,
extension or nvim source. That is always the wrong layer.

Generated and gitignored — never hand-edit, they are overwritten:
`extension/manifest.json`, `extension/hosts.gen.js`, `extension/token.gen.js`.

## Always start here

```sh
codelink doctor                 # providers, roots, mount state, instances, token, port
codelink doctor '<url>'         # ...plus a full resolve of that URL
tail -20 ~/.local/state/codelink/daemon.err.log   # one line per request
```

Quote the URL — `?` and `#` are shell metacharacters.

---

## Task: add or validate a provider

### 1. Get the shape of a real URL

Collect several real examples: a plain file link, one with a line, one with a
line *range*, one on a diff/review page, and one that is **not** a file (a
ticket, a settings page). The negatives matter as much as the positives.

### 2. Write the entry

```json
{
  "id": "example",
  "hosts": ["*.example.com"],
  "match": [
    { "path": "^/repo/(?P<repoPath>.+)$" },
    { "path": "^/pr/(?P<pr>\\d+)/files/\\d+/?$",
      "hash": "^file-(?P<repoPath>[^:]+?)(?::(?P<side>[LR])(?P<line>\\d+))?$" }
  ],
  "hash": "^L(?P<line>\\d+)(?:-L?(?P<endLine>\\d+))?$",
  "refParam": "rev",
  "defaultRef": "main",
  "projectMarkers": ["lib", "src", "test", "bin"],
  "roots": [
    { "glob": "~/checkouts/*" },
    { "path": "~/main-checkout", "label": "main" }
  ]
}
```

Traps, each of which has actually bitten:

- **Go regexes are RE2.** Named groups are `(?P<name>...)`, *not* `(?<name>...)`.
  No backreferences, no lookahead/lookbehind. Lazy quantifiers do work.
- **`match` entries are ordered**, first hit wins. Put narrow patterns (diff
  views) before broad ones (plain file view). An entry only wins if its `path`
  matches *and*, when it declares a `hash`, the fragment matches too — otherwise
  evaluation falls through, which is what makes the ordering safe.
- **Optional groups must really be optional.** Line ranges and diff-side
  suffixes are frequently absent; a pattern that requires them breaks the
  common case.
- **Range syntax varies by host.** Some emit `#L12-20`, others `#L12-L20`.
  `(?:-L?(?P<endLine>\d+))?` accepts both — prefer it unless you have
  reason not to.
- **Don't assume a URL prefix is a file path.** A host often serves review or UI
  pages under a path that *looks* file-ish. Those must NOT parse.
- **Out-of-range lines are fine.** `#L0` and lines past EOF are clamped by
  Neovim, not by the daemon. Don't add guards for them.

### 3. Roots

| Key | When you need it |
| --- | --- |
| `glob` / `path` | where checkouts live; `~` expands |
| `label` | display name; defaults to the directory basename |
| `requireMount` | checkouts on a mounted filesystem. An unmounted mountpoint is still an ordinary directory, so nothing else can tell a stale checkout from a live one — and probing it costs a full cold stat |
| `recencyPath` | when the backend reports an **identical mtime for every file** (the mount time), making file mtime useless for ranking. Point it at something on real local disk; `{name}` expands to the root's basename |

Roots are probed concurrently with a 400 ms per-root deadline and cached
(60 s positive / 10 s negative), because a cold stat on a network-backed
checkout can cost ~500 ms against ~20 ms warm.

### 4. Validate — do not skip this

```sh
codelink doctor '<file url with a line>'      # expect kind:file, line, project, roots
codelink doctor '<file url with a range>'     # expect line AND endLine
codelink doctor '<diff/review file url>'      # expect repoPath + line
codelink doctor '<a NON-file url on the host>'  # expect ok:false, NO_PROVIDER
codelink doctor '<url on an unrelated host>'    # expect ok:false, NO_PROVIDER
```

Then check `project`/`projectName` look right — the project is the longest path
prefix ending immediately before the first `projectMarkers` segment. If it is
wrong, the marker list is wrong for that repo layout.

`rootCandidates` should list every checkout that really contains the file, most
recent first. Empty means the path does not exist locally under any root — check
whether the checkout is path-filtered or simply not mounted.

### 5. Apply

```sh
codelink build-manifest                                   # only if hosts changed
launchctl kickstart -k gui/$(id -u)/com.qoob23.codelink   # reload providers.json
```

Reload the extension card in the browser if `hosts.gen.js` or `manifest.json`
changed. Chromium does not pick up file changes on its own.

---

## Task: tune injection scope

`inject` and `hosts` answer **different questions**:

- `inject` → which **pages** the content script runs on. Default `["<all_urls>"]`.
- `hosts` → which **links** get a button, checked per link against the link's own
  URL, never the page's.

Injecting everywhere is deliberate: it is what makes a button appear on a page
that merely *mentions* a repo link — a local HTML report, a ticket, a chat log.
Narrowing `inject` to the provider hosts silently disables all of those, with no
button and no error to explain it.

`file://` pages additionally require **"Allow access to file URLs"** on the
extension card. No manifest setting can grant it.

## Task: Neovim root markers

`~/.local/share/codelink/nvim.json`:

```json
{ "root_markers": [".git", ".jj", ".hg"] }
```

Walks up from the buffer's cwd until a directory contains one of these; the
result becomes `root` (and `label` = its basename) in the instance registry.
Add whatever your VCS uses. `vim.g.codelink_root_markers` overrides the file.

Restart nvim to re-register; existing instances keep their old root.

---

## Task: bootstrap a new machine

```sh
~/soft/codelink/bootstrap.sh    # keypair + starter configs; never overwrites
$EDITOR ~/.local/share/codelink/providers.json
~/soft/codelink/install.sh      # build, sign, generate manifest, load agent
```

Then load the extension unpacked from `$(cd ~/soft/codelink/extension && pwd -P)`
and **verify the id on the card matches what `bootstrap.sh` printed**.

To keep one id across machines, copy `codelink-ext.pem` (a private key) and
re-run `bootstrap.sh`. Otherwise generate a fresh keypair and update
`extensionId` in `providers.json` to match.

---

## Diagnosing

Work down this table; each row is distinguishable from the others by evidence,
so don't guess.

| Symptom | Check | Cause |
| --- | --- | --- |
| No button anywhere, any page | is the content script injected? | `inject` too narrow, or extension not loaded |
| No button only on `file://` | extension card | "Allow access to file URLs" is off |
| No button for one link | `codelink doctor '<url>'` | `NO_PROVIDER` (pattern doesn't match) or no local checkout — both correctly show nothing |
| Amber button | `curl` the daemon / `launchctl print` | daemon down |
| Every request 403 | `codelink doctor` id vs the card's id | manifest lost its `key` → Chromium derived the id from the install path → the origin allowlist rejects it |
| 403 after a token change | `token.gen.js` vs `~/.local/state/codelink/token` | extension cached the old token — reload the card |
| Button does nothing | access log | see below |

The access log distinguishes the remaining cases cleanly:

- **no line at all** → the click never reached the daemon; the fault is in the
  content script. Enable the in-page channel: `localStorage.CODELINK_DEBUG = '1'`,
  then click and read the `[codelink]` lines. The overlay lives in a *closed*
  shadow root, so this channel is the only way to observe it.
- **`GET /resolve` then nothing** → the click path never issued `/open`.
- **`POST /open -> 4xx`** → contract or auth problem; read the body.
- **`POST /open -> 200`** → the daemon accepted it. Note 200 does **not** mean
  success: the body carries `ok` and a `code`. `SPAWN_TIMEOUT` means the editor
  window was created but never registered in time.

Instances are pruned when the pid dies, so a `kill -9`'d editor disappears from
the picker on the next request. Editors started before codelink was installed
are invisible until restarted.

## Anti-patterns

- Putting a hostname, repo path or VCS name into daemon/extension/nvim source.
- Editing `manifest.json`, `hosts.gen.js` or `token.gen.js` by hand.
- Using `(?<name>...)` instead of `(?P<name>...)`.
- Regenerating the keypair without updating `extensionId`.
- Declaring a provider "working" without running the negative `codelink doctor`
  cases — a pattern that matches everything looks fine until it puts a button on
  every link on the site.
