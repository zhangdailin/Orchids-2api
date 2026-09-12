// Run with: node --test web/accounts_ui.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadUI() {
  const timers = [];
  // Minimal element/DOM surface: enough for tab rendering and modal wiring.
  const makeElement = (tag) => {
    const classes = new Set();
    const children = [];
    const listeners = {};
    return {
      tagName: tag,
      value: '',
      hidden: false,
      required: false,
      disabled: false,
      checked: false,
      textContent: '',
      innerHTML: '',
      style: {},
      dataset: {},
      children,
      reset() {},
      addEventListener(type, fn) { (listeners[type] ||= []).push(fn); },
      click() { (listeners.click || []).forEach((fn) => fn({ target: this })); },
      appendChild(child) { children.push(child); return child; },
      classList: {
        add: (name) => classes.add(name),
        remove: (name) => classes.delete(name),
        toggle: (name, on) => (on ? classes.add(name) : classes.delete(name)),
        contains: (name) => classes.has(name),
      },
    };
  };
  const elements = new Map();
  const node = (id) => {
    if (!elements.has(id)) elements.set(id, makeElement('div'));
    return elements.get(id);
  };
  const context = vm.createContext({
    document: {
      getElementById: node,
      querySelector: node,
      createElement: makeElement,
      querySelectorAll: () => [],
      addEventListener() {},
    },
    window: {
      setInterval: (fn) => { timers.push(fn); return timers.length; },
      clearInterval: () => {},
      addEventListener() {},
    },
  });
  for (const file of ['common.js', 'accounts.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js', file), 'utf8'), context);
  }
  return { context, node };
}

function workBuddyAccount(overrides = {}) {
  return {
    id: 11,
    account_type: 'workbuddy',
    workbuddy_access_token: 'eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1aWQifQ.sig',
    workbuddy_uid: '07ab88c8-5596-4257-8d21-e9fcbe3a3810',
    enabled: true,
    weight: 1,
    ...overrides,
  };
}

test('Warp exposes only official login and preserves settings editing', () => {
  const { context, node } = loadUI();
  node('credentialType').value = 'oauth';
  context.applyTokenLabels('grok');
  assert.equal(node('oauthCredentialGroup').hidden, false);
  node('clientCookie').value = 'old-input';
  context.applyTokenLabels('warp');
  for (const id of ['ssoCredentialGroup', 'oauthCredentialGroup', 'oauthRefreshGroup', 'oauthExpiresGroup', 'grokDeviceLoginGroup', 'grokProviderGroup']) {
    assert.equal(node(id).hidden, true, id);
  }
  assert.equal(node('warpDeviceLoginGroup').hidden, false);
  assert.equal(node('clientCookie').required, false);
  assert.equal(node('clientCookie').value, '');
  assert.equal(node('#accountForm button[type="submit"]').hidden, true);
  node('accountId').value = '1';
  context.applyTokenLabels('warp');
  assert.equal(node('#accountForm button[type="submit"]').hidden, false);
  assert.equal(node('warpDeviceLoginGroup').hidden, true);
  const payload = context.buildAccountPayload('warp', { account_type: 'warp', enabled: true }, 'unused-secret');
  assert.equal(payload.refresh_token, undefined);
  assert.equal(payload.client_cookie, undefined);
  assert.equal(payload.enabled, true);
  context.applyTokenLabels('puter');
  node('accountId').value = '';
  context.applyTokenLabels('puter');
  assert.equal(node('puterWebLoginGroup').hidden, false);
  assert.equal(node('ssoCredentialGroup').hidden, false);
  assert.equal(node('clientCookie').required, true);
});

test('Warp credential presence and display do not read raw tokens', () => {
  const { context } = loadUI();
  const acc = { account_type: 'warp', warp_authenticated: true };
  assert.equal(context.hasSidebarAccountCredential(acc), true);
  assert.equal(context.getSidebarAccountToken(acc), '');
  assert.equal(context.formatTokenDisplay(acc), '登录会话已配置');
  const legacy = { account_type: 'warp', refresh_token: 'old-secret', token: 'old-jwt' };
  assert.equal(context.hasSidebarAccountCredential(legacy), false);
  assert.equal(context.getSidebarAccountToken(legacy), '');
  assert.equal(context.formatTokenDisplay(legacy), '待官网登录');
});

test('Warp save cannot submit a manual creation request', async () => {
  const { context, node } = loadUI();
  node('accountType').value = 'warp';
  node('clientCookie').value = 'manual-secret';
  const notices = [];
  context.showToast = (message) => notices.push(message);
  context.fetch = () => { throw new Error('manual Warp creation must not send a request'); };
  await context.saveAccount({ preventDefault() {} });
  assert.equal(notices.length, 1);
  assert.match(notices[0], /官方网页登录/);
});

