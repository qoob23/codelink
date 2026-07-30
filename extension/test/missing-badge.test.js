/*
 * The can't-resolve badge. A resolve that comes back ok-but-empty WITH a
 * REPO_NOT_LOCAL / FILE_NOT_LOCAL code and a named repo paints a dimmed, inert
 * button with a red cross instead of the usual silence — unless the popup's
 * settings say otherwise. Guarded here:
 *
 *   - both codes render the 'missing' state, and clicking it opens nothing;
 *   - warnBadges:false suppresses it, but the verdict is still cached (one
 *     resolve, however often the link is re-hovered);
 *   - a per-repo override ("<host>/<owner>/<repo>": true) beats the global off;
 *   - flipping the global off through storage.onChanged re-gates a link this
 *     tab has already seen, without a new resolve.
 *
 * Same hermetic fixture rules as the sibling tests: synthetic host, injected
 * host list, no site knowledge.
 */
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const SRC = process.env.CL_SRC || path.join(__dirname, '..', 'content.js');
const HOSTS_JS = `self.CODELINK_HOSTS = ["*.example.com"];`;

const PAGE_HOST = 'code.example.com';
const URL_REPO = `https://${PAGE_HOST}/repo/widget/lib/main.go`;
const URL_FILE = `https://${PAGE_HOST}/repo/widget/lib/other.go`;

const RECT = { left: 100, top: 100, right: 200, bottom: 120, width: 100, height: 20, x: 100, y: 100 };

function verdict(code) {
  return {
    ok: true,
    data: {
      code,
      parsed: {
        provider: 'example',
        owner: 'you',
        repo: 'widget',
        repoPath: 'lib/main.go',
        line: null, endLine: null, refIsDefault: true,
        project: '', projectName: '', kind: 'file',
      },
      openInstances: [],
      rootCandidates: [],
      warnings: [],
    },
  };
}

