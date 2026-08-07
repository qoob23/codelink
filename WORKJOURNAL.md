# Work journal

Decisions and why. Not documentation — see `README.md`, `CONTRACT.md` and
`providers.schema.md` for how things work.

When an entry turns out to be wrong it is **corrected in place**, marked as a
correction, rather than left standing with a rebuttal further down. A reader
going top to bottom should never meet a claim this project no longer believes.
The corrections are kept visible rather than quietly edited away, because how a
belief was wrong is usually the more useful half.

## 2026-07-29 — initial build

### Shape of the thing

- **Named `codelink`, host-neutral by design.** Wanted GitHub and others later,
  so nothing in the name or the code ties it to one code host.
- **Site knowledge lives in untracked config only** (`providers.json`,
  `nvim.json`). The daemon, extension and Neovim plugin know nothing about any
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
- **Two independent checks on every request, not one.** Only the extension
  origin is ever echoed back — deliberately *not* the code host, even though
  that is where the links live. A custom header forces a preflight, which every
  web page fails.
  **Corrected after the audit:** this entry used to describe the CORS policy as
  *the* anti-CSRF control, with the token as belt-and-braces. That undersells
  the token. The header and the token are both required and neither alone
  suffices; more importantly, against a DNS-rebinding page that becomes
  same-origin with `127.0.0.1` the CORS layer contributes nothing at all and the
  token is the only thing left standing. The daemon now validates the `Host`
  header too, so the origin layer is not silently absent in the one case where
  it was being relied on.
- **The root allowlist is the control that actually matters.** Header and origin
  checks stop web pages but nothing running locally as the user: a local `curl`
  can send any header it likes. So spawn targets must be roots the daemon itself
  enumerated, compared after resolving symlinks.
  **Corrected after the audit:** true of the resolved *file* path, which held up
  under direct probing — and not true of the spawn **cwd**, which reached
  `ghostty.Spawn` without ever passing the check. See the audit entry.
- **What the allowlist actually buys.** This originally read: *"the nvim config
  sets `exrc = true`, so spawning an editor in an attacker-chosen directory is
  RCE"*. That was wrong when written. Since Neovim 0.9 an exrc file is executed
  only if it is already in the **trust list**, keyed on a hash of its contents;
  an unknown `.nvim.lua` prompts rather than running. The allowlist is therefore
  not what stops unvetted code executing. What it denies is the *choice of
  directory* — which of your already-trusted project configs gets sourced, and
  where the editor you are about to start typing into is rooted. Both are worth
  denying. Neither is arbitrary code execution.

### Behaviour calls

- **Injection scope ≠ link triage.** Content script runs on `<all_urls>`; the
  button appears only for links matching a provider. Scoping injection to
  provider hosts silently broke local HTML reports, tickets and chat logs that
  merely *mention* a link — no button, no error.
  **Corrected after the audit:** running everywhere also means any page can
  synthesise a `mouseover` on a link it planted and drive a resolve with no user
  present. The default is still right; trusting the page's events was not, and
  the listeners now require a trusted event.
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
  is verified by deleting the guard and confirming the failure first. A test
  that has never been red proves only that it runs.
- **Diagnostics kept, not removed after debugging.** The overlay is a closed
  shadow root, so `localStorage.CODELINK_DEBUG` is the only way to observe it;
  request access logging is the only way to tell "click never arrived" from
  "click rejected". Both absences cost real time.
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
- **The Neovim half moved in, and to the repo ROOT.** It was two files in a
  personal config, which is what let the daemon and the editor drift apart in
  the first place — the registry schema is a contract with three writers and one
  reader, and only one side of it was versioned. First attempt put it in
  `nvim/`, which reads tidier but is not installable: lazy.nvim clones a repo
  root, so a subdirectory plugin forces every user to write
  `dir = '/some/path/nvim'` — an install instruction that is really a
  workaround, and one that hardcodes a path the rest of the tree had just been
  cleaned of. At the root, `{ 'qoob23/codelink' }` is the whole spec.
- **`build = './install.sh'` makes the plugin manager the installer.** The clone
  already contains the daemon and the extension, and nothing assumes a checkout
  location, so the clone is a valid install root. `install.sh` runs
  `bootstrap.sh` itself when the keypair is missing: a one-line install gets
  exactly one hook, and without the keypair the failure is an opaque 403.
- **`root_markers` deliberately stayed in `nvim.json`.** It is the one setting
  that would name your VCS, and the whole point of the untracked-config split is
  that publishing this tree reveals nothing about where you work.

## 2026-07-29 — prepared for a public release

- **MIT.** `extension/LICENSES.md` had been deferring to "whatever license the
  enclosing repository carries", which resolved to nothing — i.e. all rights
  reserved, i.e. nobody could legally use it. A license file is not paperwork
  you add later; without it the repo is public in the sense of readable and
  useless.
