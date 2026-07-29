/*
 * Guards the button's size and the two places that must agree with it.
 *
 * The 33px circle is stated three times over: in content.css, in the copy of
 * content.css inlined into content.js, and — as a bare number — in content.js's
 * positioning maths, which lifts the panel one button-height above the link's
 * line box against it.
 * Let any of the three drift and the button either sits off its link or is
 * clamped by the wrong amount, with nothing to notice it. The tooltip's own
 * offset is derived from the same number for the same reason.
 *
 * jsdom has NO layout and does not apply a shadow root's stylesheet, so
 * getComputedStyle here returns nothing useful. What is checkable is what the
 * extension actually ships — the declarations in the stylesheet it injects — and
 * the transform reposition() computes from a known rect. Neither is a
 * substitute for looking at the thing in a real browser.
 *
 * The fixture is deliberately synthetic: host, repo layout and checkout roots are
 * placeholders, and the host allowlist is injected here rather than read from the
 * generated hosts.gen.js, so the test is hermetic and carries no site knowledge.
 */
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const SRC = process.env.CL_SRC || path.join(__dirname, '..', 'content.js');
const CSS_SRC = path.join(__dirname, '..', 'content.css');
const HOSTS_JS = `self.CODELINK_HOSTS = ["*.example.com"];`;

const PAGE_HOST = 'code.example.com';
const LINK_URL = `https://${PAGE_HOST}/repo/src/pkg/widget/lib/main.go#L192`;

// One line of text, 100px wide, 20px tall, at (100,100). The numbers below are
// worked out by hand from this rect, so a change to it changes them all.
const RECT = { left: 100, top: 100, right: 200, bottom: 120, width: 100, height: 20, x: 100, y: 100 };
const BTN = 33;
const GAP = 4;

const RESOLVE = {
  ok: true,
  data: {
    parsed: {
      provider: 'example',
      repoPath: 'src/pkg/widget/lib/main.go',
      line: 192, endLine: null, refIsDefault: true,
      project: 'src/pkg/widget', projectName: 'widget', kind: 'file',
    },
    openInstances: [],
    rootCandidates: [
      { root: '/Users/you/checkouts/main', label: 'main', localPath: '/y', recency: 1, hasOpenInstance: false },
    ],
    warnings: [],
  },
};

const dom = new JSDOM(
  `<!doctype html><html><body><p>see <a id="lnk" href="${LINK_URL}">link</a></p></body></html>`,
  { url: `https://${PAGE_HOST}/repo/src/pkg`, pretendToBeVisual: true, runScripts: 'outside-only' }
);
const { window } = dom;

window.Element.prototype.getClientRects = function () {
  if (!this.isConnected) return [];
  return Object.assign([Object.assign({}, RECT)], { length: 1 });
};
window.Element.prototype.getBoundingClientRect = function () {
  return this.isConnected
    ? Object.assign({}, RECT)
    : { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0, x: 0, y: 0 };
};

window.chrome = {
  runtime: {
    id: 'test',
    getURL: (p) => 'chrome-extension://test/' + p,
    lastError: null,
    sendMessage(msg, cb) {
      setTimeout(() => cb(msg.type === 'resolve' ? RESOLVE : { ok: true, data: { ok: true } }), 0);
    },
  },
};

['requestAnimationFrame', 'cancelAnimationFrame'].forEach((k) => {
  if (!window[k]) window[k] = (f) => setTimeout(f, 0);
});

let capturedRoot = null;
const realAttach = window.Element.prototype.attachShadow;
window.Element.prototype.attachShadow = function (init) {
  const r = realAttach.call(this, init);
  capturedRoot = r;
  return r;
};

window.eval(HOSTS_JS);
window.eval(fs.readFileSync(SRC, 'utf8'));

// See untrusted-events.test.js: the overlay only reacts to events the user agent
// produced, and jsdom's dispatchEvent stamps isTrusted = false.
const IMPL = Object.getOwnPropertySymbols(new window.Event('probe'))[0];

