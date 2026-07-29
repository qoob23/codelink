/*
 * Regression test for the stale-instance-cache bug.
 *
 * The content script's resolve cache was evicted only by LRU capacity, so a
 * URL's `openInstances` were frozen for the tab's whole life. Hover a link
 * before starting Neovim and the empty instance list was cached; start Neovim,
 * hover the same link again, and the overlay still said "Open in a new Neovim"
 * and offered the new-instance picker. Worse, cacheGet refreshed recency on
 * every read, so the links you use most were the ones that never expired.
 *
 * Asserts that a cached resolve is reused while fresh, is re-fetched once its
 * TTL passes, and that a NO_PROVIDER verdict — a fact about the URL's shape,
 * not about this machine — is still cached forever.
 *
 * The fixture is deliberately synthetic: host, repo layout and checkout roots
 * are placeholders, and the host allowlist is injected here rather than read
 * from the generated hosts.gen.js, so the test is hermetic and carries no site
 * knowledge.
 */
const fs = require('fs');
const { JSDOM } = require('jsdom');

const SRC = process.env.CL_SRC || require('path').join(__dirname, '..', 'content.js');
const HOSTS_JS = `self.CODELINK_HOSTS = ["*.example.com"];`;

const PAGE_HOST = 'code.example.com';
const LINK_URL = `https://${PAGE_HOST}/repo/src/pkg/widget/lib/main.go#L192`;
const TICKET_URL = `https://${PAGE_HOST}/tickets/PROJ-1/comments/src/pkg/widget`;

const PARSED = {
  provider: 'example',
  repoPath: 'src/pkg/widget/lib/main.go',
  line: 192,
  endLine: null,
  refIsDefault: true,
  project: 'src/pkg/widget',
  projectName: 'widget',
  kind: 'file',
};
const ROOTS = [
  { root: '/Users/you/checkouts/feature-a', label: 'feature-a', localPath: '/x', recency: 2, hasOpenInstance: false },
];
const INSTANCE = {
  id: '4242',
  label: 'feature-a',
  root: '/Users/you/checkouts/feature-a',
  cwd: '/Users/you/checkouts/feature-a/src/pkg/widget',
  localPath: '/x',
  inProject: true,
  lastFocused: 100,
  focusable: 'heuristic',
};

// Flipped mid-test to stand in for "the user just started Neovim".
let nvimRunning = false;

function resolveReply() {
  return {
    ok: true,
    data: {
      parsed: PARSED,
      openInstances: nvimRunning ? [INSTANCE] : [],
      rootCandidates: ROOTS,
      warnings: [],
    },
  };
}

const dom = new JSDOM(
  `<!doctype html><html><body>
     <p>see <a id="lnk" href="${LINK_URL}">link</a></p>
     <p>and <a id="tkt" href="${TICKET_URL}">ticket</a></p>
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

/*
 * A controllable clock, so the test does not have to sleep out the real TTL.
 * Only the cache reads Date.now(); the overlay's own hover/hide delays run on
 * setTimeout and are left on real time, which keeps the event sequencing honest.
 */
let clock = 1000000;
window.Date.now = () => clock;

const sent = [];
window.chrome = {
  runtime: {
    id: 'test',
    getURL: (p) => 'chrome-extension://test/' + p,
    lastError: null,
    sendMessage(msg, cb) {
      sent.push(msg);
      setTimeout(() => {
        if (msg.type !== 'resolve') return cb({ ok: true, data: { ok: true } });
        if (msg.url === TICKET_URL) return cb({ ok: false, code: 'NO_PROVIDER' });
        cb(resolveReply());
      }, 0);
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

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const doc = window.document;
const away = doc.getElementById('elsewhere');

const resolves = (url) => sent.filter((m) => m.type === 'resolve' && m.url === url).length;

async function hover(a) {
  a.dispatchEvent(new window.MouseEvent('mouseover', { bubbles: true }));
  await sleep(400);
}

async function unhover(a) {
  a.dispatchEvent(new window.MouseEvent('mouseout', { bubbles: true, relatedTarget: away }));
  await sleep(300);
}

(async () => {
  const lnk = doc.getElementById('lnk');
  const tkt = doc.getElementById('tkt');
  let fails = 0;
  const check = (name, cond, extra) => {
    console.log((cond ? 'PASS  ' : 'FAIL  ') + name + (cond ? '' : '   <- ' + extra));
    if (!cond) fails++;
  };

  // --- first hover, no editor running ------------------------------------
  await hover(lnk);
  const shadow = capturedRoot;
  const tip = shadow.getElementById('tip') || shadow.querySelector('.cl-tip');
  check('first hover resolved', resolves(LINK_URL) === 1, 'n=' + resolves(LINK_URL));

  // --- re-hover while fresh: must NOT re-resolve --------------------------
  await unhover(lnk);
  await hover(lnk);
  check('fresh cache hit does not re-resolve', resolves(LINK_URL) === 1, 'n=' + resolves(LINK_URL));

  // --- the user starts Neovim, and time passes past the TTL ---------------
  nvimRunning = true;
  clock += 11000;

  await unhover(lnk);
  await hover(lnk);
  check('expired cache re-resolves', resolves(LINK_URL) === 2, 'n=' + resolves(LINK_URL));

  // The whole point: the click must now reuse the running instance instead of
  // offering to spawn a new one.
  const btn = shadow.querySelector('.cl-btn');
  btn.dispatchEvent(new window.MouseEvent('mouseenter', { bubbles: true }));
  await sleep(10);
  check(
    'tooltip names the running instance',
    /Open in feature-a/.test(tip ? tip.textContent : ''),
    'tip=' + JSON.stringify(tip ? tip.textContent : null)
  );

  btn.dispatchEvent(new window.MouseEvent('click', { bubbles: true, composed: true }));
  await sleep(50);

  const picker = shadow.querySelector('.cl-picker');
  check('no new-instance picker opened', !picker.classList.contains('is-on'), 'class=' + picker.className);

  const open = sent.filter((m) => m.type === 'open').pop();
  check('an open was sent', !!open, JSON.stringify(sent.map((m) => m.type)));
  check('open reuses the existing instance', open && open.payload.mode === 'existing',
        'mode=' + (open && open.payload.mode));
  check('open targets the instance id', open && open.payload.target === '4242',
        'target=' + (open && open.payload.target));
  check('open carries the line', open && open.payload.line === 192,
        'line=' + (open && open.payload.line));

  // --- NO_PROVIDER stays cached forever -----------------------------------
  await unhover(lnk);
  await hover(tkt);
  check('ticket link resolved once', resolves(TICKET_URL) === 1, 'n=' + resolves(TICKET_URL));

  clock += 600000; // ten minutes
  await unhover(tkt);
  await hover(tkt);
  check('NO_PROVIDER verdict never expires', resolves(TICKET_URL) === 1, 'n=' + resolves(TICKET_URL));

  console.log(fails ? `\n${fails} FAILURE(S)` : '\nall checks passed');
  process.exit(fails ? 1 : 0);
})();