- **A working GitHub provider ships in `providers.schema.md`.** `bootstrap.sh`
  writes a starter naming `example.com`, so a first run installed cleanly,
  resolved nothing, and offered no next step. The example is verified against
  `codelink doctor` rather than written from memory, and it documents the thing
  that surprises people: `repoPath` is repo-*relative*, so the owner and repo
  segments are skipped rather than captured, and a glob root therefore matches
  on the path alone. A link to `daemon/main.go` finds one clone; a link to
  `README.md` finds every clone that has one.
- **The Go floor is 1.22, not 1.26.** `go.mod` had been pinned to whatever
  toolchain happened to be installed. Tested rather than guessed: 1.22 builds
  and passes, 1.21 does not (`ghostty.go` ranges over an int, which 1.22
  introduced). Declaring a version two years newer than the code needs turns a
  plugin manager's `build` step into a failure nobody can diagnose.
- **Credits are explicit.** The Neovim mark is Jason Long's under CC BY 3.0,
  with the modifications stated; jsdom is the only third-party package anywhere,
  and only in the test suite.

## 2026-07-29 — security audit

An independent adversarial audit, run against the code rather than the README,
on the principle that the docs are the thing being tested. No critical finding.
The daemon, extension and documentation fixes were made in parallel, so this
records the calls, not the diffs.

- **One allowlist, two ways past it.** The containment check on resolved *file*
  paths held under direct probing — `..`, double-encoding, absolute paths and a
  directory symlink out of the root were all refused. The spawn **cwd** did not
  route through the same guard, so the directory an editor starts in was
  reachable by exactly the route the control was written to cover. A guard
  protects the call sites routed through it and nothing else, so enumerating
  those call sites is part of writing the guard — not something a later review
  is supposed to catch.
- **The extension trusted synthetic events.** A listener cannot tell a real
  hover from a `dispatchEvent` by a page script, and the content script runs on
  every page, so any site could plant a link and silently drive `/resolve` — an
  existence oracle for which private repositories you have cloned. The CORS
  policy keeps page scripts off the daemon's port; it does nothing here, because
  the request originates from the extension's own service worker. The button
  lives in the page's DOM, so **the page, not the origin check, is the boundary
  that matters for the UI half.**
- **Auditing the class beat fixing the instance.** The audit reported the
  `mouseover` leak. Sweeping *every* page-facing listener for the same defect
  turned up a worse one it had not: `keydown`, where a forged `Enter` while the
  picker is open runs `choose()` → `doOpen()` → `POST /open`, with forged arrows
  moving the selection first. A page could let you open the picker for one
  checkout and silently commit another. That is an **action**, not an oracle —
  strictly more serious than the finding that led to it. Worth remembering that
  a report names the instance it happened to find; the fix is only finished once
  the class has been swept.
- **Not every listener got the guard, deliberately.** `mouseout`, `pointerdown`
  and `blur` only tear the overlay down, and failing toward hidden is safe —
  a page can already get there by removing the anchor. `scroll` and `resize`
  are *deliberately* left open: layout libraries fire them synthetically, and
  honouring those is how the overlay stays glued to its link. Guarding
  everything uniformly would have been a regression dressed as rigour.
- **`exrc` was documented as RCE, and it is not.** Both `README.md` and
  `CONTRACT.md` claimed that spawning Neovim in an attacker-chosen directory
  containing a `.nvim.lua` is remote code execution. Checked against the
  installed Neovim's own `:help`: since 0.9 an exrc file runs **only if it is
  already in the trust list**, keyed on a content hash. The claim was inherited
  from how Vim's `exrc` used to work and never verified. Corrected rather than
  deleted — the allowlist still denies an attacker the choice of directory,
  which is worth denying — but the general lesson is the one worth keeping:
  **the threat model was written more confidently than the implementation
  warranted.** A security note that asserts a consequence has to cite the
  mechanism it rests on, or it ages into a false claim that also flatters the
  control it exists to justify.
- **Defence in depth where it was free.** `open(1)` was invoked through `$PATH`
  while `osascript` and `mount` were absolute, and the LaunchAgent puts a
  group-writable Homebrew directory first. The probe cache had no eviction and a
  page-controlled key. The registry read any file type, so a FIFO in the
  instances directory wedged every endpoint. None of these is an attack on its
  own; all of them are cheaper to fix than to argue about.
- **Two install costs now stated in the README rather than discovered.**
  `build = './install.sh'` means a plugin update rebuilds a binary and
  bootstraps a login LaunchAgent; `<all_urls>` reads to a user as "read and
  change all your data on all websites". Both are deliberate and both stay — but
  they are the first two things anyone will raise publicly, and a reader who
  meets them in a permission dialog rather than in the install section has been
  ambushed.