test('linked Console rows are filtered and SSO saves target the Web source', async () => {
  const { context, node } = loadUI();
  node('accountModal').classList = { add() {}, remove() {} };
  node('accountModal').style = {};
  context.stopWarpDeviceLogin = () => {};
  context.resetWarpDeviceLoginStatus = () => {};
  context.stopGrokDeviceLogin = () => {};
  context.resetGrokDeviceLoginStatus = () => {};
  context.clearAccountImportStatus = () => {};
  context.sortAccounts = () => {};
  context.renderPlatformTabs = () => {};
  context.renderAccounts = () => {};
  context.updateStats = () => {};
  context.autoRefreshWarpAccounts = () => {};
  context.fetch = async () => ({ status: 200, json: async () => [
    { id: 42, account_type: 'grok', credential_type: 'sso', grok_provider: 'console', grok_sso_parent_id: 7, client_cookie: 'sso=internal', enabled: true },
    { id: 7, account_type: 'grok', credential_type: 'sso', grok_provider: 'web', client_cookie: 'sso=visible', enabled: true, weight: 2 },
  ] });
  await context.loadAccounts();
  assert.equal(vm.runInContext('accounts.length', context), 1);
  assert.equal(vm.runInContext('accounts[0].id', context), 7);

  const account = vm.runInContext('accounts[0]', context);
  context.openModal(account);
  assert.equal(node('grokProvider').value, 'web');
  assert.match(node('grokProviderHint').textContent, /内部维护 Console/);

  let sent;
  context.fetch = async (url, options) => { sent = { url, options }; return { ok: true }; };
  context.closeModal = () => {};
  context.loadAccounts = () => {};
  context.showToast = () => {};
  await context.saveAccount({ preventDefault() {} });

  assert.equal(sent.url, '/api/accounts/7');
  assert.equal(sent.options.method, 'PUT');
  assert.equal(JSON.parse(sent.options.body).grok_provider, 'web');
});

test('Warp settings save succeeds without submitting credentials', async () => {
  const { context, node } = loadUI();
  node('accountType').value = 'warp';
  node('accountId').value = '7';
  node('enabled').checked = true;
  vm.runInContext('accounts = [{ id: 7, account_type: "warp", weight: 2, warp_authenticated: true }]', context);
  let sent;
  context.fetch = async (url, options) => { sent = { url, options }; return { ok: true }; };
  context.clearAccountImportStatus = () => {};
  context.closeModal = () => {};
  context.loadAccounts = () => {};
  context.showToast = () => {};
  await context.saveAccount({ preventDefault() {} });
  assert.equal(sent.url, '/api/accounts/7');
  assert.equal(sent.options.method, 'PUT');
  assert.deepEqual(JSON.parse(sent.options.body), { account_type: 'warp', weight: 2, enabled: true });
});

test('WorkBuddy exposes official login only for new accounts and keeps manual input optional', () => {
  const { context, node } = loadUI();
  node('accountId').value = '';
  context.applyTokenLabels('workbuddy');
  assert.equal(node('workbuddyLoginGroup').hidden, false);
  assert.equal(node('puterWebLoginGroup').hidden, true);
  assert.equal(node('warpDeviceLoginGroup').hidden, true);
  assert.equal(node('clientCookie').required, false);
  assert.match(node('tokenLabel').textContent, /WorkBuddy/);

  node('accountId').value = '11';
  context.applyTokenLabels('workbuddy');
  assert.equal(node('workbuddyLoginGroup').hidden, true);

  context.applyTokenLabels('puter');
  node('accountId').value = '';
  context.applyTokenLabels('puter');
  assert.equal(node('workbuddyLoginGroup').hidden, true);
  assert.equal(node('puterWebLoginGroup').hidden, false);
});

test('WorkBuddy edits may keep the stored credential and never display the refresh token', async () => {
  const { context, node } = loadUI();
  const account = workBuddyAccount();
  vm.runInContext(`accounts = [${JSON.stringify(account)}]`, context);
  assert.equal(context.getAccountToken(account), account.workbuddy_access_token);
  assert.equal(context.hasSidebarAccountCredential(account), true);
  assert.equal(context.hasSidebarAccountCredential({ account_type: 'workbuddy', enabled: true }), false);

  node('accountType').value = 'workbuddy';
  node('accountId').value = '11';
  node('enabled').checked = true;
  node('clientCookie').value = '';
  let sent;
  context.fetch = async (url, options) => { sent = { url, options }; return { ok: true }; };
  context.clearAccountImportStatus = () => {};
  context.closeModal = () => {};
  context.loadAccounts = () => {};
  context.showToast = () => {};
  await context.saveAccount({ preventDefault() {} });

  assert.equal(sent.url, '/api/accounts/11');
  assert.equal(sent.options.method, 'PUT');
  const body = JSON.parse(sent.options.body);
  assert.equal(body.account_type, 'workbuddy');
  // An empty submission must not overwrite the server-side credential.
  assert.ok(!body.client_cookie || body.client_cookie === '', JSON.stringify(body));
});

