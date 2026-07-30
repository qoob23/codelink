'use strict';

/*
 * codelink — popup logic. Reads and writes the one "settings" object; asks the
 * service worker for daemon health, and the active tab's content script for
 * which repo its page is about. The per-repo override key is
 * "<host>/<owner>/<repo>", lowercased — built by the content script's
 * overrideKey and returned here verbatim, so the two sides cannot drift and
 * this file never parses a provider's URL layout itself.
 */

const DEFAULTS = { paused: false, warnBadges: true, warnOverrides: {}, debug: false };

let settings = Object.assign({}, DEFAULTS);
let repoKey = null; // set once the content script names the active tab's repo

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

// Ask the active tab's content script. Distinct from send(): tab messaging has
// its own addressing, and "no receiver" (a tab with no content script — the
// new-tab page, chrome://) is an ordinary outcome, not an error.
function askTab(msg) {
  return new Promise((resolve) => {
    chrome.tabs.query({ active: true, currentWindow: true }, (tabs) => {
      const tab = tabs && tabs[0];
      if (!tab || tab.id == null) return resolve(null);
      try {
        chrome.tabs.sendMessage(tab.id, msg, (reply) => {
          if (chrome.runtime.lastError) return resolve(null);
          resolve(reply || null);
        });
      } catch (e) {
        resolve(null);
      }
    });
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
  if (reply.ok && reply.data) {
    dot.className = 'dot up';
    text.textContent = 'daemon ' + (reply.data.version || 'up') + ' · pid ' + reply.data.pid;
    return;
  }
  dot.className = 'dot down';
  text.textContent = 'daemon is not running';
}

async function initRepoRow() {
  // The content script remembers the last verdict that named a repo, so the
  // row appears only once a repo link on this tab has been hovered. Before
  // that — or on a page with no content script at all — there is simply no
  // repo to talk about, and the row stays hidden.
  const reply = await askTab({ type: 'repo-info' });
  if (!reply || !reply.key) return;

  repoKey = reply.key;
  $('repo-name').textContent = 'badges for ' + (reply.label || reply.key);
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

  initDaemon();
  initRepoRow();
}

init();
