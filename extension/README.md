# codelink — browser extension

Hover a link to a source file on a code-hosting site; a small Neovim button
appears next to it. Click it and the file opens at the right line in a Neovim
instance you already have running. Shift-click opens a new one.

The extension is deliberately dumb. It does no URL parsing, no checkout
lookup and no line-number arithmetic — it forwards the hovered URL to the local
`codelink` daemon on `127.0.0.1:47391` and renders whatever comes back.

---

## Layout

| File | Purpose |
| --- | --- |
| `manifest.template.json` | Committed manifest with `__EXTENSION_KEY__` / `__MATCHES__` placeholders. Input to `codelink build-manifest`. |
| `background.js` | MV3 service worker. The only place that calls `fetch`. Talks to the daemon, normalises every outcome into one reply shape. |
| `content.js` | Hover triage, the floating button, the picker, all shadow-DOM UI. Contains the inlined copies of `content.css` and `nvim-mark.svg`. |
| `content.css` | Editable source for the shadow-root stylesheet. Not referenced by the manifest — see *Rebuilding* below. |
| `nvim-mark.svg` | The Neovim mark, recolored to `currentColor`. Source for the inlined SVG and for the PNG icons. |
| `icons/16.png`, `icons/48.png`, `icons/128.png` | Toolbar / extensions-page icons. |
| `LICENSES.md` | Attribution for the Neovim mark (Jason Long, CC BY 3.0) and the statement of modification. |

**Generated, not committed** (all three are gitignored and are produced by the
Go daemon — do not hand-edit them, they get overwritten):

| File | Produced by | Contents |
| --- | --- | --- |
| `manifest.json` | `codelink build-manifest` | `manifest.template.json` with the key and match list substituted. |
| `hosts.gen.js` | `codelink build-manifest` | `self.CODELINK_HOSTS = ["*.example.com"];` |
| `token.gen.js` | `codelink serve` | `self.CODELINK_TOKEN = "…";` — the shared secret for the daemon's HTTP API. |

The extension is packaged with a fixed `key`, so its ID is stable at
**`hllcocddojiooecjhfcnfcijkbcincmd`** in every profile and every browser. The
daemon can allowlist that origin.

---

## Loading it unpacked

First make sure the generated files exist, otherwise the browser will refuse the
directory (a missing `hosts.gen.js` fails the `content_scripts` load):

```sh
codelink build-manifest      # writes manifest.json + hosts.gen.js
codelink serve               # writes token.gen.js
ls ~/.config/codelink/extension/{manifest.json,hosts.gen.js,token.gen.js}
```

### Loading it

1. Open the extensions page. Most Chromium builds use `chrome://extensions`;
   some rebrand the scheme (`browser://extensions`, `vivaldi://extensions`, …)
   or redirect to it. Either way it is reachable from
   ⋮ → Extensions → Manage Extensions.
2. Toggle **Developer mode** on, usually top right. Localised builds label it in
   their own language.
3. **Load unpacked** → select `~/.config/codelink/extension`.

Some Chromium builds restrict normal installs to a vendor catalogue and/or the
Chrome Web Store; developer-mode unpacked loading is the supported escape hatch
and is what this extension expects. If a browser silently disables the extension
after a restart, re-enable it from the same page.

The manifest `key` pins the extension ID, so it is identical in every browser
you load it into — which is exactly what the daemon's CORS allowlist is keyed
to. Check the ID shown on the card matches the one `bootstrap.sh` printed.

### After the daemon regenerates a file

Unpacked extensions read their resources from disk, but the browser caches the
package. After `codelink build-manifest` or the first ever `codelink serve`
(which creates `token.gen.js`), **hit Reload (⟳) on the extension card**.
Without that the service worker keeps the old — or absent — token, and every
link shows the amber "daemon isn't running" button.

---

## Rebuilding

### `content.css` / `nvim-mark.svg` → `content.js`

Both are inlined into `content.js` as `String.raw` literals between
`codelink:inline:*:begin` / `:end` markers. **Editing `content.css` alone does
nothing** until you re-run the inliner:

```sh
cd ~/.config/codelink/extension
node - "$PWD" <<'EOF'
const fs = require('fs'), path = require('path');
const dir = process.argv[2], js = path.join(dir, 'content.js');
let src = fs.readFileSync(js, 'utf8');
const block = (name, body) => {
  const re = new RegExp('(/\\* codelink:inline:' + name + ':begin \\*/\\n)[\\s\\S]*?(  /\\* codelink:inline:' + name + ':end \\*/)');
  if (!re.test(src)) throw new Error('marker block not found: ' + name);
  src = src.replace(re, (m, a, b) => a + body + '\n' + b);
};
const css = fs.readFileSync(path.join(dir, 'content.css'), 'utf8').trim();
const svg = fs.readFileSync(path.join(dir, 'nvim-mark.svg'), 'utf8')
  .replace(/<!--[\s\S]*?-->/g, '').replace(/\s*\n\s*/g, ' ').trim();
for (const [n, s] of [['css', css], ['svg', svg]])
  if (s.includes('`') || s.includes('${')) throw new Error(n + ': backtick or ${ cannot be inlined');
block('css', '  var CSS = String.raw`\n' + css + '\n`;');
block('svg', '  var MARK_SVG = String.raw`' + svg + '`;');
fs.writeFileSync(js, src);
console.log('inlined: css ' + css.length + 'B, svg ' + svg.length + 'B');
EOF
node --check content.js
```

**Why inline instead of loading at runtime?** Both alternatives are worse:

- A manifest `"css"` entry injects *page-level* CSS. It would escape the shadow
  root and restyle the host page — exactly what the shadow root exists to
  prevent.
- `fetch(chrome.runtime.getURL('content.css'))` from a content script is
  governed by the **page's** CSP `connect-src` (the same reason `content.js`
  never calls `fetch` for the daemon), needs a `web_accessible_resources` entry
  that advertises the extension ID to every page for fingerprinting, and is
  async — the first hover would paint unstyled.

Inlining is synchronous, needs no extra permission, and leaks nothing. The cost
is this rebuild step, and the inliner refuses to run if a source ever grows a
backtick or `${`.

### Icons

```sh
cd ~/.config/codelink/extension
# from the ORIGINAL two-colour mark, not the currentColor one
for n in 16 48 128; do
  rsvg-convert -h $((n * 86 / 100)) -f png /path/to/neovim-mark-flat.svg -o /tmp/raw-$n.png
  magick /tmp/raw-$n.png -background none -gravity center -extent "${n}x${n}" "icons/$n.png"
done
sips -g pixelWidth -g pixelHeight icons/128.png
```

---

## Wire contract with the daemon

**The daemon's shapes below are canonical** — this documents what the daemon
actually does, verified against the live process, not what the extension would
prefer. The service worker adds `X-Codelink-Client: ext` and
`X-Codelink-Token: <token>` to both requests.

### `GET /resolve?url=<urlencoded>`

Real 200 body for
`https://code.example.com/repo/src/pkg/widget/lib/main.go#L3-7`:

```jsonc
{
  "ok": true,
  "parsed": {
    "provider": "example",
    "repoPath": "src/pkg/widget/lib/main.go",
    "line": 3,
    "endLine": 7,            // null when the link carries no range
    "col": null, "side": null, "ref": null,
    "refIsDefault": true,    // false -> badge on the button + warnings[0] in the tooltip
    "project": "src/pkg/widget",
    "projectName": "widget",
    "kind": "file"           // "unsupported" when nothing local matches
  },
  "openInstances": [         // Neovim instances already running
    { "id": "61981",         // STRING pid — the /open target for mode "existing"
      "label": "feature-a",
      "root": "/Users/you/checkouts/feature-a",
      "cwd":  "/Users/you/checkouts/feature-a/src/pkg/widget",
      "localPath": "…/lib/main.go",
      "inProject": true, "lastFocused": 1785320438, "focusable": "heuristic" }
  ],
  "rootCandidates": [        // checkouts containing the path, ALREADY sorted
    { "root": "/Users/you/checkouts/feature-a", // most-recent-first by the daemon —
      "label": "feature-a",                     // the extension never re-sorts
      "localPath": "…/lib/main.go",
      "recency": 1784796718,
      "hasOpenInstance": false }
  ],
  "warnings": []             // e.g. "rev=foo — pinned revision; local file may differ"
}
```

`repoPath`, `line` and `endLine` live **inside `parsed`** and must be lifted out
for `/open` — see below.

Field tolerance in the extension: a row's *display* path is read as
`cwd ?? root ?? path ?? dir` and `label` falls back to that path's basename;
missing arrays are treated as empty. The `/open` **target** is resolved
separately and strictly (`id` or `root`), never from the display path.

**Both lists empty** is the "nothing local matches" verdict (`parsed.kind ==
"unsupported"`, `warnings: ["no local checkout contains this path"]`): no button
is shown at all and the URL is cached as a skip.

### `POST /open`

The body is **flat**. There is no `url`, no nested `parsed`, and no `instance` /
`root` object — sending those yields
`{"ok":false,"error":"repoPath is required","code":"BAD_REQUEST"}`.

```jsonc
// open in an instance that is already running
{ "mode": "existing",
  "target": "61981",                          // openInstances[i].id — a STRING
  "repoPath": "src/pkg/widget/lib/main.go",
  "line": 3,
  "endLine": 7,                               // null when there is no range
  "focus": true }

// start a new instance in a checkout
{ "mode": "new",
  "target": "/Users/you/checkouts/main",      // rootCandidates[i].root — abs path
  "repoPath": "src/pkg/widget/lib/main.go",
  "line": 3,
  "endLine": null,
  "focus": true }
```

`endLine: null` and omitting `endLine` both decode fine; the extension always
sends the key explicitly.

### Errors

The service worker normalises every outcome into
`{ok:false, kind, error, status?, code?}`:

| Daemon response | `kind` | Extension behaviour |
| --- | --- | --- |
| `fetch` rejects (nothing on 47391) | `daemon-down` | amber button, click-to-copy `launchctl kickstart -k gui/$(id -u)/com.qoob23.codelink` |
| **200** `{"ok":false,"code":"NO_PROVIDER"}` — *no `error` field* | `api` | **silent skip**, cached negatively. Not an error: the URL simply is not a repo link (an issue ticket, a wiki page). Keyed on `code`, never on status. |
| 401 / 403 (plain-text `forbidden` body) | `auth` | amber button telling the user to hit Reload on the extension card; the SW drops its cached token and re-reads `token.gen.js` with a cache-buster |
| non-2xx with a JSON body | `api` | `code` and `status` preserved; 4xx cached as a skip |
| non-2xx, non-JSON | `http` | generic error tooltip |
| 200 with `{"ok":false,…}` | `api` | as above |

`code: "INSTANCE_GONE"` is special-cased: the extension re-runs `/resolve` once
and re-opens the picker with the fresh list.

---

## Behaviour notes

- **Triage is deliberately coarse.** A link qualifies if its host matches
  `CODELINK_HOSTS` and its path has ≥ 3 non-empty segments. That is all. The
  daemon owns the URL grammar (its regexes are RE2, not JS) and decides
  file-vs-directory by stat-ing the path, so the extension over-triggers and
  lets `/resolve` say no.
- **Hover.** ~150 ms debounce before asking the daemon; results are cached per
  URL for the session, so re-hovering is instant.
- **The hand-over.** A single shared 120 ms timer keeps the button alive while
  the pointer travels from the link to it. Leaving *either* the link or the
  button starts it; entering *either* cancels it.
- **The picker is modal.** Once open it ignores hover-out; close it with Esc,
  Tab, a click outside, or by picking a row. ↑/↓ move, Enter opens, mouse hover
  moves the selection too.
- **SPA-safe.** Event delegation in the capture phase (so the site's own
  `stopPropagation` cannot suppress it), and the overlay host is appended to
  `document.documentElement`, not `<body>`, because a single-page app re-renders
  `<body>` and would take the overlay with it.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| Extension will not load at all | `hosts.gen.js` or `manifest.json` missing — run `codelink build-manifest`. |
| Every link shows the amber button | Daemon down, or the SW is holding a stale/absent token. Start the daemon, then Reload the extension card. |
| No button ever appears | Host not in `CODELINK_HOSTS`, or the path has fewer than 3 segments, or the daemon answered `NO_PROVIDER` (not a repo link), or it returned two empty lists (nothing local matches). All four are silent by design. |
| Button appears but the picker is empty | `openInstances` and `rootCandidates` disagreeing with the click path — check the daemon's `/resolve` output directly with `curl`. |

Click the toolbar icon at any time for a health check: the badge shows a green
`ok` if the daemon answered, an amber `!` if it did not.