test('WorkBuddy new-account modal starts the official login flow automatically', () => {
  const { context, node } = loadUI();
  let started = 0;
  vm.runInContext(
    'globalThis.WorkBuddyLogin = { start() { globalThis.__wbStarted = (globalThis.__wbStarted || 0) + 1; }, stop() {} };',
    context,
  );
  node('accountModal').classList = { add() {}, remove() {}, contains() { return true; } };
  node('accountModal').style = {};
  node('accountId').value = '';
  node('enabled').checked = true;
  // The modal derives its type from the platform tab, which is reflected in the
  // hidden accountType field.
  node('accountType').value = 'workbuddy';
  context.openModal();
  started = vm.runInContext('globalThis.__wbStarted || 0', context);
  assert.equal(started, 1);

  // Editing an existing account must not auto-open the login popup.
  vm.runInContext('globalThis.__wbStarted = 0', context);
  context.openModal(workBuddyAccount());
  started = vm.runInContext('globalThis.__wbStarted || 0', context);
  assert.equal(started, 0);
});

test('workbuddy-auth module exposes a popup login without persisting tokens', () => {
  const source = fs.readFileSync(path.join(__dirname, 'static/js/workbuddy-auth.js'), 'utf8');
  assert.match(source, /api\/workbuddy\/login/);
  assert.match(source, /window\.open\(/);
  assert.doesNotMatch(source, /localStorage\.setItem\([^)]*token/i);
  assert.doesNotMatch(source, /document\.cookie/);
});

test('workbuddy-auth reports the server error code instead of blaming the network', () => {
  const source = fs.readFileSync(path.join(__dirname, 'static/js/workbuddy-auth.js'), 'utf8');
  // Every server-side failure code must have an operator-facing message.
  for (const code of [
    'upstream_unreachable',
    'upstream_rejected',
    'origin_mismatch',
    'insecure_origin',
    'store_unavailable',
    'too_many_logins',
  ]) {
    assert.match(source, new RegExp(`${code}:`), `no message for ${code}`);
  }
  // The response body must be read, not discarded behind a generic message.
  assert.match(source, /readErrorPayload\(/);
  // The popup is reserved during the click, before any await, so the browser
  // does not treat it as a blocked script-initiated window.
  const startIndex = source.indexOf('function start()');
  const reserveIndex = source.indexOf("window.open('about:blank'", startIndex);
  const awaitIndex = source.indexOf('await begin(', startIndex);
  assert.ok(reserveIndex > startIndex, 'popup is not reserved in start()');
  assert.ok(awaitIndex === -1 || reserveIndex < awaitIndex, 'popup must be reserved before the first await');
});

test('clicking the WorkBuddy tab then 添加账号 shows the WorkBuddy login, never Warp', () => {
  const { context, node } = loadUI();
  vm.runInContext(
    'globalThis.WorkBuddyLogin = { start() { globalThis.__wbStarted = (globalThis.__wbStarted || 0) + 1; }, stop() {} };',
    context,
  );
  node('accountModal').classList = { add() {}, remove() {}, contains() { return true; } };
  node('accountId').value = '';
  node('enabled').checked = true;
  context.renderPlatformTabs();

  const tabs = node('platformFilters').children;
  const workbuddyTab = tabs.find((tab) => tab.textContent === 'workbuddy');
  assert.ok(workbuddyTab, `no workbuddy tab among ${tabs.map((tab) => tab.textContent).join(',')}`);
  workbuddyTab.click();
  assert.equal(vm.runInContext('currentPlatform', context), 'workbuddy');

  // This is the 添加账号 button in the accounts page header.
  context.openModal();

  assert.equal(node('accountType').value, 'workbuddy', 'modal type');
  assert.equal(node('accountTypeDisplay').value, 'WorkBuddy', 'modal type label');
  assert.equal(node('workbuddyLoginGroup').hidden, false, 'workbuddy login must be visible');
  assert.equal(node('warpDeviceLoginGroup').hidden, true, 'warp login must stay hidden');
  assert.equal(node('grokDeviceLoginGroup').hidden, true, 'grok login must stay hidden');
  assert.equal(node('puterWebLoginGroup').hidden, true, 'puter login must stay hidden');
  assert.equal(node('ssoCredentialGroup').hidden, false, 'manual credential field must stay available');
});

test('every platform tab maps to its own provider login surface', () => {
  const { context, node } = loadUI();
  vm.runInContext('globalThis.WorkBuddyLogin = { start() {}, stop() {} };', context);
  node('accountModal').classList = { add() {}, remove() {}, contains() { return true; } };
  node('enabled').checked = true;
  context.renderPlatformTabs();

  const expectations = {
    warp: { warpDeviceLoginGroup: false, workbuddyLoginGroup: true, puterWebLoginGroup: true },
    puter: { warpDeviceLoginGroup: true, workbuddyLoginGroup: true, puterWebLoginGroup: false },
    workbuddy: { warpDeviceLoginGroup: true, workbuddyLoginGroup: false, puterWebLoginGroup: true },
    grok: { warpDeviceLoginGroup: true, workbuddyLoginGroup: true, puterWebLoginGroup: true },
  };
  for (const [platform, expected] of Object.entries(expectations)) {
    node('accountId').value = '';
    context.filterByPlatform(platform);
    context.openModal();
    assert.equal(node('accountType').value, platform, `${platform}: modal type`);
    for (const [id, hidden] of Object.entries(expected)) {
      assert.equal(node(id).hidden, hidden, `${platform}: ${id} hidden`);
    }
  }
});
