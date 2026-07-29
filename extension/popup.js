'use strict';

/*
 * codelink — popup logic. Reads and writes the one "settings" object; asks the
 * service worker for daemon health and for the active tab's repo identity. The
 * per-repo override key is "<host>/<owner>/<repo>" — host from the tab URL
 * (lowercased), owner and repo echoed verbatim from the daemon's /repostatus,
 * so this file never parses a provider's URL layout itself.
 */

const KICKSTART = 'launchctl kickstart -k gui/$(id -u)/com.qoob23.codelink';

const DEFAULTS = { paused: false, warnBadges: true, warnOverrides: {}, debug: false };

let settings = Object.assign({}, DEFAULTS);
let repoKey = null; // set once /repostatus names the active tab's repo

const $ = (id) => document.getElementById(id);

function send(msg) {
  return new Promise((resolve) => {
    try {
      chrome.runtime.sendMessage(msg, (reply) => {
        if (chrome.runtime.lastError) {
          resolve({ ok: false, error: chrome.runtime.lastError.message });
          return;
        }
        resolve(reply || { ok: false, error: 'empty reply' });
      });
    } catch (e) {
      resolve({ ok: false, error: String((e && e.message) || e) });
    }
  });
}

async function save() {
  // Whole-object writes on purpose: one storage key means one onChanged event,
  // and every reader re-derives its state from the full object.
  await chrome.storage.local.set({ settings });
}

function renderToggles() {
  $('paused').checked = settings.paused;
  $('warnBadges').checked = settings.warnBadges;
  $('debug').checked = settings.debug;
  if (repoKey) {
    const over = settings.warnOverrides[repoKey];
    $('repo-override').value = typeof over === 'boolean' ? (over ? 'on' : 'off') : 'default';
  }
}

async function initDaemon() {
  const reply = await send({ type: 'status' });
  const dot = $('daemon-dot');
  const text = $('daemon-text');
  const cmd = $('kickstart');
  if (reply.ok && reply.data) {
    dot.className = 'dot up';
    text.textContent = 'daemon ' + (reply.data.version || 'up') + ' · pid ' + reply.data.pid;
    cmd.hidden = true;
    return;
  }
  dot.className = 'dot down';
  text.textContent = 'daemon is not running';
  cmd.textContent = KICKSTART;
  cmd.hidden = false;
}

async function initRepoRow() {
  let tab = null;
  try {
    const tabs = await chrome.tabs.query({ active: true, currentWindow: true });
    tab = tabs && tabs[0];
  } catch (e) {
    tab = null;
  }
  if (!tab || !tab.url) return;

  let host = '';
  try {
    host = new URL(tab.url).hostname.toLowerCase();
  } catch (e) {
    return;
  }

  const reply = await send({ type: 'repostatus', url: tab.url });
  if (!reply.ok || !reply.data || !reply.data.repo) return; // not a repo page

  // Lowercased like content.js's overrideKey — the two must produce the same
  // key for the same repo or the tri-state below is silently inert.
  repoKey = host + '/' + String(reply.data.owner || '').toLowerCase() + '/' +
    String(reply.data.repo).toLowerCase();
  $('repo-name').textContent = 'badges for ' + host + '/' +
    (reply.data.owner ? reply.data.owner + '/' : '') + reply.data.repo;
  $('repo-row').hidden = false;
  renderToggles();
}

async function init() {
  try {
    const got = await chrome.storage.local.get('settings');
    if (got && got.settings && typeof got.settings === 'object') {
      settings = Object.assign({}, DEFAULTS, got.settings);
      if (typeof settings.warnOverrides !== 'object' || !settings.warnOverrides) {
        settings.warnOverrides = {};
      }
    }
  } catch (e) {
    /* defaults stand */
  }
  renderToggles();

  $('paused').addEventListener('change', (e) => {
    settings.paused = e.target.checked;
    save();
  });
  $('warnBadges').addEventListener('change', (e) => {
    settings.warnBadges = e.target.checked;
    save();
  });
  $('debug').addEventListener('change', (e) => {
    settings.debug = e.target.checked;
    save();
  });
  $('repo-override').addEventListener('change', (e) => {
    if (!repoKey) return;
    // "default" means no entry at all — an override equal to the global switch
    // would silently pin the old behaviour after the global is flipped.
    if (e.target.value === 'default') delete settings.warnOverrides[repoKey];
    else settings.warnOverrides[repoKey] = e.target.value === 'on';
    save();
  });
  $('kickstart').addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(KICKSTART);
      $('kickstart').textContent = 'copied to clipboard';
      setTimeout(() => ($('kickstart').textContent = KICKSTART), 1200);
    } catch (e) {
      /* leave the command visible for manual copy */
    }
  });

  initDaemon();
  initRepoRow();
}

init();
