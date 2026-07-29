/*
 * Regression test for the SPA detached-anchor bug.
 *
 * A single-page-app code host replaces the <a> node between hover and click.
 * openPicker() built its rows and then called showPanel() -> reposition(), which
 * saw the dead anchor and called hideNow() in the SAME TICK — wiping entry,
 * anchorEl and both visibility classes. Result: the button vanished on click and
 * no picker ever painted.
 *
 * Asserts the picker survives its anchor being detached.
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
const LINK_URL =
  `https://${PAGE_HOST}/repo/src/pkg/widget/lib/main.go?rev=r1#L192`;

const RESOLVE = {
  ok: true,
  data: {
    parsed: {
      provider: 'example',
      repoPath: 'src/pkg/widget/lib/main.go',
      line: 192, endLine: null, refIsDefault: false,
      project: 'src/pkg/widget', projectName: 'widget', kind: 'file',
    },
    openInstances: [],
    rootCandidates: [
      { root: '/Users/you/checkouts/feature-a', label: 'feature-a', localPath: '/x', recency: 2, hasOpenInstance: false },
      { root: '/Users/you/checkouts/main', label: 'main', localPath: '/y', recency: 1, hasOpenInstance: false },
    ],
    warnings: ['rev=r1 — pinned revision; local file may differ'],
  },
};

const dom = new JSDOM(
  `<!doctype html><html><body><p>see <a id="lnk" href="${LINK_URL}">link</a></p></body></html>`,
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
 * The overlay only reacts to events the user agent itself produced — a page
 * script cannot forge a hover and drive the daemon (see untrusted-events.test.js).
 * Every event jsdom builds is untrusted, and dispatchEvent() re-stamps
 * isTrusted = false on the way in, exactly as the DOM spec requires, so a test
 * cannot just set the flag on the event it is about to send. Go through the same
 * internal dispatch the user agent uses, which leaves the flag alone: `user()`
 * is a real cursor, and this test simulates a real user throughout.
 */
const IMPL = Object.getOwnPropertySymbols(new window.Event('probe'))[0];

function user(target, event) {
  event[IMPL].isTrusted = true;
  target[IMPL]._dispatch(event[IMPL]);
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const doc = window.document;

// The overlay host is the only child of <html> carrying our z-index.
function findHost() {
  return [...doc.documentElement.children].find((n) => n.style && n.style.zIndex === '2147483647');
}

(async () => {
  const a = doc.getElementById('lnk');
  let fails = 0;
  const check = (name, cond, extra) => {
    console.log((cond ? 'PASS  ' : 'FAIL  ') + name + (cond ? '' : '   <- ' + extra));
    if (!cond) fails++;
  };

  // --- hover -------------------------------------------------------------
  user(a, new window.MouseEvent('mouseover', { bubbles: true }));
  await sleep(400);

  const host = findHost();
  check('overlay host exists', !!host);
  check('/resolve was requested', sent.some((m) => m.type === 'resolve'), JSON.stringify(sent));

  const openShadow = capturedRoot;
  const panel = openShadow.getElementById('panel');

  // --- THE BUG: detach the anchor, exactly as an SPA re-render would ------
  a.remove();
  check('anchor is detached', !a.isConnected);

  // --- click the button ---------------------------------------------------
  const btn = openShadow.querySelector('.cl-btn');
  check('button element found', !!btn);
  check('panel is-open before click', panel.classList.contains('is-open'), panel.className);

  user(btn, new window.MouseEvent('click', { bubbles: true, composed: true }));
  await sleep(50);

  const picker = openShadow.querySelector('.cl-picker');
  check('picker has is-on after click on a DETACHED anchor',
        picker.classList.contains('is-on'), 'class=' + picker.className);
  check('panel has is-open after click on a DETACHED anchor',
        panel.classList.contains('is-open'), 'class=' + panel.className);
  check('picker rendered 2 rows', picker.querySelectorAll('.cl-row').length === 2,
        'rows=' + picker.querySelectorAll('.cl-row').length);

  // --- capture-phase blur bug --------------------------------------------
  // `blur` does not bubble but DOES capture, so a capture listener on window
  // sees every element's blur. picker.focus() blurs the previously focused
  // element, which used to dismiss the picker in the tick it opened.
  user(doc.body, new window.FocusEvent('blur', { bubbles: false }));
  await sleep(10);
  check('picker survives an ELEMENT blur (capture-phase leak)',
        picker.classList.contains('is-on'), 'class=' + picker.className);

  // ...but a real window blur must still dismiss it.
  user(window, new window.FocusEvent('blur', { bubbles: false }));
  await sleep(10);
  check('picker closes on a genuine WINDOW blur',
        !picker.classList.contains('is-on'), 'class=' + picker.className);

  console.log(fails ? `\n${fails} FAILURE(S)` : '\nall checks passed');
  process.exit(fails ? 1 : 0);
})();