function makeWorld(settings) {
  const dom = new JSDOM(
    `<!doctype html><html><body>
       <a id="lnk1" href="${URL_REPO}">one</a>
       <a id="lnk2" href="${URL_FILE}">two</a>
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
  let onChanged = null;
  let onRuntimeMessage = null;
  window.chrome = {
    runtime: {
      id: 'test',
      getURL: (p) => 'chrome-extension://test/' + p,
      lastError: null,
      sendMessage(msg, cb) {
        sent.push(msg);
        setTimeout(() => {
          if (msg.type !== 'resolve') return cb({ ok: true, data: { ok: true } });
          cb(verdict(msg.url === URL_FILE ? 'FILE_NOT_LOCAL' : 'REPO_NOT_LOCAL'));
        }, 0);
      },
      onMessage: {
        addListener(fn) {
          onRuntimeMessage = fn;
        },
      },
    },
    storage: {
      local: {
        get(key, cb) {
          cb({ settings: settings || {} });
        },
      },
      onChanged: {
        addListener(fn) {
          onChanged = fn;
        },
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

  return {
    window,
    shadow: capturedRoot,
    sent,
    user,
    fireSettings: (s) => onChanged && onChanged({ settings: { newValue: s } }, 'local'),
    askRepoInfo: () => {
      let out = null;
      if (onRuntimeMessage) onRuntimeMessage({ type: 'repo-info' }, {}, (r) => (out = r));
      return out;
    },
  };
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const resolves = (sent) => sent.filter((m) => m.type === 'resolve').length;
const opens = (sent) => sent.filter((m) => m.type === 'open').length;

(async () => {
  let fails = 0;
  const check = (name, cond, extra) => {
    console.log((cond ? 'PASS  ' : 'FAIL  ') + name + (cond ? '' : '   <- ' + extra));
    if (!cond) fails++;
  };

  // --- defaults: both codes paint, the button is inert ----------------------
  {
    const w = makeWorld(null);
    const btn = w.shadow.querySelector('.cl-btn');
    const panel = w.shadow.getElementById('panel');

    w.user(w.window.document.getElementById('lnk1'),
           new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(400);
    check('REPO_NOT_LOCAL paints the missing state',
          panel.classList.contains('is-open') && btn.classList.contains('is-missing'),
          'panel=' + panel.className + ' btn=' + btn.className);
    check('missing aria names the repo',
          (btn.getAttribute('aria-label') || '').indexOf('widget') >= 0,
          btn.getAttribute('aria-label'));

    btn.dispatchEvent(new w.window.MouseEvent('click', { bubbles: true }));
    await sleep(100);
    check('clicking the missing button opens nothing', opens(w.sent) === 0, opens(w.sent) + ' opens');

    // The popup's per-repo row is fed from here: the last verdict that named a
    // repo, keyed exactly like warnOverrides.
    const info = w.askRepoInfo();
    check("repo-info answers the popup with the page's repo",
          info && info.key === 'code.example.com/you/widget' &&
          info.label === 'code.example.com/you/widget',
          JSON.stringify(info));

    w.user(w.window.document.getElementById('lnk1'),
           new w.window.MouseEvent('mouseout', { bubbles: true }));
    await sleep(250);
    w.user(w.window.document.getElementById('lnk2'),
           new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(400);
    check('FILE_NOT_LOCAL paints the missing state too',
          panel.classList.contains('is-open') && btn.classList.contains('is-missing'),
          'panel=' + panel.className + ' btn=' + btn.className);

    // Flip the global off through onChanged: the cached verdict for lnk1 must
    // re-gate on the next hover, with no new resolve.
    const before = resolves(w.sent);
    w.user(w.window.document.getElementById('lnk2'),
           new w.window.MouseEvent('mouseout', { bubbles: true }));
    await sleep(250);
    w.fireSettings({ warnBadges: false });
    w.user(w.window.document.getElementById('lnk1'),
           new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(400);
    check('global off re-gates an already-seen link',
          !panel.classList.contains('is-open'), 'panel=' + panel.className);
    check('...without a new resolve', resolves(w.sent) === before, resolves(w.sent) + ' vs ' + before);
  }

  // --- warnBadges:false from startup: silent, but the verdict is cached -----
  {
    const w = makeWorld({ warnBadges: false });
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');

    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(400);
    check('global off: nothing paints', !panel.classList.contains('is-open'), panel.className);

    w.user(lnk1, new w.window.MouseEvent('mouseout', { bubbles: true }));
    await sleep(250);
    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(400);
    check('the hidden verdict was still cached', resolves(w.sent) === 1, resolves(w.sent) + ' resolves');
  }

  // --- per-repo override beats the global off -------------------------------
  {
    const w = makeWorld({
      warnBadges: false,
      warnOverrides: { [`${PAGE_HOST}/you/widget`]: true },
    });
    const btn = w.shadow.querySelector('.cl-btn');
    const panel = w.shadow.getElementById('panel');

    w.user(w.window.document.getElementById('lnk1'),
           new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(400);
    check('override on beats global off',
          panel.classList.contains('is-open') && btn.classList.contains('is-missing'),
          'panel=' + panel.className + ' btn=' + btn.className);
  }

  // --- ...and the other way: silence one noisy repo -------------------------
  {
    const w = makeWorld({
      warnBadges: true,
      warnOverrides: { [`${PAGE_HOST}/you/widget`]: false },
    });
    const panel = w.shadow.getElementById('panel');

    w.user(w.window.document.getElementById('lnk1'),
           new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(400);
    check('override off beats global on', !panel.classList.contains('is-open'), panel.className);
  }

  // --- paused stops triage before it starts ---------------------------------
  {
    const w = makeWorld({ paused: true });
    const panel = w.shadow.getElementById('panel');

    w.user(w.window.document.getElementById('lnk1'),
           new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(400);
    check('paused: nothing paints', !panel.classList.contains('is-open'), panel.className);
    check('paused: the daemon is never asked', resolves(w.sent) === 0, resolves(w.sent) + ' resolves');
  }

  console.log(fails ? `\n${fails} FAILURE(S)` : '\nall checks passed');
  process.exit(fails ? 1 : 0);
})();
