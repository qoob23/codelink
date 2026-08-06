/*
 * The hover flow is VERDICT FIRST: the button's presence is itself the answer,
 * so nothing may paint before the daemon has given one. Pinned here:
 *
 *   - an unknown link paints NOTHING — no button, no spinner — through the
 *     dwell and until the reply lands; the icon appears for the first time with
 *     the verdict;
 *   - the daemon is asked only after the pointer dwells RESOLVE_DELAY on the
 *     link, so hopping across links (or leaving to empty space) sends nothing
 *     for the ones merely passed through, and paints nothing for them either;
 *   - a leave-and-return within the hide grace re-arms the dwell;
 *   - PER-PROJECT OPTIMISM: once a link in a project has resolved to something
 *     local, the next link in that project paints the ready button BEFORE its
 *     own verdict — and the verdict corrects it in place, turning the ready
 *     icon into the can't-resolve warning on FILE_NOT_LOCAL;
 *   - a click on that optimistic button IS the dwell: it resolves immediately
 *     and then performs the open it stood for, with no second interaction —
 *     the /open it triggers outliving the hide that the leaving pointer armed
 *     before the verdict landed — but that stored click DIES with the button it
 *     was made on, so a pointer that leaves before the verdict opens nothing,
 *     then or ever, a verdict of "not here" answers the click by painting the
 *     warning instead, and a failed one leaves nothing behind for the retry;
 *   - a slow daemon still shows no spinner, because there is no pre-verdict
 *     state left to spin.
 *
 * The numbers lean on RESOLVE_DELAY = 150 and HIDE_DELAY = 120: "before" probes
 * sit well inside a window, "after" probes well past it. Same hermetic fixture
 * rules as the siblings.
 */
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const SRC = process.env.CL_SRC || path.join(__dirname, '..', 'content.js');
const HOSTS_JS = `self.CODELINK_HOSTS = ["*.example.com"];`;

const PAGE_HOST = 'code.example.com';
// Same first two path segments, so both links belong to one project as far as
// projKey() is concerned — which is what the optimism blocks below exercise.
const URL_ONE = `https://${PAGE_HOST}/repo/widget/lib/one.go`;
const URL_TWO = `https://${PAGE_HOST}/repo/widget/lib/two.go`;

const RECT = { left: 100, top: 100, right: 200, bottom: 120, width: 100, height: 20, x: 100, y: 100 };

const ROOT = {
  root: '/Users/you/checkouts/widget', label: 'widget',
  localPath: '/y', recency: 1, hasOpenInstance: false,
};
const INSTANCE = {
  id: '4242', label: 'widget',
  root: '/Users/you/checkouts/widget',
  cwd: '/Users/you/checkouts/widget/lib',
  localPath: '/y', inProject: true, lastFocused: 100, focusable: 'heuristic',
};

function parsed(repoPath) {
  return {
    provider: 'example', owner: 'you', repo: 'widget', repoPath,
    line: null, endLine: null, refIsDefault: true,
    project: '', projectName: '', kind: 'file',
  };
}

// ok, with something local behind it: the verdict that paints the ready button
// and teaches the project.
function okReply(instances) {
  return {
    ok: true,
    data: {
      parsed: parsed('lib/one.go'),
      openInstances: instances || [],
      rootCandidates: [ROOT],
      warnings: [],
    },
  };
}

// ok-but-empty with a code and a named repo: the can't-resolve verdict.
function missingReply(code) {
  return {
    ok: true,
    data: {
      code,
      parsed: parsed('lib/two.go'),
      openInstances: [],
      rootCandidates: [],
      warnings: [],
    },
  };
}