- **The plist renderer stopped using `sed`.** Not a vulnerability — the paths
  are the user's own — but a checkout under `A&B` rendered as
  `A__EXTENSION_DIR__B` with no error, because an unescaped `&` in a sed
  replacement means "the whole match". The obvious fix,
  `${plist//__BIN__/$BIN}`, is **worse**: bash 5.2 gave `&` that same meaning,
  so the output would depend on whether `/bin/bash` or a Homebrew bash ran the
  installer. Explicit literal splice plus XML escaping, verified byte-identical
  across both shells and with `plutil -lint`, against a path holding
  `& < > |` and a space.
- **The correction was itself wrong, and a review caught it.** The rewritten
  notes attributed Neovim's parent-directory exrc search to 0.9 along with the
  trust list. It arrived in **0.12**; 0.11 does not have it, and 0.11 is the
  floor this plugin declares — so a rationale citing the parent walk was false
  on the project's own minimum supported version. Established from the runtime's
  own `news-0.9.txt`, `news-0.11.txt` and `news.txt` rather than from `:help`,
  which only ever describes the version you happen to have installed. That is
  the trap: the first correction *was* checked against the installed Neovim, and
  the installed Neovim was 0.12. **Verifying a version-dependent claim requires
  reading the version history, not the current docs.** Two rounds of getting the
  same paragraph wrong is the argument for the correction policy at the top of
  this file.
- **Left deliberately unfixed: clickjacking.** The button is a one-click action
  at a position the page controls, so a page can put its bait link under a decoy
  and harvest the click. The suggested mitigation — swallow clicks arriving
  within ~300 ms of the panel appearing — trades a real cost on the main path
  for partial coverage. Requiring trusted events removes the zero-interaction
  half of the attack; the rest is a product decision about friction, recorded
  here so it is a choice rather than an oversight.

## 2026-08-07 — the icon is the verdict

The hover flow painted a plain button instantly and resolved behind it, with a
spinner reserved for a slow daemon. Replaced wholesale: nothing paints until a
verdict is in, and there is no loading state at all.

- **A pre-verdict button was a promise the extension could not keep.** It looked
  clickable before anyone knew whether there was anything to open, and the
  spinner advertised latency the daemon does not have. Now the icon appearing
  *is* the answer — click it and something happens, warning or editor.
- **Per-project optimism keeps it feeling instant without lying twice.** Once
  any URL of a project has proven the repo local, later links in it paint the
  ready icon before their own verdict and are corrected in place if the bet was
  wrong. `FILE_NOT_LOCAL` counts as proof — the repo *is* here, only the file is
  not — which is the case the user asked for by name: ready icon first, warning
  when the daemon says so.
- **The project key is a client-side guess, and that is fine.** Host plus the
  first two path segments, learnt and looked up the same way, so it is
  self-consistent without the extension growing a URL grammar it is forbidden
  to have. It only gates an *early* paint; every click still goes through a
  real verdict. A GitLab-style deep namespace collapses a group into one key —
  acknowledged in the code rather than engineered away, because the verdict's
  own gate decides what stays painted.
- **A click on the optimistic button is the dwell.** Waiting out a timer after
  an explicit click would be latency theatre; the click resolves immediately
  and performs the open it stood for. The intent is claimed once, at the top of
  the reply handler, so a down/error verdict can never leave it armed for a
  retry the user did not mean.
- **Optimism obeys the badge switches** (added same day, after the first cut
  shipped the flaw as a documented trade-off and it was rejected): with the
  can't-resolve warning silenced for a repo, the optimistic icon must not paint
  either — the only visible outcome of being wrong would be a paint immediately
  withdrawn. The switch means "put nothing on the page about this repo unless
  it is real", and optimism is part of "anything".
- **Review caught a teardown race the tests could not see.** The hide timer
  armed by a leaving pointer had no `busy` guard, so an intent-driven `/open`
  could be torn down mid-flight: file opens, no checkmark, failures swallowed.
  The picker leg was already safe only because `openPicker` happens to call
  `cancelHide()`; `doOpen` now does the same. A guard that exists on one leg of
  a fork and not the other is the same lesson as the audit's cwd finding.
- **Mutation-checking the new tests earned its keep twice.** The intent-safety
  pins survived every single-point mutation of the abandonment path — it is
  triple-guarded, so no one deletion breaks it — which itself was worth
  learning: the check with teeth turned out to be the error-retry one. And the
  hover-time-vs-learn-time gate pin was proven against the exact plausible
  regression (gate evaluated at learn time), not against a strawman.
