'use strict';

/*
 * codelink — MV3 service worker.
 *
 * Sole reason this file exists: the content script must never call fetch().
 * A content script's requests are subject to the *page's* CSP `connect-src`,
 * so a fetch to 127.0.0.1 from a page whose `connect-src` does not allow it
 * would be blocked outright — and a code-hosting site's CSP never will.
 * The service worker is exempt from page CSP and carries the
 * `host_permissions` entry for the daemon, so all HTTP goes through here.
 *
 * Content script -> chrome.runtime.sendMessage -> here -> fetch -> daemon.
 */

const DAEMON = 'http://127.0.0.1:47391';

/*
 * token.gen.js is written by `codelink serve`, so on a fresh checkout (or
 * before the daemon has ever run) it simply does not exist. importScripts()
 * throws in that case; swallowing it keeps the worker installable and lets us
 * degrade into a "daemon isn't running" state instead of an install failure.
 */
let TOKEN = null;
let TOKEN_ERROR = null;
try {
  importScripts('token.gen.js');
  TOKEN = (typeof self.CODELINK_TOKEN === 'string' && self.CODELINK_TOKEN) || null;
  if (!TOKEN) {
    TOKEN_ERROR = 'token.gen.js loaded but CODELINK_TOKEN is empty';
  }
} catch (e) {
  TOKEN_ERROR = 'token.gen.js is missing — the codelink daemon has never run';
}

/*
 * importScripts() may only be called during initial worker evaluation, so it
 * cannot be retried later. When the token was missing at startup, fall back to
 * reading the packaged file over fetch(). Fetching our own extension resource
 * needs no web_accessible_resources entry (that is only for page contexts) and
 * is not subject to page CSP here.
 */
let tokenProbe = null;

async function getToken() {
  if (TOKEN) return TOKEN;
  if (!tokenProbe) {
    tokenProbe = (async () => {
      try {
        // Chromium serves unpacked-extension resources out of its package
        // cache, so a plain fetch here happily returns the *stale* token
        // forever after a rotation. Cache-bust the URL and force revalidation;
        // without both, a rotated token wedges into a permanent 403 loop.
        const url = chrome.runtime.getURL('token.gen.js') + '?rev=' + Date.now();
        const res = await fetch(url, { cache: 'reload' });
        if (!res.ok) return null;
        const src = await res.text();
        const m = src.match(/CODELINK_TOKEN\s*=\s*(['"])([^'"]+)\1/);
        return m ? m[2] : null;
      } catch (e) {
        return null;
      }
    })();
  }
  const token = await tokenProbe;
  if (token) {
    TOKEN = token;
    TOKEN_ERROR = null;
  } else {
    tokenProbe = null; // let a later request try again
  }
  return token;
}

const STALE_TOKEN_MSG =
  'codelink rejected this extension’s token (it was rotated). ' +
  'Open your extensions page and hit Reload on the codelink card.';

function forgetToken() {
  TOKEN = null;
  tokenProbe = null;
  TOKEN_ERROR = STALE_TOKEN_MSG;
}

/**
 * Perform one daemon call and normalise every possible outcome into the reply
 * shape the content script understands:
 *   { ok: true,  data }
 *   { ok: false, kind: 'daemon-down' | 'http' | 'api', error, status?, code? }
 */
async function call(path, init) {
  const token = await getToken();
  if (!token) {
    return {
      ok: false,
      kind: 'daemon-down',
      error: TOKEN_ERROR || 'no codelink token available',
    };
  }

  const headers = Object.assign({}, (init && init.headers) || null, {
    'X-Codelink-Client': 'ext',
    'X-Codelink-Token': token,
  });

  let res;
  try {
    res = await fetch(DAEMON + path, {
      method: (init && init.method) || 'GET',
      body: (init && init.body) || undefined,
      headers: headers,
      cache: 'no-store',
      credentials: 'omit',
      redirect: 'follow',
    });
  } catch (e) {
    // A rejected fetch (TypeError: Failed to fetch) means nothing is listening
    // on the port — i.e. the daemon is down. This is the only network-level
    // failure mode we can get against a loopback address.
    return {
      ok: false,
      kind: 'daemon-down',
      error: (e && e.message) || String(e),
    };
  }

  let text = '';
  try {
    text = await res.text();
  } catch (e) {
    text = '';
  }

  let body = null;
  let isJson = false;
  if (text) {
    try {
      body = JSON.parse(text);
      isJson = true;
    } catch (e) {
      isJson = false;
    }
  }

  /*
   * Auth failures get their own kind, returned before the generic paths.
   * Two reasons: the daemon answers 403 with a plain-text "forbidden" body, so
   * the generic path would surface that instead of the actionable "reload the
   * extension" guidance; and a 403 carrying a JSON body would otherwise land in
   * kind 'api' with a 4xx status, which the content script silently caches as a
   * skip — a wedged token would look exactly like an unsupported link.
   */
  if (res.status === 401 || res.status === 403) {
    forgetToken();
    return { ok: false, kind: 'auth', status: res.status, error: STALE_TOKEN_MSG };
  }

  if (!res.ok) {
    if (isJson && body && typeof body === 'object') {
      return {
        ok: false,
        kind: 'api',
        status: res.status,
        code: body.code,
        error: body.error || body.message || 'HTTP ' + res.status,
        data: body,
      };
    }
    return {
      ok: false,
      kind: 'http',
      status: res.status,
      error: text.slice(0, 300) || 'HTTP ' + res.status,
    };
  }

  if (!isJson) {
    return {
      ok: false,
      kind: 'http',
      status: res.status,
      error: 'daemon returned a non-JSON body',
    };
  }

  // The daemon may also report a logical failure inside a 200.
  if (body && typeof body === 'object' && body.ok === false) {
    return {
      ok: false,
      kind: 'api',
      status: res.status,
      code: body.code,
      error: body.error || body.message || 'request failed',
      data: body,
    };
  }

  return { ok: true, data: body };
}

function resolve(url) {
  return call('/resolve?url=' + encodeURIComponent(url), { method: 'GET' });
}

function open(payload) {
  return call('/open', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload || {}),
  });
}

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (!msg || typeof msg !== 'object') return false;

  if (msg.type === 'resolve') {
    if (typeof msg.url !== 'string' || !msg.url) {
      sendResponse({ ok: false, kind: 'api', error: 'resolve: missing url' });
      return false;
    }
    resolve(msg.url).then(sendResponse);
    return true; // keep the message channel open for the async reply
  }

  if (msg.type === 'open') {
    open(msg.payload).then(sendResponse);
    return true; // keep the message channel open for the async reply
  }

  if (msg.type === 'ping') {
    getToken().then((token) => {
      sendResponse({ ok: true, data: { hasToken: Boolean(token), tokenError: TOKEN_ERROR } });
    });
    return true; // keep the message channel open for the async reply
  }

  // Popup-only message. The popup does no fetches of its own — everything the
  // daemon answers flows through here, same as for the content script.
  if (msg.type === 'status') {
    call('/health', { method: 'GET' }).then(sendResponse);
    return true; // keep the message channel open for the async reply
  }

  return false;
});
