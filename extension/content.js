'use strict';

/*
 * codelink — content script.
 *
 * Responsibilities, in order:
 *   1. coarse triage of hovered links (deliberately dumb — the daemon owns the
 *      real URL grammar and decides file-vs-directory by stat-ing the path);
 *   2. a floating Neovim button in a closed shadow root;
 *   3. relaying everything to the service worker.
 *
 * Hard rule: there is no fetch() in this file, and there must never be one.
 * Content-script network requests are governed by the *page's* CSP
 * `connect-src`, so a request to 127.0.0.1 would be blocked on the very sites
 * we care about. All HTTP goes through background.js.
 */

(function () {
  if (self.__codelinkContentLoaded) return;
  self.__codelinkContentLoaded = true;

  // ---------------------------------------------------------------- constants

  var RESOLVE_DELAY = 150; // dwell before we bother the daemon; the PAINT is instant
  var SPIN_DELAY = 200; // resolve in flight this long before the spinner shows
  var HIDE_DELAY = 120; // grace period so the mouse can travel link -> button
  var OK_DELAY = 900; // how long the success checkmark stays up
  var MIN_SEGMENTS = 3; // coarse triage: nav routes are shorter than file paths
  var CACHE_MAX = 400; // resolve-cache ceiling, LRU-evicted
  var CACHE_TTL = 10000; // ms a machine-state-dependent resolve stays fresh
  var GAP = 4; // px between the link box and the button
  var BTN = 33; // .cl-btn width/height — must match content.css, see reposition()
  var POINTER_DX = 10; // px right of the pointer where the button's left edge lands
  // Rough tooltip box, used only to pick which side of the button it takes. It
  // has no layout yet at the moment we decide, and being a few px out only ever
  // costs a flip that was not strictly needed.
  var TIP_W = 280;
  var TIP_H = 96;
  var KICKSTART = 'launchctl kickstart -k gui/$(id -u)/com.qoob23.codelink';
  var SVG_NS = 'http://www.w3.org/2000/svg';

  // -------------------------------------------------- inlined build artefacts
  //
  // content.css and nvim-mark.svg are the editable sources; they are inlined
  // here rather than loaded at runtime. Both alternatives are worse:
  //   * a manifest `css` entry injects page-level CSS, which leaks out of the
  //     shadow root and can restyle the host page;
  //   * fetch()/<img src=chrome.runtime.getURL(...)> from a content script goes
  //     through the *page's* CSP (connect-src / img-src) and needs a
  //     web_accessible_resources entry, which advertises the extension ID to
  //     every page for fingerprinting — and it is async, so the first hover
  //     would paint unstyled.
  // Inlining is synchronous, needs no extra permissions and leaks nothing.
  // Regenerate after editing either source — see README.md.

  /* codelink:inline:css:begin */
  var CSS = String.raw`
/* codelink — shadow-root stylesheet.
 *
 * This file is the EDITABLE SOURCE. It is never referenced from manifest.json:
 * it is inlined verbatim into content.js between the codelink:inline:css
 * markers and injected as a <style> element into the closed shadow root.
 * After editing, re-run the inliner (see README.md) or content.js keeps the
 * stale copy.
 *
 * Page CSS cannot reach into the shadow root, but *inherited* properties
 * (font, color, line-height, ...) do cross the shadow boundary through the
 * host, so #panel resets every inheritable property it cares about.
 */

:host {
  all: initial;
}

#panel {
  position: fixed;
  top: 0;
  left: 0;
  display: none;
  box-sizing: border-box;

  /* hard reset of everything the page could have handed us by inheritance */
  margin: 0;
  padding: 0;
  font: 500 12px/1.35 -apple-system, BlinkMacSystemFont, "SF Pro Text", "Segoe UI", system-ui, sans-serif;
  letter-spacing: normal;
  word-spacing: normal;
  text-transform: none;
  text-indent: 0;
  text-align: left;
  text-shadow: none;
  direction: ltr;
  white-space: normal;
  -webkit-font-smoothing: antialiased;

  color: #1f2328;
  --cl-surface: #ffffff;
  --cl-surface-hi: #f1f3f5;
  --cl-border: rgba(0, 0, 0, 0.16);
  --cl-text: #1f2328;
  --cl-text-dim: #6b7280;
  --cl-accent: #57a143;
  --cl-warn: #b26b00;
  --cl-warn-soft: rgba(178, 107, 0, 0.12);
  /* the can't-resolve cross. Deliberately NOT --cl-warn: an amber button means
   * "the daemon is down, do something about it", while this one means "there is
   * nothing here for you" and must not read as the same class of problem. */
  --cl-miss: #d1443c;
  --cl-shadow: 0 2px 10px rgba(0, 0, 0, 0.18), 0 0 0 0.5px rgba(0, 0, 0, 0.06);
  --cl-sel: rgba(87, 161, 67, 0.16);
}

#panel.is-open {
  display: block;
}

/* ---------- the round button ---------- */

.cl-btn {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: center;
  box-sizing: border-box;
  /* 33px, i.e. 1.5x the 22px this started at. Every other size in this block and
   * the three below it is that same 1.5 applied to its own original, never to an
   * intermediate value, so the ratio stays exactly recoverable if it is resized
   * again. content.js repeats the 33 as its BTN constant — both as the
   * offsetWidth/offsetHeight fallback and as the height it centres the panel on
   * the link's line box against — so changing it here alone makes the button sit
   * off its link. The 1px border is deliberately NOT scaled: it is a hairline
   * shared with the tooltip and the picker, and thickening only the button's
   * would break that family. */
  width: 33px;
  height: 33px;
  padding: 0;
  margin: 0;
  border: 1px solid var(--cl-border);
  border-radius: 50%;
  background: var(--cl-surface);
  color: var(--cl-accent);
  box-shadow: var(--cl-shadow);
  cursor: pointer;
  pointer-events: auto;
  font: inherit;
  -webkit-appearance: none;
  appearance: none;
  opacity: 0;
  transform: scale(0.86);
  transition: opacity 90ms ease-out, transform 90ms ease-out, color 90ms linear, border-color 90ms linear;
}

#panel.is-open .cl-btn {
  opacity: 1;
  transform: scale(1);
}

.cl-btn:hover {
  background: var(--cl-surface-hi);
}

.cl-btn:focus-visible {
  outline: 2px solid var(--cl-accent);
  outline-offset: 1px;
}

.cl-btn .cl-mark {
  display: block;
  width: 15px;
  height: 18px;
  color: inherit;
  fill: currentColor;
  pointer-events: none;
}

/* loading: the mark fades back, a thin ring spins */
.cl-btn.is-loading {
  color: var(--cl-text-dim);
}

.cl-btn.is-loading .cl-mark {
  opacity: 0.25;
}

.cl-spin {
  display: none;
  position: absolute;
  inset: 3px;
  border-radius: 50%;
  /* 2.25 rather than a rounded 2: this ring is under a continuous rotate, so it
   * never lands on the pixel grid at any frame and an integer buys nothing. */
  border: 2.25px solid transparent;
  border-top-color: var(--cl-accent);
  animation: cl-rot 620ms linear infinite;
  pointer-events: none;
}

.cl-btn.is-loading .cl-spin {
  display: block;
}

@keyframes cl-rot {
  to {
    transform: rotate(360deg);
  }
}

/* daemon not reachable */
.cl-btn.is-down {
  color: var(--cl-warn);
  border-color: var(--cl-warn);
  background: var(--cl-warn-soft);
}

/* success checkmark */
.cl-btn.is-ok {
  color: var(--cl-accent);
  border-color: var(--cl-accent);
}

.cl-btn.is-ok .cl-mark,
.cl-btn.is-ok .cl-spin {
  display: none;
}

.cl-check {
  display: none;
  /* 16.5, the exact 1.5, deliberately not rounded to 16 or 17: this is vector
   * art, so a fractional box costs nothing, and the box scale is what sets the
   * painted stroke weight (see below) — rounding the box would quietly take the
   * checkmark's stroke off the 1.5 ratio the rest of the button follows. */
  width: 16.5px;
  height: 16.5px;
  stroke: currentColor;
  /* NOT scaled with the box. content.js builds this svg with viewBox="0 0 12 12",
   * so stroke-width is in viewBox user units and the viewBox->box scale already
   * multiplied the painted stroke by 1.5 when the width above went 11px ->
   * 16.5px. Scaling this number too would compound to 2.25x. */
  stroke-width: 2.2;
  stroke-linecap: round;
  stroke-linejoin: round;
  fill: none;
  pointer-events: none;
}

.cl-btn.is-ok .cl-check {
  display: block;
}

/* non-default ref badge */
.cl-badge {
  display: none;
  position: absolute;
  top: -3px;
  right: -3px;
  width: 10.5px;
  height: 10.5px;
  border-radius: 50%;
  background: var(--cl-warn);
  box-shadow: 0 0 0 2.25px var(--cl-surface);
  pointer-events: none;
}

.cl-btn.has-badge .cl-badge {
  display: block;
}

/* ---------- can't-resolve state ---------- */

/* The daemon answered REPO_NOT_LOCAL / FILE_NOT_LOCAL: the link IS a file in a
 * repo we understand, we just have no local copy of it. Shown instead of the
 * usual silent skip so the user learns *why* nothing happens — but it must never
 * compete with a live button, hence dimmed, smaller and inert. The desaturation
 * is done through color rather than a filter: .cl-mark is fill:currentColor, so
 * dimming the colour greys the Neovim mark for free, and a filter here would drag
 * the red cross below down with it. */
.cl-btn.is-missing {
  color: var(--cl-text-dim);
  background: var(--cl-surface-hi);
  border-color: var(--cl-border);
  box-shadow: none;
  cursor: default;
}

.cl-btn.is-missing .cl-mark {
  opacity: 0.45;
}

/* beats the #panel.is-open .cl-btn scale(1) rule on specificity */
#panel.is-open .cl-btn.is-missing {
  transform: scale(0.8);
}

.cl-btn.is-missing:hover {
  background: var(--cl-surface-hi);
}

/* Mirrors .cl-badge — same 10.5px circle, same 2.25px surface ring — in the
 * opposite corner, so the two can never be confused for one another. */
.cl-xbadge {
  display: none;
  position: absolute;
  bottom: -3px;
  right: -3px;
  box-sizing: border-box;
  width: 10.5px;
  height: 10.5px;
  border-radius: 50%;
  background: var(--cl-miss);
  box-shadow: 0 0 0 2.25px var(--cl-surface);
  color: #ffffff;
  font-size: 8px;
  font-weight: 700;
  line-height: 10.5px;
  text-align: center;
  pointer-events: none;
}

.cl-btn.is-missing .cl-xbadge {
  display: block;
}

/* ---------- tooltip ---------- */

.cl-tip {
  display: none;
  position: absolute;
  /* clears the button: its 33px plus 4px of air between button and tooltip.
   * Derived, not chosen — it must track .cl-btn. */
  top: 37px;
  left: 0;
  box-sizing: border-box;
  max-width: 272px;
  width: max-content;
  padding: 6px 8px;
  border: 1px solid var(--cl-border);
  border-radius: 6px;
  background: var(--cl-surface);
  color: var(--cl-text);
  box-shadow: var(--cl-shadow);
  font-size: 11px;
  font-weight: 400;
  line-height: 1.4;
  white-space: normal;
  pointer-events: none;
}

.cl-tip.is-on {
  display: block;
}

.cl-tip.is-right {
  left: auto;
  right: 0;
}

/* Flipped by content.js. The button normally sits ABOVE the hovered link, so
 * this is the common case, not the edge case: a tooltip hanging below the button
 * would land straight on the line of code the user is reading. */
.cl-tip.is-above {
  top: auto;
  bottom: 37px;
}

.cl-tip-cmd {
  display: block;
  margin-top: 5px;
  padding: 4px 5px;
  border-radius: 4px;
  background: var(--cl-surface-hi);
  color: var(--cl-text-dim);
  font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, monospace;
  font-size: 10px;
  line-height: 1.35;
  word-break: break-all;
  cursor: copy;
  pointer-events: auto;
}

.cl-tip-cmd:hover {
  color: var(--cl-text);
}

.cl-tip-hint {
  display: block;
  margin-top: 4px;
  color: var(--cl-text-dim);
  font-size: 10px;
}

/* ---------- picker ---------- */

.cl-picker {
  display: none;
  box-sizing: border-box;
  min-width: 216px;
  max-width: 380px;
  padding: 4px;
  border: 1px solid var(--cl-border);
  border-radius: 8px;
  background: var(--cl-surface);
  color: var(--cl-text);
  box-shadow: var(--cl-shadow);
  pointer-events: auto;
  outline: none;
}

.cl-picker.is-on {
  display: block;
}

.cl-picker-head {
  display: flex;
  align-items: center;
  gap: 5px;
  padding: 3px 6px 5px;
  color: var(--cl-text-dim);
  font-size: 10px;
  font-weight: 600;
  letter-spacing: 0.03em;
  text-transform: uppercase;
}

.cl-picker-head .cl-mark {
  width: 8px;
  height: 10px;
  color: var(--cl-accent);
  fill: currentColor;
  flex: none;
}

.cl-picker-list {
  max-height: 244px;
  overflow-y: auto;
  overscroll-behavior: contain;
  scrollbar-width: thin;
}

.cl-row {
  display: block;
  box-sizing: border-box;
  width: 100%;
  padding: 4px 6px;
  border: 0;
  border-radius: 5px;
  background: transparent;
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
}

.cl-row.is-sel {
  background: var(--cl-sel);
}

.cl-row-top {
  display: flex;
  align-items: baseline;
  gap: 6px;
}

.cl-row-label {
  flex: 1 1 auto;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 12px;
  font-weight: 600;
  color: var(--cl-text);
}

.cl-row-tag {
  flex: none;
  padding: 0 4px;
  border-radius: 3px;
  background: var(--cl-sel);
  color: var(--cl-accent);
  font-size: 9px;
  font-weight: 700;
  letter-spacing: 0.04em;
  text-transform: uppercase;
}

.cl-row-sub {
  display: block;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--cl-text-dim);
  font-size: 10px;
  font-weight: 400;
}

.cl-picker-foot {
  padding: 4px 6px 2px;
  color: var(--cl-text-dim);
  font-size: 10px;
  font-weight: 400;
}

.cl-picker-foot.is-warn {
  color: var(--cl-warn);
}

/* ---------- dark theme ---------- */

@media (prefers-color-scheme: dark) {
  #panel {
    color: #e8eaed;
    --cl-surface: #1e2126;
    --cl-surface-hi: #2a2e35;
    --cl-border: rgba(255, 255, 255, 0.18);
    --cl-text: #e8eaed;
    --cl-text-dim: #9aa3ad;
    --cl-accent: #7ec96a;
    --cl-warn: #e0a458;
    --cl-warn-soft: rgba(224, 164, 88, 0.14);
    --cl-miss: #e5645c;
    --cl-shadow: 0 2px 12px rgba(0, 0, 0, 0.55), 0 0 0 0.5px rgba(255, 255, 255, 0.05);
    --cl-sel: rgba(126, 201, 106, 0.18);
  }
}

@media (prefers-reduced-motion: reduce) {
  .cl-btn {
    transition: none;
  }

  .cl-spin {
    animation-duration: 2s;
  }
}
`;
  /* codelink:inline:css:end */

  /* codelink:inline:svg:begin */
  var MARK_SVG = String.raw`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 601 736" width="601" height="736" role="img" aria-label="Neovim"> <title>Neovim</title> <g fill="currentColor" fill-rule="evenodd" transform="translate(1 4)"> <path d="M0,155.437209 L26.1659448,129 L155,320.997949 L154.999997,727 L0,572.201847 L0,155.437209 Z"/> <path d="M443.060403,156.91391 L599.999996,-1 L600,403.297284 L468.704944,601 C468.704944,600.999977 442,571.971417 442,571.971417 L443.060403,156.91391 Z" transform="translate(521 300) scale(-1 1) translate(-521 -300)"/> <path d="M154.986294,0 L558,615.189696 L445.224605,728 L42,114.172017 L154.986294,0 Z"/> </g> </svg>`;
  /* codelink:inline:svg:end */

  // ------------------------------------------------------------ host matching

  // hosts.gen.js is generated by the daemon alongside manifest.json. If it is
  // somehow absent, fall back to the document's own host: the manifest matched
  // this page, so by definition we are on a supported host.
  var HOSTS = Array.isArray(self.CODELINK_HOSTS) && self.CODELINK_HOSTS.length
    ? self.CODELINK_HOSTS.slice()
    : [location.hostname];

  function hostMatches(hostname) {
    var h = String(hostname || '').toLowerCase();
    for (var i = 0; i < HOSTS.length; i++) {
      var g = String(HOSTS[i] || '').toLowerCase();
      if (!g) continue;
      if (g === h) return true;
      if (g.charCodeAt(0) === 42 /* * */ && g.charCodeAt(1) === 46 /* . */) {
        var base = g.slice(2);
        if (h === base || (h.length > base.length && h.slice(-(base.length + 1)) === '.' + base)) {
          return true;
        }
      }
    }
    return false;
  }

  /*
   * Coarse triage. This is intentionally not a URL grammar: the daemon's
   * parser is RE2 and it settles file-vs-directory by stat-ing the checkout.
   * We over-trigger a little and let /resolve say no — that is the correct
   * division of labour. The only real filter is "deep enough to plausibly be a
   * path inside a repo", which keeps us off short SPA routes like /review/123.
   */
  function triage(href) {
    var u;
    try {
      u = new URL(href, location.href);
    } catch (e) {
      return false;
    }
    if (u.protocol !== 'https:' && u.protocol !== 'http:') return false;
    if (!hostMatches(u.hostname)) return false;

    var segments = u.pathname.split('/');
    var count = 0;
    for (var i = 0; i < segments.length; i++) {
      if (segments[i]) count++;
    }
    return count >= MIN_SEGMENTS;
  }

  // ------------------------------------------------------------------ helpers

  function basename(p) {
    var s = String(p || '');
    while (s.length > 1 && s.charAt(s.length - 1) === '/') s = s.slice(0, -1);
    var i = s.lastIndexOf('/');
    return i < 0 ? s : s.slice(i + 1);
  }

  // Dim secondary line: the last few segments of a path, elided at the front.
  function pathTail(p, keep) {
    var s = String(p || '');
    if (!s) return '';
    var parts = s.split('/').filter(Boolean);
    var n = keep || 3;
    if (parts.length <= n) return s;
    return '…/' + parts.slice(-n).join('/');
  }

  function firstRect(el) {
    var rects = el.getClientRects();
    var r = rects && rects.length ? rects[0] : el.getBoundingClientRect();
    if (!r || (r.width === 0 && r.height === 0)) return null;
    return r;
  }

  function clear(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  function normalize(d) {
    var o = d && typeof d === 'object' ? d : {};
    return {
      parsed: o.parsed && typeof o.parsed === 'object' ? o.parsed : {},
      warnings: Array.isArray(o.warnings) ? o.warnings : [],
      openInstances: Array.isArray(o.openInstances) ? o.openInstances : [],
      // Already sorted most-recent-first by the daemon. Never re-sort.
      rootCandidates: Array.isArray(o.rootCandidates) ? o.rootCandidates : [],
    };
  }

  function itemPath(o) {
    if (!o || typeof o !== 'object') return '';
    return o.cwd || o.root || o.path || o.dir || '';
  }

  function itemLabel(o) {
    if (o && typeof o.label === 'string' && o.label) return o.label;
    return basename(itemPath(o)) || '(unnamed)';
  }

  /*
   * What the daemon wants in `target`:
   *   mode "existing" -> the instance's `id`, a STRING (a pid, e.g. "61981")
   *   mode "new"      -> the root candidate's absolute `root` path
   * Deliberately not itemPath(): that one prefers `cwd` for display, but an
   * instance's cwd is a subdirectory of the checkout and is not a valid target.
   */
  function itemTarget(o, mode) {
    if (!o || typeof o !== 'object') return '';
    if (mode === 'new') return String(o.root || o.path || o.dir || o.cwd || '');
    return o.id == null ? '' : String(o.id);
  }

  // ------------------------------------------------- service-worker messaging

  function send(msg) {
    return new Promise(function (resolve) {
      var settled = false;
      function done(reply) {
        if (settled) return;
        settled = true;
        resolve(reply);
      }
      try {
        chrome.runtime.sendMessage(msg, function (reply) {
          var err = chrome.runtime.lastError;
          if (err) {
            // "Extension context invalidated" after a reload: our world is a
            // ghost. Stay silent rather than blaming the daemon.
            var m = String(err.message || '');
            var gone = m.indexOf('context invalidated') >= 0 || m.indexOf('Receiving end') >= 0;
            return done({ ok: false, kind: gone ? 'ext-gone' : 'daemon-down', error: m });
          }
          done(reply || { ok: false, kind: 'api', error: 'empty reply from service worker' });
        });
      } catch (e) {
        done({ ok: false, kind: 'ext-gone', error: String((e && e.message) || e) });
      }
    });
  }

  // ---------------------------------------------------------------- shadow UI

  var host = document.createElement('div');
  // !important throughout: per spec, page rules that match the host element beat
  // :host rules inside the shadow tree, so a stray `div { display:none }` on the
  // site could otherwise erase the whole overlay. Zero-sized so it can never
  // shift page layout; #panel is position:fixed and escapes the box anyway.
  host.style.cssText =
    'position:fixed!important;top:0!important;left:0!important;' +
    'width:0!important;height:0!important;margin:0!important;padding:0!important;border:0!important;' +
    'display:block!important;visibility:visible!important;opacity:1!important;' +
    'z-index:2147483647!important;pointer-events:none!important';
  // documentElement, not body: the host page is a single-page app and re-renders
  // <body>, which would take the overlay with it. Attach above that boundary.
  var root = host.attachShadow({ mode: 'closed' });
  document.documentElement.appendChild(host);

  var style = document.createElement('style');
  style.textContent = CSS;
  root.appendChild(style);

  var panel = document.createElement('div');
  panel.id = 'panel';
  root.appendChild(panel);

  function markNode(cls) {
    var parsed = null;
    try {
      var doc = new DOMParser().parseFromString(MARK_SVG, 'image/svg+xml');
      if (doc.documentElement && doc.documentElement.nodeName.toLowerCase() === 'svg') {
        parsed = document.importNode(doc.documentElement, true);
      }
    } catch (e) {
      parsed = null;
    }
    if (!parsed) {
      // Never ship a broken button: fall back to a plain dot.
      parsed = document.createElementNS(SVG_NS, 'svg');
      parsed.setAttribute('viewBox', '0 0 10 10');
      var dot = document.createElementNS(SVG_NS, 'circle');
      dot.setAttribute('cx', '5');
      dot.setAttribute('cy', '5');
      dot.setAttribute('r', '4');
      dot.setAttribute('fill', 'currentColor');
      parsed.appendChild(dot);
    }
    parsed.removeAttribute('width');
    parsed.removeAttribute('height');
    parsed.removeAttribute('role');
    parsed.removeAttribute('aria-label');
    parsed.setAttribute('class', cls);
    parsed.setAttribute('aria-hidden', 'true');
    parsed.setAttribute('focusable', 'false');
    return parsed;
  }

  var btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'cl-btn';
  btn.setAttribute('aria-label', 'Open in Neovim');
  btn.appendChild(markNode('cl-mark'));

  var check = document.createElementNS(SVG_NS, 'svg');
  check.setAttribute('viewBox', '0 0 12 12');
  check.setAttribute('class', 'cl-check');
  check.setAttribute('aria-hidden', 'true');
  var checkPath = document.createElementNS(SVG_NS, 'path');
  checkPath.setAttribute('d', 'M1.7 6.3 L4.5 9.1 L10.3 3.1');
  check.appendChild(checkPath);
  btn.appendChild(check);

  var spin = document.createElement('span');
  spin.className = 'cl-spin';
  btn.appendChild(spin);

  var badge = document.createElement('span');
  badge.className = 'cl-badge';
  btn.appendChild(badge);

  // The can't-resolve cross. Bottom-right, mirroring the non-default-ref dot in
  // the opposite corner; only ever visible in the 'missing' state.
  var xbadge = document.createElement('span');
  xbadge.className = 'cl-xbadge';
  xbadge.textContent = '✕';
  xbadge.setAttribute('aria-hidden', 'true');
  btn.appendChild(xbadge);

  panel.appendChild(btn);

  var tip = document.createElement('div');
  tip.className = 'cl-tip';
  panel.appendChild(tip);

  var picker = document.createElement('div');
  picker.className = 'cl-picker';
  picker.setAttribute('tabindex', '-1');
  picker.setAttribute('role', 'listbox');
  var pickerHead = document.createElement('div');
  pickerHead.className = 'cl-picker-head';
  var pickerList = document.createElement('div');
  pickerList.className = 'cl-picker-list';
  var pickerFoot = document.createElement('div');
  pickerFoot.className = 'cl-picker-foot';
  picker.appendChild(pickerHead);
  picker.appendChild(pickerList);
  picker.appendChild(pickerFoot);
  panel.appendChild(picker);

  // ------------------------------------------------------------------- state

  var anchorEl = null; // the <a> the panel currently belongs to
  var anchorUrl = '';
  var entry = null; // {kind:'ok', data} | {kind:'missing', …} for anchorUrl
  var uiMode = 'hidden'; // hidden | loading | ready | missing | down | error | ok | picker
  var pinned = false; // picker open: ignore hover-out entirely
  var busy = false; // an /open round trip is in flight: survive a dying anchor
  var lastRect = null; // last good anchor rect, for repositioning past its death
  var pointerX = null; // clientX of the trusted mouseover that started this show
  var overPanel = false; // pointer is inside the overlay right now
  var lastRepo = null; // {key, label} of the most recent verdict that named a repo

  var hideTimer = 0; // the single shared hide timer — see startHide/cancelHide
  var resolveTimer = 0; // the RESOLVE_DELAY dwell timer — see scheduleShow
  var spinTimer = 0; // the SPIN_DELAY timer — see renderLoading/beginResolve
  var okTimer = 0;

  /*
   * Settings, mirrored from chrome.storage.local under the single key
   * "settings". The popup is the only writer; this is a read-only replica kept
   * in sync through chrome.storage.onChanged, so a toggle takes effect on every
   * open tab without a reload.
   *
   *   { paused: false, warnBadges: true, warnOverrides: {}, debug: false }
   *
   * Absent keys fall back to these defaults rather than to undefined, because
   * the very first hover in a profile that has never opened the popup must
   * behave exactly like the documented default.
   */
  var settings = { paused: false, warnBadges: true, warnOverrides: {}, debug: false };

  function applySettings(raw) {
    var o = raw && typeof raw === 'object' ? raw : {};
    settings = {
      paused: o.paused === true,
      warnBadges: o.warnBadges !== false,
      warnOverrides: o.warnOverrides && typeof o.warnOverrides === 'object' ? o.warnOverrides : {},
      debug: o.debug === true,
    };
  }

  // try/catch, not a feature test: with the storage permission missing (an old
  // profile whose card predates it) every documented default above still stands,
  // which is the same behaviour as a popup that was never opened.
  try {
    chrome.storage.local.get('settings', function (got) {
      applySettings(got && got.settings);
    });
    chrome.storage.onChanged.addListener(function (changes, area) {
      if (area !== 'local' || !changes.settings) return;
      applySettings(changes.settings.newValue);
      // Pausing must also clear anything already painted — except mid-/open,
      // where tearing down would orphan the in-flight request's UI.
      if (settings.paused && uiMode !== 'hidden' && !pinned && !busy) hideNow();
    });
  } catch (e) {
    /* no chrome.storage here: defaults stand */
  }

  /*
   * Per-repo override key: "<host>/<owner>/<repo>". The host comes from the URL
   * that produced the verdict (lowercased, as hostnames are case-insensitive);
   * owner and repo are echoed verbatim from the daemon, which is the only party
   * that knows how a provider spells them. The popup never builds this key: it
   * receives it prebuilt through the repo-info message, so the two sides cannot
   * drift and nothing outside this function re-parses a URL.
   */
  function overrideKey(url, owner, repo) {
    var h;
    try {
      h = new URL(url, location.href).hostname.toLowerCase();
    } catch (e) {
      h = String(location.hostname || '').toLowerCase();
    }
    // Lowercased end to end: the daemon matches repos case-insensitively, so a
    // link spelt /Acme/Widget and a tab at /acme/widget must share one key.
    return h + '/' + String(owner || '').toLowerCase() + '/' + String(repo || '').toLowerCase();
  }

  // Effective badge visibility for one link: the per-repo override if there is
  // one, the global switch otherwise.
  function badgeVisible(url, owner, repo) {
    var over = settings.warnOverrides[overrideKey(url, owner, repo)];
    if (typeof over === 'boolean') return over;
    return settings.warnBadges;
  }

  /*
   * Debug channel. The overlay lives in a CLOSED shadow root, so the page
   * console cannot inspect it at all — logging is the only way to see this
   * state machine from outside, and that is exactly what was needed to find the
   * detached-anchor bug (see reposition()). Kept deliberately.
   *
   *   enable:  the popup's Debug toggle (settings.debug), or, with no popup and
   *            no extension storage at hand, localStorage.CODELINK_DEBUG = '1'
   *   disable: untick it, or delete localStorage.CODELINK_DEBUG
   *
   * The localStorage flag stays because it is the only channel that works from a
   * page console, i.e. while debugging this file itself.
   */
  function dbg() {
    var on = settings.debug;
    if (!on) {
      try {
        on = localStorage.getItem('CODELINK_DEBUG') === '1';
      } catch (e) {
        on = false;
      }
    }
    if (!on) return;
    var a = ['[codelink]'].concat([].slice.call(arguments));
    console.log.apply(console, a);
  }
  var reqSeq = 0; // guards against out-of-order async replies
  var rafPending = false;

  // url -> {v: {kind:'ok', data} | {kind:'missing', code, owner, repo, repoPath}
  //             | {kind:'skip'}, exp}
  var cache = new Map();

  /*
   * Bounded LRU. A single-page-app session is long-lived and triage over-triggers, so
   * an unbounded Map would accrue one entry per hovered URL for the tab's whole
   * life. Map iterates in insertion order, so deleting-then-setting moves a key
   * to the back and the first key is always the least recently used.
   *
   * Entries also EXPIRE, because a resolve is mostly a snapshot of mutable machine
   * state: which Neovim instances are running, which checkouts are mounted. Held
   * forever, an editor started after the first hover never takes effect — the link
   * goes on offering a new instance because this tab still believes none exists,
   * and cacheGet's recency refresh keeps the links you use most the most stale.
   *
   * A verdict about the URL's *shape* (NO_PROVIDER, 4xx) is not machine state and
   * cannot go stale, so it is stored with exp 0, meaning permanent.
   */
  function cachePut(url, value, ttl) {
    if (cache.has(url)) cache.delete(url);
    cache.set(url, { v: value, exp: ttl ? Date.now() + ttl : 0 });
    while (cache.size > CACHE_MAX) {
      var oldest = cache.keys().next().value;
      if (oldest === undefined) break;
      cache.delete(oldest);
    }
  }

  function cacheGet(url) {
    var hit = cache.get(url);
    if (hit === undefined) return undefined;
    if (hit.exp && Date.now() >= hit.exp) {
      cache.delete(url);
      return undefined;
    }
    // Refresh LRU recency but NOT the deadline: re-hovering a link must not keep
    // a stale instance list alive indefinitely.
    cache.delete(url);
    cache.set(url, hit);
    return hit.v;
  }

  var pickerRows = [];
  var pickerItems = [];
  var pickerKind = 'existing';
  var pickerIndex = 0;

  /*
   * `entry` holds either a resolved payload or a 'missing' verdict, and only the
   * first has a `data`. Everything that opens a file goes through here, so an
   * inert 'missing' entry can never be mistaken for something clickable.
   */
  function okData() {
    return entry && entry.kind === 'ok' ? entry.data : null;
  }

  // ------------------------------------------------------ show / hide timing

  /*
   * The single most important UX detail: the button must survive the mouse
   * travelling from the link to the button. One shared timer, started by a
   * leave from *either* the anchor or the panel and cancelled by an enter on
   * *either*. Because mouseout fires before the following mouseover, the
   * handover reads as: start(120ms) -> cancel, and nothing flickers.
   */
  function cancelHide() {
    if (hideTimer) {
      clearTimeout(hideTimer);
      hideTimer = 0;
    }
  }

  function startHide() {
    if (pinned) return; // picker is modal: only Esc / outside click / select close it
    cancelHide();
    hideTimer = setTimeout(hideNow, HIDE_DELAY);
  }

  function hideNow() {
    cancelHide();
    stopDwell();
    clearSpin();
    if (okTimer) {
      clearTimeout(okTimer);
      okTimer = 0;
    }
    reqSeq++;
    pinned = false;
    // reqSeq++ above already invalidates any in-flight reply.
    busy = false;
    // display:none does not reliably emit mouseleave, so drop the flag here too.
    overPanel = false;
    uiMode = 'hidden';
    anchorEl = null;
    anchorUrl = '';
    pointerX = null;
    entry = null;
    panel.classList.remove('is-open');
    picker.classList.remove('is-on');
    btn.style.display = '';
    btn.className = 'cl-btn';
    hideTip();
  }

  // ------------------------------------------------------------- positioning

  function reposition() {
    if (uiMode === 'hidden' || !anchorEl) return;

    var vw = document.documentElement.clientWidth || window.innerWidth;
    var vh = document.documentElement.clientHeight || window.innerHeight;

    // One "is the anchor still usable?" test, three ways it can fail: the node
    // was removed, it collapsed to nothing, or it scrolled out of view.
    var r = anchorEl.isConnected ? firstRect(anchorEl) : null;
    if (r && (r.bottom < 0 || r.top > vh || r.right < 0 || r.left > vw)) r = null;

    if (!r) {
      /*
       * The picker is modal and self-contained — it already holds its rows and
       * the resolved payload, and it is dismissed by Esc / outside click /
       * selection, never by the anchor.
       *
       * This guard is load-bearing on a SPA. The page re-renders and REPLACES the
       * <a> node underneath us, so by the time the user clicks, the anchor we
       * captured on hover is detached. Without this, openPicker() built the rows,
       * then called showPanel() -> reposition(), which saw the dead anchor and
       * called hideNow() — wiping anchorEl, entry and both visibility classes in
       * the same tick the picker was built. The picker never painted, and the
       * button appeared to vanish on click with nothing replacing it.
       *
       * Keep the last known position instead: an open picker outlives its link.
       *
       * `busy` covers the same hazard on the no-picker path: doOpen() captures a
       * sequence number, then renderBusy() -> showPanel() -> here. Tearing down
       * would bump reqSeq and silently discard the reply, so the file would open
       * in Neovim while the UI vanished without a word.
       */
      if (!pinned && !busy) {
        dbg('reposition: TEARDOWN — anchor dead, uiMode=' + uiMode);
        hideNow();
        return;
      }
      /*
       * Held open. Fall back to the last known anchor rect rather than simply
       * returning: the panel must still be re-clamped against its CURRENT size.
       * The picker is several times larger than the 33px button it replaces, so
       * keeping the button's old transform pushes it off the bottom or right
       * edge, where it cannot be reached or dismissed.
       */
      r = lastRect;
      if (!r) {
        dbg('reposition: anchor dead and no remembered rect — leaving as is');
        return;
      }
      dbg('reposition: anchor dead, re-clamping against remembered rect');
    } else {
      lastRect = r;
    }

    var pw = panel.offsetWidth || BTN;
    var ph = panel.offsetHeight || BTN;

    /*
     * ABOVE the link's line box, horizontally next to the pointer, then clamped
     * into the viewport.
     *
     * Vertically this is the whole point of the placement: the button used to be
     * centred on the link's own line, which meant it covered whatever followed
     * the link on that line — on a code host, usually more code. One BTN plus the
     * same GAP above the line box moves it into the leading between lines
     * instead, and nothing on the hovered line is ever hidden.
     *
     * Horizontally it follows the cursor rather than the link's right edge. An
     * anchor on these sites is regularly a whole table cell or a wrapped path, so
     * its right edge can be hundreds of px from where the hand actually is;
     * pointerX is the clientX of the trusted mouseover that started this show.
     * POINTER_DX keeps the button's left edge clear of the arrow's hotspot so it
     * never paints under the cursor. With no pointerX (a reposition that
     * predates any hover) fall back to the link's left edge.
     *
     * BTN rather than ph on purpose: with the picker open, ph is the PICKER's
     * height, and subtracting that would fling it far above the link. The picker
     * is meant to appear where the button was; the clamp is what keeps it on
     * screen.
     */
    var px = typeof pointerX === 'number' ? pointerX : r.left;
    var left = px + POINTER_DX;
    // Flush with the line box, no vertical gap: every px of air here is a px
    // the pointer has to travel, and the button never overlaps the line anyway.
    var top = r.top - BTN;
    // No room above — a link on the first line of the viewport. Mirror below,
    // which is the only other place that does not cover the line.
    var above = true;
    if (top < GAP) {
      top = r.bottom;
      above = false;
    }
    left = Math.max(GAP, Math.min(left, vw - pw - GAP));
    top = Math.max(GAP, Math.min(top, vh - ph - GAP));

    panel.style.transform = 'translate3d(' + Math.round(left) + 'px,' + Math.round(top) + 'px,0)';

    /*
     * Which side the tooltip takes. It defaults to hanging below the button, and
     * that was harmless while the button sat beside the link — now the button is
     * above the link, so "below the button" IS the link's own line. Flip it above
     * whenever the button is above (unless it would not fit up there), and leave
     * it below when the button had to flip below the link, for the same reason.
     */
    tip.classList.toggle('is-right', left + TIP_W > vw);
    tip.classList.toggle('is-above', above ? top >= TIP_H : top + ph + TIP_H > vh);
  }

  function onViewportChange() {
    if (rafPending) return;
    rafPending = true;
    requestAnimationFrame(function () {
      rafPending = false;
      reposition();
    });
  }

  // ---------------------------------------------------------------- tooltip

  function hideTip() {
    tip.classList.remove('is-on');
    clear(tip);
  }

  function showTip(text, cmd, hint) {
    clear(tip);
    var line = document.createElement('span');
    line.textContent = text;
    tip.appendChild(line);
    if (cmd) {
      var code = document.createElement('span');
      code.className = 'cl-tip-cmd';
      code.textContent = cmd;
      code.addEventListener('click', function (e) {
        e.preventDefault();
        e.stopPropagation();
        copyText(cmd).then(function (ok) {
          code.textContent = ok ? 'copied to clipboard' : cmd;
          if (ok) {
            setTimeout(function () {
              code.textContent = cmd;
            }, 1200);
          }
        });
      });
      tip.appendChild(code);
    }
    if (hint) {
      var h = document.createElement('span');
      h.className = 'cl-tip-hint';
      h.textContent = hint;
      tip.appendChild(h);
    }
    tip.classList.add('is-on');
    reposition();
  }

  function copyText(text) {
    return new Promise(function (resolve) {
      var viaApi = null;
      try {
        viaApi = navigator.clipboard && navigator.clipboard.writeText(text);
      } catch (e) {
        viaApi = null;
      }
      if (viaApi && typeof viaApi.then === 'function') {
        viaApi.then(
          function () {
            resolve(true);
          },
          function () {
            resolve(fallbackCopy(text));
          }
        );
        return;
      }
      resolve(fallbackCopy(text));
    });
  }

  function fallbackCopy(text) {
    try {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('style', 'position:fixed;top:-2000px;left:-2000px;opacity:0');
      document.documentElement.appendChild(ta);
      ta.select();
      var ok = document.execCommand('copy');
      ta.remove();
      return Boolean(ok);
    } catch (e) {
      return false;
    }
  }

  // ------------------------------------------------------------- button views

  function showPanel() {
    panel.classList.add('is-open');
    reposition();
  }

  /*
   * The pre-verdict state paints the PLAIN button, not the spinner: the daemon
   * answers in single-digit milliseconds normally, and a spinner that flashes
   * on every hover reads as latency codelink does not have. beginResolve arms
   * the spinner separately, SPIN_DELAY after the request actually went out, so
   * it only ever shows against a daemon that is genuinely slow.
   */
  function renderLoading() {
    clearSpin();
    uiMode = 'loading';
    btn.style.display = '';
    picker.classList.remove('is-on');
    hideTip();
    btn.className = 'cl-btn';
    btn.setAttribute('aria-label', 'codelink: resolving…');
    showPanel();
  }

  function clearSpin() {
    if (spinTimer) {
      clearTimeout(spinTimer);
      spinTimer = 0;
    }
  }

  function renderReady() {
    var d = okData();
    if (!d) return;
    uiMode = 'ready';
    btn.style.display = '';
    picker.classList.remove('is-on');
    hideTip();
    var odd = d.parsed.refIsDefault === false;
    btn.className = 'cl-btn' + (odd ? ' has-badge' : '');
    btn.setAttribute('aria-label', 'Open in Neovim');
    showPanel();
  }

  /*
   * The daemon understood the link but has nothing local behind it. Unlike the
   * two-empty-lists verdict — which is the ordinary outcome for most links on the
   * page and must paint nothing — this one names a repo, so it is worth telling
   * the user about once: a dimmed, smaller, inert button with a red cross. It
   * never opens anything; the click handler only re-shows the tooltip.
   */
  function renderMissing() {
    if (!entry || entry.kind !== 'missing') return;
    uiMode = 'missing';
    btn.style.display = '';
    picker.classList.remove('is-on');
    hideTip();
    btn.className = 'cl-btn is-missing';
    btn.setAttribute(
      'aria-label',
      entry.code === 'FILE_NOT_LOCAL'
        ? 'codelink: not in the local ' + entry.repo + ' checkout'
        : 'codelink: ' + entry.repo + " isn't cloned locally"
    );
    showPanel();
  }

  function renderDown() {
    uiMode = 'down';
    btn.style.display = '';
    picker.classList.remove('is-on');
    btn.className = 'cl-btn is-down';
    btn.setAttribute('aria-label', "codelink daemon isn't running");
    showPanel();
  }

  function renderError(reply) {
    uiMode = 'error';
    btn.style.display = '';
    picker.classList.remove('is-on');
    btn.className = 'cl-btn is-down';
    showPanel();
    // A rotated token is actionable and already reads as a full instruction —
    // show it verbatim, without the "codelink:" prefix or a raw HTTP 403 line.
    if (reply && reply.kind === 'auth') {
      btn.setAttribute('aria-label', 'codelink: token rejected');
      showTip(reply.error || 'codelink rejected this extension’s token');
      return;
    }
    btn.setAttribute('aria-label', 'codelink error');
    showTip(
      'codelink: ' + ((reply && reply.error) || 'request failed'),
      null,
      reply && reply.status ? 'HTTP ' + reply.status : null
    );
  }

  function showOk() {
    uiMode = 'ok';
    picker.classList.remove('is-on');
    btn.style.display = '';
    btn.className = 'cl-btn is-ok';
    hideTip();
    showPanel();
    if (okTimer) clearTimeout(okTimer);
    okTimer = setTimeout(hideNow, OK_DELAY);
  }

  function hoverTipForState() {
    if (uiMode === 'down') {
      showTip("codelink daemon isn't running", KICKSTART, 'click the command to copy');
      return;
    }
    if (uiMode === 'missing' && entry && entry.kind === 'missing') {
      if (entry.code === 'FILE_NOT_LOCAL') {
        showTip(
          (entry.repoPath || 'that path') + " isn't in the local " + entry.repo + ' checkout',
          null,
          'pull, or switch to the right branch'
        );
      } else {
        showTip(
          "project '" + entry.repo + "' isn't cloned locally",
          null,
          'clone it, add a repoAliases entry, or add its folder to roots — see providers.schema.md'
        );
      }
      return;
    }
    var d = okData();
    if (uiMode !== 'ready' || !d) return;
    var warn = d.parsed.refIsDefault === false ? d.warnings[0] || 'not on the default branch' : null;
    var n = d.openInstances.length;
    var main =
      n === 1
        ? 'Open in ' + itemLabel(d.openInstances[0])
        : n > 1
          ? 'Open in Neovim — ' + n + ' instances'
          : 'Open in a new Neovim';
    // With nothing to reuse, a plain click already spawns a new instance, so
    // advertising shift-click there would name the same gesture twice.
    var hint = n === 0 ? 'no running Neovim has this project open' : 'shift-click: new instance';
    showTip(warn || main, null, warn ? main + ' · ' + hint : hint);
  }

  // ------------------------------------------------------------------ picker

  /*
   * how:
   *   'keep'    — a row was chosen; the caller renders whatever comes next
   *   'restore' — Esc / Tab; put the button back where the picker was
   *   'dismiss' — click outside or window blur; the user is done with us
   */
  function closePicker(how) {
    pinned = false;
    picker.classList.remove('is-on');
    clear(pickerList);
    pickerRows = [];
    pickerItems = [];
    btn.style.display = '';
    if (how === 'dismiss' || (how === 'restore' && !okData())) {
      hideNow();
      return;
    }
    if (how === 'restore') {
      renderReady();
      // Any leave that happened while the picker was pinned was swallowed by
      // startHide, so reinstate it — unless the pointer is still on the overlay,
      // in which case the button would vanish from under the cursor.
      if (!overPanel) startHide();
    }
  }

  function openPicker(kind, note) {
    var d = okData();
    dbg('openPicker:enter', kind, 'hasEntry=', Boolean(d));
    if (!d) return;
    var items = kind === 'new' ? d.rootCandidates : d.openInstances;
    if (!items.length) {
      dbg('openPicker:abort — no items for kind', kind);
      return;
    }

    pickerKind = kind;
    pickerItems = items;
    pickerRows = [];
    pickerIndex = 0;
    uiMode = 'picker';
    pinned = true;
    cancelHide();
    hideTip();

    clear(pickerHead);
    pickerHead.appendChild(markNode('cl-mark'));
    var headText = document.createElement('span');
    headText.textContent = kind === 'new' ? 'New Neovim instance' : 'Open in';
    pickerHead.appendChild(headText);

    clear(pickerList);
    for (var i = 0; i < items.length; i++) {
      pickerList.appendChild(buildRow(items[i], i, kind));
    }

    var warn = d.parsed.refIsDefault === false ? d.warnings[0] || 'not on the default branch' : null;
    pickerFoot.textContent = note || warn || '↑↓ move · ⏎ open · esc cancel';
    pickerFoot.classList.toggle('is-warn', Boolean(note || warn));

    btn.style.display = 'none';
    picker.classList.add('is-on');
    showPanel();
    setSelection(0);
    try {
      picker.focus({ preventScroll: true });
    } catch (e) {
      /* focus is a nicety; the document-level key handler is the real path */
    }

    var pr = picker.getBoundingClientRect();
    var nr = panel.getBoundingClientRect();
    dbg('openPicker:done', {
      rows: items.length,
      pickerClass: picker.className,
      panelClass: panel.className,
      hostConnected: host.isConnected,
      picker: Math.round(pr.width) + 'x' + Math.round(pr.height) + ' @' + Math.round(pr.left) + ',' + Math.round(pr.top),
      panel: Math.round(nr.width) + 'x' + Math.round(nr.height) + ' @' + Math.round(nr.left) + ',' + Math.round(nr.top),
      pickerDisplay: getComputedStyle(picker).display,
      panelDisplay: getComputedStyle(panel).display,
    });
  }

  function buildRow(item, index, kind) {
    var row = document.createElement('div');
    row.className = 'cl-row';
    row.setAttribute('role', 'option');

    var top = document.createElement('div');
    top.className = 'cl-row-top';

    var label = document.createElement('span');
    label.className = 'cl-row-label';
    label.textContent = itemLabel(item);
    top.appendChild(label);

    if (kind === 'new' && item && item.hasOpenInstance) {
      var tag = document.createElement('span');
      tag.className = 'cl-row-tag';
      tag.textContent = 'open';
      top.appendChild(tag);
    }
    row.appendChild(top);

    var sub = document.createElement('span');
    sub.className = 'cl-row-sub';
    sub.textContent = pathTail(itemPath(item), 3);
    row.appendChild(sub);

    row.addEventListener('mouseenter', function () {
      setSelection(index);
    });
    row.addEventListener('click', function (e) {
      e.preventDefault();
      e.stopPropagation();
      choose(index);
    });

    pickerRows.push(row);
    return row;
  }

  function setSelection(i) {
    if (!pickerRows.length) return;
    pickerIndex = ((i % pickerRows.length) + pickerRows.length) % pickerRows.length;
    for (var k = 0; k < pickerRows.length; k++) {
      var on = k === pickerIndex;
      pickerRows[k].classList.toggle('is-sel', on);
      pickerRows[k].setAttribute('aria-selected', on ? 'true' : 'false');
      if (on && pickerRows[k].scrollIntoView) {
        pickerRows[k].scrollIntoView({ block: 'nearest' });
      }
    }
  }

  function choose(i) {
    var item = pickerItems[i];
    if (!item) return;
    var payload = buildOpenPayload(pickerKind === 'new' ? 'new' : 'existing', item);
    closePicker('keep');
    doOpen(payload, true);
  }

  /*
   * The daemon's /open body is FLAT and is the canonical contract:
   *   {"mode":"existing","target":"<instance id>","repoPath":"…","line":12,
   *    "endLine":20,"focus":true}
   *   {"mode":"new","target":"<root absolute path>","repoPath":"…","line":12,
   *    "endLine":null}
   * repoPath/line/endLine live inside the /resolve response's `parsed` object
   * and must be lifted out explicitly — leaving them nested silently loses the
   * line jump, and a missing repoPath is rejected with BAD_REQUEST.
   */
  function buildOpenPayload(mode, item) {
    var d = okData();
    var parsed = d ? d.parsed : {};
    return {
      mode: mode,
      target: itemTarget(item, mode),
      repoPath: typeof parsed.repoPath === 'string' ? parsed.repoPath : '',
      line: typeof parsed.line === 'number' ? parsed.line : null,
      endLine: typeof parsed.endLine === 'number' ? parsed.endLine : null,
      focus: true,
    };
  }

  // ------------------------------------------------------------------ actions

  function onButtonClick(e) {
    e.preventDefault();
    e.stopPropagation();

    var d = okData();
    dbg('click', {
      type: e.type,
      shift: e.shiftKey,
      uiMode: uiMode,
      hasEntry: Boolean(d),
      instances: d ? d.openInstances.length : -1,
      roots: d ? d.rootCandidates.length : -1,
    });

    // Inert by design: there is nothing local to open. Re-show the explanation
    // for anyone who clicked it expecting something to happen.
    if (uiMode === 'missing') {
      hoverTipForState();
      return;
    }
    if (uiMode === 'down') {
      copyText(KICKSTART).then(function (ok) {
        showTip(
          ok ? 'command copied — run it in a terminal' : "codelink daemon isn't running",
          KICKSTART,
          null
        );
      });
      return;
    }
    if (uiMode === 'error') {
      // A retry is the only useful action here — and an explicit click IS the
      // dwell, so the daemon is asked immediately rather than after the delay.
      if (anchorEl && anchorUrl) {
        cache.delete(anchorUrl);
        renderLoading();
        beginResolve(anchorEl, anchorUrl);
      }
      return;
    }
    if (uiMode !== 'ready' || !d) return;

    if (e.shiftKey) {
      if (d.rootCandidates.length) openPicker('new');
      else showTip('no local checkout contains this path');
      return;
    }
    if (d.openInstances.length === 1) {
      dbg('branch: single instance -> doOpen');
      doOpen(buildOpenPayload('existing', d.openInstances[0]), true);
      return;
    }
    if (d.openInstances.length > 1) {
      dbg('branch: multiple instances -> picker');
      openPicker('existing');
      return;
    }
    dbg('branch: no instances -> root picker, roots=' + d.rootCandidates.length);
    // Nothing is open yet — a new instance is the only sensible action.
    if (d.rootCandidates.length) openPicker('new');
    else showTip('no local checkout contains this path');
  }

  function doOpen(payload, allowRetry) {
    var seq = ++reqSeq;
    var url = anchorUrl;
    busy = true;
    renderBusy();
    send({ type: 'open', payload: payload }).then(function (reply) {
      if (seq !== reqSeq) return;
      busy = false;
      if (reply.ok) {
        showOk();
        return;
      }
      if (reply.kind === 'ext-gone') {
        hideNow();
        return;
      }
      if (allowRetry && reply.code === 'INSTANCE_GONE') {
        cache.delete(url);
        send({ type: 'resolve', url: url }).then(function (fresh) {
          if (seq !== reqSeq) return;
          if (fresh.ok) {
            var data = normalize(fresh.data);
            entry = { kind: 'ok', data: data };
            cachePut(url, entry, CACHE_TTL);
            if (data.openInstances.length || data.rootCandidates.length) {
              openPicker(data.openInstances.length ? 'existing' : 'new', 'that instance is gone — pick another');
              return;
            }
          }
          renderError(reply);
        });
        return;
      }
      if (reply.kind === 'daemon-down') {
        renderDown();
        hoverTipForState();
        return;
      }
      renderError(reply);
    });
  }

  function renderBusy() {
    picker.classList.remove('is-on');
    btn.style.display = '';
    btn.className = 'cl-btn is-loading';
    hideTip();
    showPanel();
  }

  // -------------------------------------------------------------- hover flow

  /*
   * The hover flow paints FIRST and asks the daemon LATER. scheduleShow runs on
   * every trusted mouseover of a triaged link and re-targets the overlay in the
   * same tick — a cached verdict renders its final state, an unknown link
   * paints the loading circle — and only once the pointer has DWELT
   * RESOLVE_DELAY on the link is the daemon asked. Scanning quickly down a list
   * of links therefore moves the button instantly from link to link and sends
   * nothing at all for the links merely passed through. (The old shape gated
   * the PAINT on the delay, so exactly that scan kept resetting it and the
   * button lagged or never came.)
   *
   * `px` is the clientX of the mouseover that triggered this — captured and
   * carried to reposition(), because by the time anything paints the event is
   * long gone and the content script has no other way to know where the cursor
   * is. Only the first mouseover on an anchor sets it, so the button lands
   * where the pointer entered the link and stays put instead of chasing it.
   */
  function scheduleShow(a, px) {
    var url = a.href;

    if (pinned) return;
    // The mouseover gate covers the ordinary path; this covers a hover that was
    // already being processed when the pause landed.
    if (settings.paused) return;
    if (a === anchorEl && uiMode !== 'hidden') {
      cancelHide();
      // A dwell that a leave cancelled must be re-armed on re-entry, or the
      // plain button would sit unresolved forever.
      if (uiMode === 'loading' && !resolveTimer && !busy) armDwell(a, url);
      return;
    }

    var cached = cacheGet(url);
    /*
     * Known-silent links return BEFORE cancelHide(), so a hide already in
     * flight for the previous link still completes — hopping from a painted
     * link onto a known-skip link must not strand the old button. They DO stop
     * a pending dwell: the pointer is provably elsewhere now, and the previous
     * link was merely passed through. The 'missing' gate is re-checked on every
     * hover rather than at store time, so turning the badges off in the popup
     * takes effect on links this tab has already seen, without a reload and
     * without waiting out the TTL.
     */
    if (cached && cached.kind === 'skip') {
      stopDwell();
      return;
    }
    if (cached && cached.kind === 'missing' && !badgeVisible(url, cached.owner, cached.repo)) {
      noteRepo(url, cached.owner, cached.repo);
      stopDwell();
      return;
    }

    cancelHide();
    stopDwell();
    anchorEl = a;
    anchorUrl = url;
    if (typeof px === 'number') pointerX = px;

    if (cached && cached.kind === 'ok') {
      entry = cached;
      noteRepo(url, cached.data.parsed.owner, cached.data.parsed.repo);
      renderReady();
      return;
    }
    if (cached && cached.kind === 'missing') {
      entry = cached;
      noteRepo(url, cached.owner, cached.repo);
      renderMissing();
      return;
    }

    entry = null;
    renderLoading();
    armDwell(a, url);
  }

  function armDwell(a, url) {
    resolveTimer = setTimeout(function () {
      resolveTimer = 0;
      if (anchorEl !== a || pinned) return;
      beginResolve(a, url);
    }, RESOLVE_DELAY);
  }

  function stopDwell() {
    if (resolveTimer) {
      clearTimeout(resolveTimer);
      resolveTimer = 0;
    }
  }

  /*
   * The popup's per-repo switch needs to know which repository the page is
   * about, and only verdicts carry that answer — so every one that names a repo
   * is remembered, gated or not: "enable badges for this repo" must work even
   * while the global switch keeps them all hidden.
   */
  function noteRepo(url, owner, repo) {
    if (typeof repo !== 'string' || !repo) return;
    var o = typeof owner === 'string' ? owner : '';
    var h;
    try {
      h = new URL(url, location.href).hostname.toLowerCase();
    } catch (e) {
      h = String(location.hostname || '').toLowerCase();
    }
    lastRepo = {
      key: overrideKey(url, o, repo),
      label: h + '/' + (o ? o + '/' : '') + repo,
    };
  }

  function beginResolve(a, url) {
    var seq = ++reqSeq;
    // The spinner clock starts when the request goes out, not when the button
    // painted: it measures the daemon, not the dwell.
    clearSpin();
    spinTimer = setTimeout(function () {
      spinTimer = 0;
      if (seq !== reqSeq || uiMode !== 'loading') return;
      btn.className = 'cl-btn is-loading';
    }, SPIN_DELAY);
    send({ type: 'resolve', url: url }).then(function (reply) {
      if (seq !== reqSeq || anchorEl !== a) return;
      clearSpin();

      if (reply.ok) {
        var raw = reply.data && typeof reply.data === 'object' ? reply.data : {};
        var data = normalize(raw);
        if (!data.openInstances.length && !data.rootCandidates.length) {
          /*
           * The daemon names WHICH repo has nothing behind it only when the
           * provider captured one; without a repo the verdict is the ordinary
           * "not ours" and must stay silent. `code` lives on the raw payload —
           * normalize() strips unknown fields on purpose. Cached even when the
           * badge is toggled off: the verdict is the expensive part, and
           * scheduleShow re-gates visibility on every hover, so flipping the popup
           * switch takes effect without waiting out a re-resolve.
           */
          var code = raw.code;
          var repo = typeof data.parsed.repo === 'string' ? data.parsed.repo : '';
          if ((code === 'REPO_NOT_LOCAL' || code === 'FILE_NOT_LOCAL') && repo) {
            var miss = {
              kind: 'missing',
              code: code,
              owner: typeof data.parsed.owner === 'string' ? data.parsed.owner : '',
              repo: repo,
              repoPath: typeof data.parsed.repoPath === 'string' ? data.parsed.repoPath : '',
            };
            cachePut(url, miss, CACHE_TTL);
            noteRepo(url, miss.owner, miss.repo);
            if (!badgeVisible(url, miss.owner, miss.repo)) {
              hideNow();
              return;
            }
            entry = miss;
            renderMissing();
            return;
          }
          // "unsupported": no local checkout knows this path. Because triage
          // deliberately over-triggers, this is the *expected* outcome for most
          // links on the site — a grey button here would put a permanent dot on
          // half the page. Cache the negative and show nothing at all.
          // Expiring, not permanent: "no checkout has this path" is a fact about
          // this machine right now, and mounting a worktree must be able to
          // change it without a page reload.
          cachePut(url, { kind: 'skip' }, CACHE_TTL);
          hideNow();
          return;
        }
        entry = { kind: 'ok', data: data };
        cachePut(url, entry, CACHE_TTL);
        noteRepo(url, data.parsed.owner, data.parsed.repo);
        renderReady();
        return;
      }

      if (reply.kind === 'ext-gone') {
        hideNow();
        return;
      }
      if (reply.kind === 'daemon-down') {
        // Never cached: the user may start the daemon at any moment.
        renderDown();
        hoverTipForState();
        return;
      }
      /*
       * NO_PROVIDER means "this URL is not one of my providers" — a Tracker
       * ticket, a wiki page, anything on a matched host that isn't a repo link.
       * The daemon deliberately reports it as HTTP *200* with no `error` field,
       * because it is not an error. Keyed on `code`, never on status: triage
       * over-triggers on purpose, so this is the ordinary outcome for most links
       * and must paint nothing at all. Cached permanently — unlike the empty-list
       * case, this is a judgement on the URL's shape, which no local change can
       * revise.
       */
      if (reply.code === 'NO_PROVIDER') {
        cachePut(url, { kind: 'skip' });
        hideNow();
        return;
      }
      // 4xx from the daemon is a verdict about this URL; cache it as a skip,
      // permanently, for the same reason.
      if (reply.kind === 'api' && reply.status >= 400 && reply.status < 500) {
        cachePut(url, { kind: 'skip' });
        hideNow();
        return;
      }
      renderError(reply);
    });
  }

  // ------------------------------------------------------------------ events

  /*
   * The host page is an SPA: no querySelectorAll snapshot, no MutationObserver.
   * Delegation in the CAPTURE phase survives the page's own stopPropagation.
   * mouseover/mouseout (which bubble) stand in for mouseenter/mouseleave: the
   * relatedTarget containment check below makes them exactly equivalent, and
   * unlike direct binding it needs no cleanup when the SPA swaps nodes out.
   */

  /*
   * isTrusted is the ENTIRE access control on a listener bound to the page's own
   * document. We share the DOM and the event system with the page, so any script
   * on it can build a MouseEvent and dispatch it at an invisible <a> it just
   * created; nothing else in the event distinguishes that from a cursor. Links
   * are never fetched, so an attacker just writes a real provider hostname into
   * the href and drives /resolve at will from any origin the manifest matches.
   * Each forged hover makes the daemon stat that path across every configured
   * checkout root, and while the overlay's shadow root is closed, the button is
   * still hit-testable via document.elementFromPoint at coordinates the page
   * chose — one bit per URL, "is this repo cloned here", which enumerates into
   * the list of private checkouts on the machine. A real cursor is the only
   * legitimate trigger for talking to the daemon. The missing badge adds the
   * complementary bit — "this repo is NOT cloned here" — behind the same
   * real-cursor gate and the popup's badge switches; it observes the same
   * boundary, it does not move it.
   *
   * The teardown listeners below (mouseout, pointerdown, blur) deliberately do
   * NOT check it: their only effect is to return the overlay to its resting
   * state, which is the safe direction, and a page that wants the overlay gone
   * can simply remove the anchor. Nor do scroll/resize, which merely reposition
   * a panel that is already up — and which pages and layout libraries routinely
   * fire synthetically to force a relayout.
   */
  document.addEventListener(
    'mouseover',
    function (e) {
      if (!e.isTrusted) return;
      if (settings.paused) return;
      var a = e.target instanceof Element && e.target.closest('a[href]');
      if (!a || !triage(a.href)) return;
      scheduleShow(a, e.clientX);
    },
    true
  );

  document.addEventListener(
    'mouseout',
    function (e) {
      var a = e.target instanceof Element && e.target.closest('a[href]');
      if (!a) return;
      var to = e.relatedTarget;
      if (to instanceof Node && a.contains(to)) return; // still within the anchor
      if (a === anchorEl) {
        /*
         * Leaving the link cancels a dwell still pending: the daemon must not
         * be asked about a link merely passed through, and the reply would
         * repaint a button the pointer has already abandoned. The one
         * exception is leaving ONTO the overlay itself — events from the
         * closed shadow root retarget to its host — which is ordinary travel
         * toward a button that sits flush against the link.
         */
        if (!(to instanceof Node && to === host)) stopDwell();
        startHide();
      }
    },
    true
  );

  panel.addEventListener('mouseenter', function () {
    overPanel = true;
    cancelHide();
  });
  panel.addEventListener('mouseleave', function () {
    overPanel = false;
    startHide();
  });

  // Do not let the overlay steal focus or start a text selection.
  btn.addEventListener('mousedown', function (e) {
    e.preventDefault();
    e.stopPropagation();
  });
  btn.addEventListener('click', onButtonClick);
  btn.addEventListener('mouseenter', hoverTipForState);
  btn.addEventListener('mouseleave', hideTip);

  /*
   * Same trust rule as the hover listener, and the stakes are higher: while the
   * picker is pinned, Enter COMMITS — a forged one sends /open, and forged
   * arrows move the selection first, so the page would choose which checkout the
   * user's own picker opens.
   */
  document.addEventListener(
    'keydown',
    function (e) {
      if (!e.isTrusted) return;
      if (!pinned) return;
      var k = e.key;
      if (k === 'ArrowDown' || k === 'ArrowUp' || k === 'Enter' || k === 'Escape' || k === 'Tab') {
        e.preventDefault();
        e.stopPropagation();
      } else {
        return;
      }
      if (k === 'Escape' || k === 'Tab') {
        closePicker('restore');
      } else if (k === 'ArrowDown') {
        setSelection(pickerIndex + 1);
      } else if (k === 'ArrowUp') {
        setSelection(pickerIndex - 1);
      } else if (k === 'Enter') {
        choose(pickerIndex);
      }
    },
    true
  );

  document.addEventListener(
    'pointerdown',
    function (e) {
      if (!pinned) return;
      // Events from inside a closed shadow root are retargeted to the host.
      if (e.target === host) return;
      closePicker('dismiss');
    },
    true
  );

  /*
   * Answers the popup's "which repo is this page about?". Only extension
   * contexts can reach this: without externally_connectable in the manifest,
   * page scripts cannot address this extension's runtime messaging at all.
   * Guarded, because the jsdom harnesses stub chrome.runtime without onMessage.
   */
  try {
    chrome.runtime.onMessage.addListener(function (msg, sender, sendResponse) {
      if (msg && msg.type === 'repo-info') sendResponse(lastRepo || {});
    });
  } catch (e) {
    /* no runtime messaging here */
  }

  document.addEventListener('scroll', onViewportChange, { passive: true, capture: true });
  window.addEventListener('resize', onViewportChange, { passive: true });
  /*
   * Dismiss the picker when the WINDOW loses focus (alt-tab, another app).
   *
   * The target check is essential. `blur` does not bubble, but it does
   * propagate through the CAPTURE phase — so a capture listener on window
   * receives the blur of every element in the document, not just the window's
   * own. openPicker() calls picker.focus(), which blurs whatever had focus, and
   * without this guard that self-inflicted blur dismissed the picker in the very
   * tick it was opened: the button vanished and nothing replaced it.
   */
  window.addEventListener(
    'blur',
    function (e) {
      if (e.target !== window && e.target !== document) return;
      if (pinned) closePicker('dismiss');
    },
    true
  );
})();
