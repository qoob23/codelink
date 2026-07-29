# providers.json schema

The one file that knows about specific code-hosting sites and where their
checkouts live locally. It is deliberately **outside** this versioned config, at:

```
~/.local/share/codelink/providers.json      # override with $CODELINK_PROVIDERS
```

Everything in `~/.config/codelink/` is host-agnostic. Adding a new site means
editing this file and running `codelink build-manifest` — no code changes.

## Top level

| Key | Type | Notes |
| --- | --- | --- |
| `version` | int | Schema version. Currently `1`. |
| `extensionId` | string | The pinned Chromium extension ID. The daemon allows CORS for `chrome-extension://<this>` and **nothing else**. Must match the id Chromium derives from the manifest's `key`, which `codelink build-manifest` reads from `$CODELINK_EXTENSION_KEY` or, failing that, `~/.local/share/codelink/extension_key.txt`. |
| `inject` | []string | Optional. Chromium match patterns for which **pages** the content script runs on. Defaults to `["<all_urls>"]`. |
| `providers` | array | One object per site. Evaluated in order; first host match wins. |

## `inject` — pages, not links

Two different questions, easily conflated:

- **Which pages does the content script run on?** → `inject`
- **Which links get a button?** → the provider `hosts`, checked per link

They are deliberately decoupled, and the default is to inject **everywhere**.
That is what makes a button appear on a page that merely *mentions* a repo
link — a local HTML report opened over `file://`, a ticket, a chat log, a
rendered Markdown preview. None of those live on a provider host, so scoping
injection to the provider hosts would silently do nothing there, with no button
and no error explaining why.

The content script's triage resolves each **link's** URL and compares its
hostname against the generated host list, so running everywhere costs one
delegated `mouseover` listener per page and shows nothing off-provider.

Set `inject` only to deliberately narrow that, e.g.:

```json
"inject": ["*://*.example.com/*", "file:///*"]
```

`file://` pages additionally need **"Allow access to file URLs"** enabled on the
extension's card. No manifest setting can grant it — Chromium requires the
toggle.

## Provider object

| Key | Type | Notes |
| --- | --- | --- |
| `id` | string | Stable identifier, echoed back in `/resolve`. |
| `hosts` | []string | Hostname globs. `*.example.com` matches any subdomain **and** the bare domain. Expanded into the extension's `content_scripts.matches` by `codelink build-manifest`. |
| `match` | []object | Ordered URL patterns — see below. |
| `hash` | string | Fallback fragment pattern, used when the winning `match` entry has no `hash` of its own. |
| `refParam` | string | Query parameter carrying a revision/branch (e.g. `rev`). |
| `defaultRef` | string | The ref that means "current". Anything else sets `refIsDefault:false` and adds a warning that the local file may differ. |
| `projectMarkers` | []string | Directory names that mark the start of source inside a package (`lib`, `src`, `test`, …). The *project* is the longest path prefix ending immediately before the first of these. |
| `roots` | []object | Where local checkouts live — see below. |

## `match` entries

```json
{ "path": "^/repo/(?P<repoPath>.+)$",
  "hash": "^file-(?P<repoPath>[^:]+?)(?::(?P<side>[LR])(?P<line>\\d+))?$" }
```

Evaluated in order. An entry wins when its `path` matches the URL path **and**,
if it declares a `hash`, the URL fragment matches that too. Otherwise evaluation
falls through to the next entry — so a narrow pattern (a diff view) can precede
a broad one (a plain file view) safely.

**These are Go RE2 regexes**, not PCRE:

- named groups are `(?P<name>…)`, *not* `(?<name>…)`
- no backreferences, no lookahead/lookbehind
- lazy quantifiers (`+?`) do work

Recognised group names — everything else is ignored:

| Group | Meaning |
| --- | --- |
| `repoPath` | **Required.** Repo-relative path to the file. |
| `line`, `endLine` | 1-based line range. Out-of-range values are clamped by Neovim, not here. |
| `col` | Column, currently parsed but unused. |
| `side` | `L`/`R` in a diff view. |

A `ref` group is **not** recognised. The revision comes only from the query
parameter named by `refParam`; a `(?P<ref>…)` group in a `path` or `hash` regex
is ignored like any other unrecognised name.

A URL that no provider matches returns `{"ok":false,"code":"NO_PROVIDER"}` — it
is not an error, just "not ours".

## `roots` entries

```json
{ "glob": "~/checkouts/*", "requireMount": true, "recencyPath": "~/.vcs/stores/{name}" }
{ "path": "~/main-checkout", "label": "trunk" }
```

| Key | Notes |
| --- | --- |
| `path` | A single root. `~` expanded. |
| `glob` | A shell glob expanding to many roots. Directories only; dotfiles skipped. |
| `label` | Display name only. Defaults to the root's basename. It has no effect on ordering. |
| `requireMount` | Only consider this root if it appears in `mount(8)`. Needed for FUSE checkouts, where a stale unmounted directory still exists and costs a ~500 ms `stat` to discover. |
| `recencyPath` | Template for the path whose mtime ranks this root, `{name}` = root basename. Needed when the checkout's own file mtimes are useless — a FUSE mount reports the *mount* time for every file it contains, identically. Point this at something on real local disk. Falls back to the root's own mtime. |

Roots are probed concurrently with a 400 ms per-root deadline and cached
(60 s positive, 10 s negative), because a cold FUSE `stat` costs 440–510 ms
while a warm one costs ~20 ms.

Matching roots are ranked by: whether the root already has an open instance,
then position in the `recent.json` LRU, then `recency` descending.

## Adding a site

1. Append a provider object here.
2. `codelink build-manifest` — regenerates `extension/manifest.json` and
   `extension/hosts.gen.js`.
3. Reload the unpacked extension in each browser.

`codelink doctor` prints the parsed result, the expanded roots with mount state
and recency, and the live Neovim instances. Start there when something looks
wrong.
