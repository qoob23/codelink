/*
 * The hover flow paints first and resolves later. Pinned here:
 *
 *   - the button is up — PLAIN, no spinner — before the daemon has been asked;
 *   - the daemon is asked only after the pointer dwells RESOLVE_DELAY on the
 *     link, so hopping across links (or leaving to empty space) sends nothing
 *     for the ones merely passed through;
 *   - a cached verdict renders its final state instantly, with no new resolve;
 *   - the spinner appears only when a resolve has been in flight SPIN_DELAY
 *     without an answer — a fast daemon never shows one;
 *   - a leave-and-return within the hide grace re-arms the dwell.
 *
 * The numbers lean on RESOLVE_DELAY = 150, SPIN_DELAY = 200, HIDE_DELAY = 120:
 * "before" probes sit well inside a window, "after" probes well past it. Same
 * hermetic fixture rules as the siblings.
 */
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const SRC = process.env.CL_SRC || path.join(__dirname, '..', 'content.js');
const HOSTS_JS = `self.CODELINK_HOSTS = ["*.example.com"];`;

const PAGE_HOST = 'code.example.com';
const URL_ONE = `https://${PAGE_HOST}/repo/widget/lib/one.go`;
const URL_TWO = `https://${PAGE_HOST}/repo/widget/lib/two.go`;

const RECT = { left: 100, top: 100, right: 200, bottom: 120, width: 100, height: 20, x: 100, y: 100 };

const RESOLVE = {
  ok: true,
  data: {
    parsed: {
      provider: 'example', repoPath: 'lib/one.go',
      line: null, endLine: null, refIsDefault: true,
      project: '', projectName: '', kind: 'file',
    },
    openInstances: [],
    rootCandidates: [
      { root: '/Users/you/checkouts/widget', label: 'widget', localPath: '/y', recency: 1, hasOpenInstance: false },
    ],
    warnings: [],
  },
};

function makeWorld(replyDelayMs) {
  const dom = new JSDOM(
    `<!doctype html><html><body>
       <a id="lnk1" href="${URL_ONE}">one</a>
       <a id="lnk2" href="${URL_TWO}">two</a>
     </body></html>`,
    { url: `https://${PAGE_HOST}/repo/widget`, pretendToBeVisual: true, runScripts: 'outside-only' }
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

  const sent = [];
  window.chrome = {
    runtime: {
      id: 'test',
      getURL: (p) => 'chrome-extension://test/' + p,
      lastError: null,
      sendMessage(msg, cb) {
        sent.push(msg);
        setTimeout(
          () => cb(msg.type === 'resolve' ? RESOLVE : { ok: true, data: { ok: true } }),
          replyDelayMs || 0
        );
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

  const IMPL = Object.getOwnPropertySymbols(new window.Event('probe'))[0];
  const user = (target, event) => {
    event[IMPL].isTrusted = true;
    target[IMPL]._dispatch(event[IMPL]);
  };

  return { window, shadow: capturedRoot, sent, user };
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const resolves = (sent) => sent.filter((m) => m.type === 'resolve');

(async () => {
  let fails = 0;
  const check = (name, cond, extra) => {
    console.log((cond ? 'PASS  ' : 'FAIL  ') + name + (cond ? '' : '   <- ' + extra));
    if (!cond) fails++;
  };

  // --- paint precedes the daemon ---------------------------------------------
  {
    const w = makeWorld();
    const btn = w.shadow.querySelector('.cl-btn');
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');

    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(60);
    check('button is up, plain, before the dwell elapses',
          panel.classList.contains('is-open') && !btn.classList.contains('is-loading'),
          'panel=' + panel.className + ' btn=' + btn.className);
    check('...and the daemon has not been asked yet',
          resolves(w.sent).length === 0, resolves(w.sent).length + ' resolves');

    await sleep(340);
    check('after the dwell: exactly one resolve', resolves(w.sent).length === 1,
          resolves(w.sent).length + ' resolves');
    check('...and the button reached ready, never having spun',
          panel.classList.contains('is-open') && !btn.classList.contains('is-loading'),
          'btn=' + btn.className);

    // --- cached verdicts render instantly ------------------------------------
    w.user(lnk1, new w.window.MouseEvent('mouseout', { bubbles: true }));
    await sleep(250);
    check('panel hid between hovers', !panel.classList.contains('is-open'), panel.className);
    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(60);
    check('cache hit renders ready within the dwell window',
          panel.classList.contains('is-open') && !btn.classList.contains('is-loading'),
          'panel=' + panel.className + ' btn=' + btn.className);
    check('...with no second resolve', resolves(w.sent).length === 1,
          resolves(w.sent).length + ' resolves');
  }

  // --- hopping sends nothing for the link passed through ---------------------
  {
    const w = makeWorld();
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');
    const lnk2 = w.window.document.getElementById('lnk2');

    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(60);
    w.user(lnk1, new w.window.MouseEvent('mouseout', { bubbles: true }));
    w.user(lnk2, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 140 }));
    await sleep(400);

    const urls = resolves(w.sent).map((m) => m.url);
    check('only the dwelt-on link was resolved',
          urls.length === 1 && urls[0] === URL_TWO, JSON.stringify(urls));
    check('panel ended up on the second link', panel.classList.contains('is-open'), panel.className);
  }

  // --- leaving to empty space cancels the dwell ------------------------------
  {
    const w = makeWorld();
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');

    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(60);
    w.user(lnk1, new w.window.MouseEvent('mouseout', { bubbles: true }));
    await sleep(400);
    check('leave to nowhere: the daemon was never asked',
          resolves(w.sent).length === 0, resolves(w.sent).length + ' resolves');
    check('...and the plain button retired', !panel.classList.contains('is-open'), panel.className);

    // Return within the hide grace: the panel never hid, so the dwell must be
    // re-armed or the plain button would sit unresolved forever.
    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(50);
    w.user(lnk1, new w.window.MouseEvent('mouseout', { bubbles: true }));
    await sleep(60); // inside HIDE_DELAY — panel still up, dwell cancelled
    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(400);
    check('return within the grace re-arms the dwell',
          resolves(w.sent).length === 1 && panel.classList.contains('is-open'),
          resolves(w.sent).length + ' resolves, panel=' + panel.className);
  }

  // --- the spinner is reserved for a slow daemon -----------------------------
  {
    const w = makeWorld(400); // reply lands ~400 ms after the request
    const btn = w.shadow.querySelector('.cl-btn');
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');

    // request out at ~150, spinner due at ~350, reply at ~550
    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(250);
    check('slow daemon: still plain before SPIN_DELAY',
          panel.classList.contains('is-open') && !btn.classList.contains('is-loading'),
          'btn=' + btn.className);
    await sleep(200);
    check('slow daemon: spinner after SPIN_DELAY in flight',
          btn.classList.contains('is-loading'), 'btn=' + btn.className);
    await sleep(250);
    check('slow daemon: reply lands, spinner gone, ready',
          panel.classList.contains('is-open') && !btn.classList.contains('is-loading'),
          'btn=' + btn.className);
  }

  console.log(fails ? `\n${fails} FAILURE(S)` : '\nall checks passed');
  process.exit(fails ? 1 : 0);
})();
