const { test } = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');

function harness(options = {}) {
  const nodes = new Map();
  const node = id => {
    if (!nodes.has(id)) nodes.set(id, { checked: true });
    return nodes.get(id);
  };
  const listeners = new Map();
  const calls = [];
  const sent = [];
  let tick, now = 0, openedURL, serial = 0, saved = 0;
  const popup = { closed: false, close() { this.closed = true; }, postMessage(...args) { sent.push(args); } };
  const window = {
    isSecureContext: true, crossOriginIsolated: false,
    addEventListener: (name, fn) => listeners.set(name, fn),
    removeEventListener: name => listeners.delete(name),
    open(url) { openedURL = url; return options.blocked ? null : popup; },
  };
  const context = vm.createContext({
    window, document: { getElementById: node }, AbortController,
    crypto: { randomUUID: () => `nonce-${++serial}` }, Date: { now: () => now },
    setInterval: fn => { tick = fn; return 1; }, clearInterval: () => { tick = null; },
    fetch: async (url, request) => { calls.push({ url, request }); return { ok: !options.fail, json: async () => ({ status: 'complete', account_id: 1 }) }; },
    closeModal() {}, loadAccounts() { saved++; }, showToast() {},
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js/puter-auth.js'), 'utf8'), context);
  const event = overrides => ({ origin: 'https://puter.com', source: popup, data: { msg: 'puter.token', msg_id: `nonce-${serial}`, success: true, token: 'grant-secret' }, ...overrides });
  return { context, window, node, popup, listeners, calls, sent, event,
    get saved() { return saved; }, get url() { return openedURL; },
    async emit(e) { await listeners.get('message')?.(e); },
    advance(ms) { now += ms; tick?.(); },
  };
}

test('Puter popup validates origin, source, nonce and saves a valid grant once', async () => {
  const h = harness();
  h.context.PuterWebLogin.start();
  assert.match(h.url, /^https:\/\/puter.com\/action\/sign-in\?/);
  assert.equal(new URL(h.url).searchParams.get('request_auth'), 'true');
  await h.emit(h.event({ origin: 'https://evil.example' }));
  await h.emit(h.event({ source: {} }));
  await h.emit(h.event({ data: { msg: 'puter.token', msg_id: 'wrong', success: true, token: 'secret' } }));
  assert.equal(h.calls.length, 0);
  await h.emit(h.event({ data: { msg: 'requestOrigin' } }));
  assert.equal(h.sent.length, 1);
  assert.equal(h.sent[0][1], 'https://puter.com');
  await h.emit(h.event());
  await h.emit(h.event());
  assert.equal(h.calls.length, 1);
  assert.equal(h.calls[0].url, '/api/puter/web-login');
  assert.deepEqual(JSON.parse(h.calls[0].request.body), { token: 'grant-secret', enabled: true });
  assert.equal(h.saved, 1);
  assert.equal(h.popup.closed, true);
  assert.equal(h.listeners.has('message'), false);
  assert.equal(h.node('puterWebLoginStatus').textContent.includes('grant-secret'), false);
});

test('Puter login cancellation removes the listener and rejects late grants', async () => {
  const h = harness();
  h.context.PuterWebLogin.start();
  const listener = h.listeners.get('message');
  const late = h.event();
  h.context.PuterWebLogin.stop();
  await listener(late);
  assert.equal(h.calls.length, 0);
  assert.equal(h.popup.closed, true);
  assert.equal(h.node('puterWebLoginButton').disabled, false);
});

test('Puter blocked, insecure, isolated, closed and expired popups fail cleanly', () => {
  const blocked = harness({ blocked: true });
  blocked.context.PuterWebLogin.start();
  assert.match(blocked.node('puterWebLoginStatus').textContent, /拦截/);
  assert.equal(blocked.listeners.has('message'), false);
  for (const flag of ['isSecureContext', 'crossOriginIsolated']) {
    const h = harness();
    h.window[flag] = flag === 'crossOriginIsolated';
    h.context.PuterWebLogin.start();
    assert.equal(h.url, undefined);
  }
  const expired = harness();
  expired.context.PuterWebLogin.start();
  expired.advance(300001);
  assert.match(expired.node('puterWebLoginStatus').textContent, /超时/);
  const closed = harness();
  closed.context.PuterWebLogin.start();
  closed.popup.closed = true;
  closed.advance(250);
  closed.advance(1000);
  assert.match(closed.node('puterWebLoginStatus').textContent, /关闭/);
});

test('Puter backend rejection never reports a saved account', async () => {
  const h = harness({ fail: true });
  h.context.PuterWebLogin.start();
  await h.emit(h.event());
  assert.equal(h.saved, 0);
  assert.match(h.node('puterWebLoginStatus').textContent, /失败/);
  assert.equal(h.listeners.has('message'), false);
});