// openDelayMs defaults to replyDelayMs; give it its own value to hold an /open
// round trip open across a hide that was armed before the click.
function makeWorld(replyDelayMs, replyFor, openDelayMs) {
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

  const reply = replyFor || (() => okReply());
  const openDelay = openDelayMs === undefined ? replyDelayMs : openDelayMs;
  const sent = [];
  window.chrome = {
    runtime: {
      id: 'test',
      getURL: (p) => 'chrome-extension://test/' + p,
      lastError: null,
      sendMessage(msg, cb) {
        sent.push(msg);
        const isResolve = msg.type === 'resolve';
        setTimeout(
          () => cb(isResolve ? reply(msg.url) : { ok: true, data: { ok: true } }),
          (isResolve ? replyDelayMs : openDelay) || 0
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
const opens = (sent) => sent.filter((m) => m.type === 'open');

(async () => {
  let fails = 0;
  const check = (name, cond, extra) => {
    console.log((cond ? 'PASS  ' : 'FAIL  ') + name + (cond ? '' : '   <- ' + extra));
    if (!cond) fails++;
  };

  // --- an unknown link paints nothing until the verdict is in ----------------
  {
    const w = makeWorld();
    const btn = w.shadow.querySelector('.cl-btn');
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');

    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(60);
    check('unknown project: nothing is painted during the dwell',
          !panel.classList.contains('is-open'), 'panel=' + panel.className);
    check('...and the daemon has not been asked yet',
          resolves(w.sent).length === 0, resolves(w.sent).length + ' resolves');

    await sleep(340);
    check('after the dwell: exactly one resolve', resolves(w.sent).length === 1,
          resolves(w.sent).length + ' resolves');
    check('...and the verdict is what puts the button up, ready',
          panel.classList.contains('is-open') &&
          !btn.classList.contains('is-loading') &&
          !btn.classList.contains('is-missing'),
          'panel=' + panel.className + ' btn=' + btn.className);

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

  // --- hopping sends nothing, and paints nothing, for the link passed through -
  {
    const w = makeWorld();
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');
    const lnk2 = w.window.document.getElementById('lnk2');

    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(60);
    check('the passed-through link painted nothing',
          !panel.classList.contains('is-open'), 'panel=' + panel.className);
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
    check('...and nothing was ever painted', !panel.classList.contains('is-open'), panel.className);

    // Return within the hide grace: the overlay still holds the anchor, so the
    // dwell must be re-armed or the link would never be asked about at all.
    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(50);
    w.user(lnk1, new w.window.MouseEvent('mouseout', { bubbles: true }));
    await sleep(60); // inside HIDE_DELAY — anchor still held, dwell cancelled
    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(400);
    check('return within the grace re-arms the dwell',
          resolves(w.sent).length === 1 && panel.classList.contains('is-open'),
          resolves(w.sent).length + ' resolves, panel=' + panel.className);
  }

  // --- project optimism, and the verdict correcting it -----------------------
  {
    const w = makeWorld(0, (url) =>
      url === URL_ONE ? okReply() : missingReply('FILE_NOT_LOCAL')
    );
    const btn = w.shadow.querySelector('.cl-btn');
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');
    const lnk2 = w.window.document.getElementById('lnk2');

    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(400);
    check('the first link in the project resolved ready',
          panel.classList.contains('is-open') && !btn.classList.contains('is-missing'),
          'panel=' + panel.className + ' btn=' + btn.className);

    w.user(lnk1, new w.window.MouseEvent('mouseout', { bubbles: true }));
    w.user(lnk2, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 140 }));
    await sleep(30); // well inside the dwell: this link's own verdict cannot be in
    check('a known-local project paints the ready button before its own verdict',
          panel.classList.contains('is-open') &&
          !btn.classList.contains('is-missing') &&
          !btn.classList.contains('is-loading'),
          'panel=' + panel.className + ' btn=' + btn.className);
    check('...optimistically: the second link has not been resolved yet',
          resolves(w.sent).length === 1, JSON.stringify(resolves(w.sent).map((m) => m.url)));

    await sleep(400);
    check('FILE_NOT_LOCAL turns the optimistic icon into the warning',
          panel.classList.contains('is-open') && btn.classList.contains('is-missing'),
          'panel=' + panel.className + ' btn=' + btn.className);
    check('...having asked the daemon exactly once for it',
          resolves(w.sent).length === 2, resolves(w.sent).length + ' resolves');
  }

  // --- a click on the optimistic button IS the dwell -------------------------
  {
    // Slow daemon, so the second link's verdict is provably still in flight at
    // the moment of the click.
    const w = makeWorld(400, (url) => okReply(url === URL_TWO ? [INSTANCE] : []));
    const btn = w.shadow.querySelector('.cl-btn');
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');
    const lnk2 = w.window.document.getElementById('lnk2');

    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(700); // dwell 150 + reply 400, with room to spare
    check('the project is known local', panel.classList.contains('is-open'), panel.className);

    w.user(lnk1, new w.window.MouseEvent('mouseout', { bubbles: true }));
    w.user(lnk2, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 140 }));
    await sleep(30);
    check('the optimistic button is up with its verdict still owed',
          panel.classList.contains('is-open') && resolves(w.sent).length === 1,
          'panel=' + panel.className + ', ' + resolves(w.sent).length + ' resolves');

    w.user(btn, new w.window.MouseEvent('click', { bubbles: true, composed: true }));
    await sleep(30);
    check('the click asks the daemon at once, without waiting out the dwell',
          resolves(w.sent).length === 2 && resolves(w.sent)[1].url === URL_TWO,
          JSON.stringify(resolves(w.sent).map((m) => m.url)));
    check('...and opens nothing yet', opens(w.sent).length === 0, opens(w.sent).length + ' opens');

    await sleep(500);
    const open = opens(w.sent).pop();
    check('the verdict lands and the click it stood for is performed',
          opens(w.sent).length === 1, opens(w.sent).length + ' opens');
    check('...reusing the one running instance',
          open && open.payload.mode === 'existing' && open.payload.target === '4242',
          JSON.stringify(open && open.payload));
  }

  // --- the /open outlives a hide armed before the click's verdict ------------
  {
    /*
     * The pointer leaves while the resolve is still out, so a hide is already
     * ticking when the verdict lands and replays the click. The /open round trip
     * has to survive it: hideNow() bumps the sequence number, and a discarded
     * /open reply means no checkmark on success and no word at all on failure.
     * Fast resolve, slow open, so the ordering is settled by the fixture rather
     * than by a race — verdict at ~T+25, hide due at ~T+130, /open reply ~T+430.
     */
    const w = makeWorld(0, (url) => okReply(url === URL_TWO ? [INSTANCE] : []), 400);
    const btn = w.shadow.querySelector('.cl-btn');
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');
    const lnk2 = w.window.document.getElementById('lnk2');

    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(250);
    w.user(lnk1, new w.window.MouseEvent('mouseout', { bubbles: true }));
    w.user(lnk2, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 140 }));
    await sleep(20);
    w.user(lnk2, new w.window.MouseEvent('mouseout', { bubbles: true })); // hide armed
    await sleep(10);
    w.user(btn, new w.window.MouseEvent('click', { bubbles: true, composed: true }));
    await sleep(60);
    check('the replayed click sent its open', opens(w.sent).length === 1,
          opens(w.sent).length + ' opens');

    await sleep(500); // past both the armed hide and the /open reply
    check('the open round trip outlived the armed hide, and reported',
          panel.classList.contains('is-open') && btn.classList.contains('is-ok'),
          'panel=' + panel.className + ' btn=' + btn.className);
  }

  // --- the intent dies with the button it was made on ------------------------
  {
    // Same shape as the block above, but the pointer leaves before the verdict
    // it is waiting for arrives.
    const w = makeWorld(400, (url) => okReply(url === URL_TWO ? [INSTANCE] : []));
    const btn = w.shadow.querySelector('.cl-btn');
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');
    const lnk2 = w.window.document.getElementById('lnk2');

    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(700);
    w.user(lnk1, new w.window.MouseEvent('mouseout', { bubbles: true }));
    w.user(lnk2, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 140 }));
    await sleep(30);
    w.user(btn, new w.window.MouseEvent('click', { bubbles: true, composed: true }));
    await sleep(30);
    check('the click went out with the verdict still in flight',
          resolves(w.sent).length === 2 && opens(w.sent).length === 0,
          resolves(w.sent).length + ' resolves, ' + opens(w.sent).length + ' opens');

    // Leave, and let the hide grace expire well before the reply is due.
    w.user(lnk2, new w.window.MouseEvent('mouseout', { bubbles: true }));
    await sleep(600); // 120 hide grace, then the ~430 reply into a torn-down overlay
    check('a click abandoned before its verdict opens nothing',
          opens(w.sent).length === 0, opens(w.sent).length + ' opens');
    check('...and the overlay stayed down', !panel.classList.contains('is-open'), panel.className);

    // The dangerous half: the dead intent must not be lying in wait for the
    // NEXT verdict on that link — coming back and merely hovering must put the
    // button up and stop there, waiting for a click the user has yet to make.
    w.user(lnk2, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 140 }));
    await sleep(700);
    check('a later verdict does not inherit the abandoned click',
          opens(w.sent).length === 0, opens(w.sent).length + ' opens');
    check('...the re-hover just paints the button again',
          panel.classList.contains('is-open') && !btn.classList.contains('is-missing'),
          'panel=' + panel.className + ' btn=' + btn.className);
  }

  // --- a missing verdict IS the answer to the click --------------------------
  {
    const w = makeWorld(400, (url) =>
      url === URL_ONE ? okReply([INSTANCE]) : missingReply('FILE_NOT_LOCAL')
    );
    const btn = w.shadow.querySelector('.cl-btn');
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');
    const lnk2 = w.window.document.getElementById('lnk2');

    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(700);
    w.user(lnk1, new w.window.MouseEvent('mouseout', { bubbles: true }));
    w.user(lnk2, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 140 }));
    await sleep(30);
    w.user(btn, new w.window.MouseEvent('click', { bubbles: true, composed: true }));
    await sleep(600);
    check('a click answered by FILE_NOT_LOCAL opens nothing',
          opens(w.sent).length === 0, opens(w.sent).length + ' opens');
    check('...the warning that paints is the answer',
          panel.classList.contains('is-open') && btn.classList.contains('is-missing'),
          'panel=' + panel.className + ' btn=' + btn.className);

    // And it stays the answer: the warning is inert, so clicking it again is not
    // a second chance for the intent to be honoured.
    w.user(btn, new w.window.MouseEvent('click', { bubbles: true, composed: true }));
    await sleep(200);
    check('clicking the warning still opens nothing',
          opens(w.sent).length === 0 && btn.classList.contains('is-missing'),
          opens(w.sent).length + ' opens, btn=' + btn.className);
  }

  // --- a failed verdict must not leave the click lying in wait ---------------
  {
    /*
     * The sharp edge of the same machinery. The click's intent is claimed once,
     * whatever the verdict, so a verdict that FAILS drops it — and the retry the
     * user then asks for (clicking the error button is a retry, not an open)
     * must come back to a painted button and stop there. Claim the intent in the
     * ok branch alone and this block sends an /open nobody asked for.
     */
    let attempt = 0;
    const w = makeWorld(400, (url) => {
      if (url === URL_ONE) return okReply();
      attempt++;
      return attempt === 1
        ? { ok: false, kind: 'http', error: 'boom', status: 500 }
        : okReply([INSTANCE]);
    });
    const btn = w.shadow.querySelector('.cl-btn');
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');
    const lnk2 = w.window.document.getElementById('lnk2');

    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(700);
    w.user(lnk1, new w.window.MouseEvent('mouseout', { bubbles: true }));
    w.user(lnk2, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 140 }));
    await sleep(30);
    w.user(btn, new w.window.MouseEvent('click', { bubbles: true, composed: true }));
    await sleep(600);
    check('a click answered by an error paints the error, opens nothing',
          btn.classList.contains('is-down') && opens(w.sent).length === 0,
          'btn=' + btn.className + ', ' + opens(w.sent).length + ' opens');

    // Clicking the error button retries the resolve. The answer is now ok with a
    // running instance — and must still just paint the button.
    w.user(btn, new w.window.MouseEvent('click', { bubbles: true, composed: true }));
    await sleep(600);
    check('the retry is a retry: its verdict opens nothing on its own',
          opens(w.sent).length === 0, opens(w.sent).length + ' opens');
    check('...it repaints the ready button and waits for a real click',
          panel.classList.contains('is-open') && !btn.classList.contains('is-down'),
          'panel=' + panel.className + ' btn=' + btn.className);

    // ...and that button, clicked for real, still works.
    w.user(btn, new w.window.MouseEvent('click', { bubbles: true, composed: true }));
    await sleep(100);
    check('a real click on it opens the running instance',
          opens(w.sent).length === 1 && opens(w.sent)[0].payload.target === '4242',
          JSON.stringify(opens(w.sent).map((m) => m.payload)));
  }

  // --- a slow daemon has no pre-verdict state left to spin -------------------
  {
    const w = makeWorld(400); // reply lands ~400 ms after the request
    const btn = w.shadow.querySelector('.cl-btn');
    const panel = w.shadow.getElementById('panel');
    const lnk1 = w.window.document.getElementById('lnk1');

    // request out at ~150, reply at ~550
    w.user(lnk1, new w.window.MouseEvent('mouseover', { bubbles: true, clientX: 120 }));
    await sleep(250);
    check('slow daemon: still nothing painted just after the request went out',
          !panel.classList.contains('is-open') && !btn.classList.contains('is-loading'),
          'panel=' + panel.className + ' btn=' + btn.className);
    await sleep(200);
    check('slow daemon: still nothing painted deep into the round trip',
          !panel.classList.contains('is-open') && !btn.classList.contains('is-loading'),
          'panel=' + panel.className + ' btn=' + btn.className);
    await sleep(250);
    check('slow daemon: the reply is what finally paints, ready and unspun',
          panel.classList.contains('is-open') && !btn.classList.contains('is-loading'),
          'panel=' + panel.className + ' btn=' + btn.className);
  }

  console.log(fails ? `\n${fails} FAILURE(S)` : '\nall checks passed');
  process.exit(fails ? 1 : 0);
})();