function user(target, event) {
  event[IMPL].isTrusted = true;
  target[IMPL]._dispatch(event[IMPL]);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const esc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

(async () => {
  let fails = 0;
  const check = (name, cond, extra) => {
    console.log((cond ? 'PASS  ' : 'FAIL  ') + name + (cond ? '' : '   <- ' + extra));
    if (!cond) fails++;
  };

  const shadow = capturedRoot;
  const shipped = shadow.querySelector('style').textContent;

  // --- content.css -> content.js was actually re-inlined -------------------
  check(
    'the inlined stylesheet matches content.css',
    shipped.trim() === fs.readFileSync(CSS_SRC, 'utf8').trim(),
    'content.js carries a stale copy — re-run the inliner, see README.md'
  );

  // --- declared geometry ---------------------------------------------------
  const css = shipped.replace(/\/\*[\s\S]*?\*\//g, '');
  const body = (sel) => {
    const m = css.match(new RegExp('(?:^|[},])\\s*' + esc(sel) + '\\s*\\{([^}]*)\\}'));
    return m ? m[1] : '';
  };
  const decl = (sel, prop) => {
    const m = body(sel).match(new RegExp('(?:^|;)\\s*' + esc(prop) + '\\s*:\\s*([^;]+)'));
    return m ? m[1].trim() : null;
  };
  const is = (sel, prop, want) =>
    check(sel + ' ' + prop + ' is ' + want, decl(sel, prop) === want, 'got ' + decl(sel, prop));

  is('.cl-btn', 'width', BTN + 'px');
  is('.cl-btn', 'height', BTN + 'px');
  // Everything inside the circle is its own original x 1.5, which lands three of
  // them on a half pixel. They are kept fractional on purpose — see content.css.
  is('.cl-btn .cl-mark', 'width', '15px');
  is('.cl-btn .cl-mark', 'height', '18px');
  is('.cl-spin', 'inset', '3px');
  is('.cl-spin', 'border', '2.25px solid transparent');
  is('.cl-check', 'width', '16.5px');
  is('.cl-check', 'height', '16.5px');
  is('.cl-badge', 'width', '10.5px');
  is('.cl-badge', 'height', '10.5px');
  is('.cl-badge', 'top', '-3px');
  is('.cl-badge', 'right', '-3px');
  is('.cl-badge', 'box-shadow', '0 0 0 2.25px var(--cl-surface)');

  // The can't-resolve cross mirrors the ref badge in the opposite corner —
  // same circle, same surface ring, or the two stop reading as a family.
  is('.cl-xbadge', 'width', '10.5px');
  is('.cl-xbadge', 'height', '10.5px');
  is('.cl-xbadge', 'bottom', '-3px');
  is('.cl-xbadge', 'right', '-3px');
  is('.cl-xbadge', 'box-shadow', '0 0 0 2.25px var(--cl-surface)');

  // The checkmark svg is drawn in a 12x12 viewBox, so the box scale already
  // thickens the stroke; the number must NOT be scaled a second time.
  is('.cl-check', 'stroke-width', '2.2');

  // The tooltip hangs off the bottom of the button, so its offset is derived.
  is('.cl-tip', 'top', BTN + GAP + 'px');
  is('.cl-tip.is-above', 'bottom', BTN + GAP + 'px');

  // --- placement, which is where content.js's own copy of the size shows up ---
  const lnk = window.document.getElementById('lnk');
  const POINTER_X = 140; // clientX of the hover that starts the show
  const DX = 10; // POINTER_DX in content.js
  user(lnk, new window.MouseEvent('mouseover', { bubbles: true, clientX: POINTER_X }));
  await sleep(400);

  const panel = shadow.getElementById('panel');
  check('panel is up', panel.classList.contains('is-open'), panel.className);

  // Above the link's line box — one button plus one gap — and a little right of
  // where the pointer entered, not of where the link happens to end.
  let want =
    'translate3d(' +
    Math.round(POINTER_X + DX) + 'px,' +
    Math.round(RECT.top - GAP - BTN) + 'px,0)';
  check('panel sits above the line, right of the pointer',
        panel.style.transform === want, 'got ' + panel.style.transform + ', want ' + want);

  // A link on the first line of the viewport has no room above; the button
  // mirrors below with the same offset, the only other spot off the line.
  user(lnk, new window.MouseEvent('mouseout', { bubbles: true }));
  await sleep(250);
  RECT.top = 10; RECT.bottom = 30; RECT.y = 10;
  user(lnk, new window.MouseEvent('mouseover', { bubbles: true, clientX: POINTER_X }));
  await sleep(400);
  want =
    'translate3d(' +
    Math.round(POINTER_X + DX) + 'px,' +
    Math.round(RECT.bottom + GAP) + 'px,0)';
  check('no room above: panel flips below the link',
        panel.style.transform === want, 'got ' + panel.style.transform + ', want ' + want);

  console.log(fails ? `\n${fails} FAILURE(S)` : '\nall checks passed');
  process.exit(fails ? 1 : 0);
})();
