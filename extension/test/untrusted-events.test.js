/*
 * Regression test for page-forged events driving the daemon.
 *
 * A content script shares the DOM and the event system with the page it is
 * injected into, so any page script can build a MouseEvent and dispatch it at an
 * <a> it just created. The hover listener had no `isTrusted` check, so that
 * forgery reached scheduleShow() -> /resolve with zero user interaction: the
 * page picked the href, the daemon stat-ed that path across every configured
 * checkout root, and the resulting button — hit-testable through
 * document.elementFromPoint even inside a closed shadow root — leaked one bit
 * per URL, "is this repo cloned on your machine".
 *
 * The same hole applied to the document-level keydown listener: while the picker
 * is pinned, a forged Enter commits a choice, i.e. sends /open.
 *
 * Asserts that forged events do nothing, that real ones still work, and that the
 * teardown listeners deliberately stay permissive.
 *
 * The fixture is deliberately synthetic: host, repo layout and checkout roots are
 * placeholders, and the host allowlist is injected here rather than read from the
 * generated hosts.gen.js, so the test is hermetic and carries no site knowledge.
 */
const fs = require('fs');
const { JSDOM } = require('jsdom');

const SRC = process.env.CL_SRC || require('path').join(__dirname, '..', 'content.js');
const HOSTS_JS = `self.CODELINK_HOSTS = ["*.example.com"];`;

const PAGE_HOST = 'code.example.com';
const LINK_URL = `https://${PAGE_HOST}/repo/src/pkg/widget/lib/main.go#L192`;

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
      { root: '/Users/you/checkouts/feature-a', label: 'feature-a', localPath: '/x', recency: 2, hasOpenInstance: false },
      { root: '/Users/you/checkouts/main', label: 'main', localPath: '/y', recency: 1, hasOpenInstance: false },
    ],
    warnings: [],
  },
};

const dom = new JSDOM(
  `<!doctype html><html><body>
     <p>see <a id="lnk" href="${LINK_URL}">link</a></p>
     <p id="elsewhere">elsewhere</p>
   </body></html>`,
  { url: `https://${PAGE_HOST}/repo/src/pkg`, pretendToBeVisual: true, runScripts: 'outside-only' }
);
const { window } = dom;

// jsdom has no layout: give every element a plausible rect so firstRect() works.
window.Element.prototype.getClientRects = function () {
  if (!this.isConnected) return [];
  const r = { left: 100, top: 100, right: 200, bottom: 120, width: 100, height: 20, x: 100, y: 100 };
  return Object.assign([r], { length: 1 });
};
window.Element.prototype.getBoundingClientRect = function () {
  return this.isConnected
    ? { left: 100, top: 100, right: 200, bottom: 120, width: 100, height: 20, x: 100, y: 100 }
    : { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0, x: 0, y: 0 };
};

const sent = [];
window.chrome = {
  runtime: {
    id: 'test',
    getURL: (p) => 'chrome-extension://test/' + p,
    lastError: null,
    sendMessage(msg, cb) {
      sent.push(msg);
      setTimeout(() => cb(msg.type === 'resolve' ? RESOLVE : { ok: true, data: { ok: true } }), 0);
    },
  },
};

['requestAnimationFrame', 'cancelAnimationFrame'].forEach((k) => {
  if (!window[k]) window[k] = (f) => setTimeout(f, 0);
});

// The overlay uses a CLOSED shadow root, deliberately, so page script cannot
// reach in. Capture the root at creation time — the only way to drive the UI
// from a test without weakening the production code.
let capturedRoot = null;
const realAttach = window.Element.prototype.attachShadow;
window.Element.prototype.attachShadow = function (init) {
  const r = realAttach.call(this, init);
  capturedRoot = r;
  return r;
};

window.eval(HOSTS_JS);
window.eval(fs.readFileSync(SRC, 'utf8'));

