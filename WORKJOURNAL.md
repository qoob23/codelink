# Work journal

Decisions and why. Not documentation — see `README.md` and
`providers.schema.md` for how things work.

## 2026-07-29 — initial build

### Shape of the thing

- **Named `codelink`, host-neutral by design.** Wanted GitHub and others later, so
  nothing in the name or the code ties it to one code host.
- **Site knowledge lives in untracked config only** (`providers.json`,
  `nvim.json`). The daemon, extension and Neovim module know nothing about any
  host, VCS or repo layout. Scrubbed 131 leaked references from the daemon and
  26 from the extension after the first pass got this wrong.
- **Daemon in Go, stdlib only.** Switched from the planned Node mid-design. A
  compiled binary also fixes TCC: an unsigned binary is keyed by cdhash, so
  every rebuild would re-prompt for Ghostty automation; a stable self-signed
  identity avoids that. Node would have re-prompted on every Homebrew upgrade.
- **Loopback HTTP daemon under launchd**, not a native-messaging host. Easier to
  `curl`, no per-browser manifest registration, works across browsers.
- **Instance registry as files**, not socket globbing. Editors write
  `~/.local/state/codelink/instances/<pid>.json`; the daemon never computes
  `stdpath('run')`, which sidesteps `$TMPDIR` differing under launchd.
- **RPC via `nvim --server --remote-expr luaeval(...)`** rather than a msgpack
  client. ~10 ms, round-trips UTF-8 and quotes, ~400 fewer lines to own.
- **Ghostty driven by AppleScript**, not `open -na`. Reuses the running app
  instead of spawning a second instance, and can focus an exact tab. `open` kept
  as the fallback when automation is denied.

### Security posture

- **Extension ID pinned via a manifest `key`.** The daemon's CORS allowlist
  accepts exactly one `chrome-extension://` origin, so the ID has to be stable
  across browsers and machines.
- **Anti-CSRF is the CORS policy itself.** Only the extension origin is ever
  echoed back — deliberately *not* the code host, even though that is where the
  links live. A custom header forces a preflight, which every web page fails.
- **The root allowlist is the control that actually matters.** Header/origin
  checks stop web pages but nothing running locally as the user. Since the nvim
  config sets `exrc = true`, spawning an editor in an attacker-chosen directory
  is RCE — so spawn targets must be roots the daemon itself enumerated.

### Behaviour calls

- **Injection scope ≠ link triage.** Content script runs on `<all_urls>`; the
  button appears only for links matching a provider. Scoping injection to
  provider hosts silently broke local HTML reports, tickets and chat logs that
  merely *mention* a link — no button, no error.
- **Show nothing, not a grey button,** when a link has no local checkout.
  Triage over-triggers on purpose, so this is the common case; a permanent dot
  on half the page trains you to ignore it.
- **Range → visual selection, single line → flash.** A one-line visual selection
  would strand you in visual mode.
- **New instance picker sorted by open-instance → recent-use → recency.** Dropped
  a "read-only checkout last" tiebreaker: it contradicted the recency rule, and
  the requested behaviour was plain most-recent-first.
- **Considered and rejected: restricting resolution to a subdirectory.** Current
  root behaviour judged fine as-is; no `scope` field added.
- **Browser names removed from docs.** Install steps are generic Chromium.

### Process

- **Three layers built in parallel by separate agents, then reviewed by a fourth.**
  The review earned its keep: it found that the extension and daemon had each
  implemented and *documented* a different `/open` body, so every click failed.
  Both had passed their own tests. Lesson: contracts between independently built
  layers need a test that spans them, not two tests that agree with themselves.
- **Regression tests must be shown to fail against the broken code.** Every fix
  today was verified by deleting the guard and confirming the failure first.
- **Diagnostics kept, not removed after debugging.** The overlay is a closed
  shadow root, so `localStorage.CODELINK_DEBUG` is the only way to observe it;
  request access logging is the only way to tell "click never arrived" from
  "click rejected". Both absences cost real time today.
- **Skill ships with the repo** (`skills/codelink-config`) rather than living in
  a personal skill tree, so an agent with a clone can install it on demand.

### Bugs worth remembering

- **SPA re-renders the `<a>` node between hover and click.** `reposition()` saw a
  detached anchor and tore the UI down in the same tick the picker was built.
  A pinned picker must outlive its link.
- **`blur` does not bubble but does capture.** A `window` capture listener
  received every element's blur, so `picker.focus()` dismissed the picker
  instantly — worked on the second click, never the first.
- **HTTP 200 did not mean success.** `/open` returned `ok:true` with an error
  string when the spawn handshake timed out, so a failed shift-click showed a
  green checkmark. Also raised the budget 500 ms → 5 s: a bare editor takes
  ~281 ms to register *before* the terminal is involved.

## 2026-07-29 — extracted from the dotfiles tree

- **Own repo, not a directory in the dotfiles.** The daemon and extension are a
  program with a build step, tests and a release surface; the dotfiles tree is
  for files that get stowed into place. History was replayed commit by commit
  with the `xdg-config/.config/codelink/` prefix stripped, rather than started
  fresh, so the reasoning above stays attached to the code it explains.
- **No checkout path appears in tracked files.** Extraction exposed how much had
  quietly assumed `~/.config/codelink`. The scripts now resolve from their own
  location and the daemon takes `$CODELINK_EXTENSION_DIR`; the LaunchAgent plist
  became a template rendered at install time, since launchd expands neither `~`
  nor `$HOME` and a literal path there would re-pin the whole thing to one
  machine.
- **The Neovim half moved in, as `nvim/`.** It was two files in a personal
  config, which is what let the daemon and the editor drift apart in the first
  place — the registry schema is a contract with three writers and one reader.
  Keeping the plugin beside `daemon/` means a change to the entry format is one
  commit that touches both sides. It stays a plain runtimepath directory with no
  `setup()`, so nothing about a plugin manager is assumed.
- **`root_markers` deliberately stayed in `nvim.json`.** It is the one setting
  that would name your VCS, and the whole point of the untracked-config split is
  that publishing this tree reveals nothing about where you work.