/*
 * Every event jsdom builds is untrusted, and dispatchEvent() re-stamps
 * isTrusted = false on the way in, exactly as the DOM spec requires — so a test
 * cannot simply set the flag on the event it is about to send. Go through the
 * same internal dispatch the user agent itself uses, which leaves the flag
 * alone. `user()` is therefore a real cursor / keyboard; a plain dispatchEvent()
 * below is a page script forging one.
 */
const IMPL = Object.getOwnPropertySymbols(new window.Event('probe'))[0];

function user(target, event) {
  event[IMPL].isTrusted = true;
  target[IMPL]._dispatch(event[IMPL]);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const doc = window.document;
const count = (type) => sent.filter((m) => m.type === type).length;

(async () => {
  const lnk = doc.getElementById('lnk');
  let fails = 0;
  const check = (name, cond, extra) => {
    console.log((cond ? 'PASS  ' : 'FAIL  ') + name + (cond ? '' : '   <- ' + extra));
    if (!cond) fails++;
  };

  const shadow = capturedRoot;
  const panel = shadow.getElementById('panel');
  const btn = shadow.querySelector('.cl-btn');
  const picker = shadow.querySelector('.cl-picker');

  // --- a page script forges a hover ---------------------------------------
  lnk.dispatchEvent(new window.MouseEvent('mouseover', { bubbles: true }));
  await sleep(400);
  check('forged mouseover sends nothing to the service worker', sent.length === 0, JSON.stringify(sent));
  check('forged mouseover paints no overlay', !panel.classList.contains('is-open'), panel.className);

  // --- a real cursor still works ------------------------------------------
  user(lnk, new window.MouseEvent('mouseover', { bubbles: true }));
  await sleep(400);
  check('trusted mouseover resolves', count('resolve') === 1, 'n=' + count('resolve'));
  check('trusted mouseover opens the panel', panel.classList.contains('is-open'), panel.className);

  user(btn, new window.MouseEvent('click', { bubbles: true, composed: true }));
  await sleep(50);
  check('picker opened with both roots', picker.querySelectorAll('.cl-row').length === 2,
        'rows=' + picker.querySelectorAll('.cl-row').length);

  // --- teardown stays permissive, by design -------------------------------
  // A forged pointerdown can only put the overlay back to rest. That is the safe
  // direction and the page can already get there by removing the anchor, so this
  // listener is deliberately NOT gated on isTrusted.
  doc.getElementById('elsewhere').dispatchEvent(new window.PointerEvent('pointerdown', { bubbles: true }));
  await sleep(10);
  check('forged pointerdown still dismisses the picker', !picker.classList.contains('is-on'), picker.className);

  // --- forged keys while the picker is pinned ------------------------------
  user(lnk, new window.MouseEvent('mouseover', { bubbles: true }));
  await sleep(400);
  user(btn, new window.MouseEvent('click', { bubbles: true, composed: true }));
  await sleep(50);
  check('picker reopened', picker.classList.contains('is-on'), picker.className);

  doc.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
  doc.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
  await sleep(50);
  check('forged Enter sends no open', count('open') === 0, 'n=' + count('open'));
  check('forged Enter leaves the picker up', picker.classList.contains('is-on'), picker.className);
  const selected = [...picker.querySelectorAll('.cl-row')].findIndex((r) => r.classList.contains('is-sel'));
  check('forged ArrowDown does not move the selection', selected === 0, 'sel=' + selected);

  // --- a real Enter still opens, on the row the USER had selected ----------
  user(doc, new window.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
  await sleep(50);
  const open = sent.filter((m) => m.type === 'open').pop();
  check('trusted Enter sends one open', count('open') === 1, 'n=' + count('open'));
  check('open targets the row the user had selected',
        open && open.payload.target === '/Users/you/checkouts/feature-a',
        'target=' + (open && open.payload.target));

  console.log(fails ? `\n${fails} FAILURE(S)` : '\nall checks passed');
  process.exit(fails ? 1 : 0);
})();
