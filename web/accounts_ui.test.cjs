// Run with: node --test web/accounts_ui.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadUI() {
  const timers = [];
  const storage = new Map();
  // Minimal element/DOM surface: enough for tab rendering and modal wiring.
  const makeElement = (tag) => {
    const classes = new Set();
    const children = [];
    const listeners = {};
    const element = {
      tagName: tag,
      value: '',
      hidden: false,
      required: false,
      disabled: false,
      checked: false,
      textContent: '',
      // escapeHtml() renders through a detached div, so the stub must mirror
      // textContent into innerHTML the way the DOM does.
      get innerHTML() { return this.__innerHTML !== undefined ? this.__innerHTML : this.textContent; },
      set innerHTML(value) { this.__innerHTML = value; },
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
    return element;
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
      createDocumentFragment: () => makeElement('fragment'),
      querySelectorAll: () => [],
      addEventListener() {},
    },
    window: {
      setInterval: (fn) => { timers.push(fn); return timers.length; },
      clearInterval: () => {},
      addEventListener() {},
      matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
      innerWidth: 1440,
      localStorage: {
        getItem: (key) => (storage.has(key) ? storage.get(key) : null),
        setItem: (key, value) => storage.set(key, String(value)),
        removeItem: (key) => storage.delete(key),
      },
    },
    // Immediate pacing so the auto-sync loop settles synchronously in tests.
    setTimeout: (fn) => { fn(); return 0; },
  });
  for (const file of ['common.js', 'accounts.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js', file), 'utf8'), context);
  }
  return { context, node, storage };
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

test('clicking a platform tab makes 添加账号 open in that platform', () => {
  const { context, node } = loadUI();
  vm.runInContext('globalThis.WorkBuddyLogin = { start() {}, stop() {} };', context);
  node('accountModal').classList = { add() {}, remove() {}, contains() { return true; } };
  node('accountId').value = '';
  node('enabled').checked = true;
  context.renderPlatformTabs();

  const tabs = node('platformFilters').children;
  for (const platform of ['grok', 'puter', 'warp', 'workbuddy']) {
    const tab = tabs.find((candidate) => candidate.textContent === platform);
    assert.ok(tab, `no ${platform} tab among ${tabs.map((candidate) => candidate.textContent).join(',')}`);
    tab.click();
    node('accountId').value = '';
    context.openModal();
    assert.equal(node('accountType').value, platform, `${platform}: openModal type`);
    const expectedLabel = platform === 'workbuddy' ? 'WorkBuddy' : platform.charAt(0).toUpperCase() + platform.slice(1);
    assert.equal(node('accountTypeDisplay').value, expectedLabel, `${platform}: displayed label`);
  }
});

test('the active platform tab wins over a stale account-type field', () => {
  const { context, node } = loadUI();
  vm.runInContext('globalThis.WorkBuddyLogin = { start() {}, stop() {} };', context);
  node('accountModal').classList = { add() {}, remove() {}, contains() { return true; } };
  node('enabled').checked = true;
  context.renderPlatformTabs();
  context.filterByPlatform('grok');
  // A previous modal interaction must not leak its type into the next open.
  context.setAccountModalType('warp');
  node('accountId').value = '';
  context.openModal();
  assert.equal(node('accountType').value, 'grok', 'the active platform tab wins over a stale field');
});

test('editing a Grok account keeps its credential UI while another tab is active', () => {
  const { context, node } = loadUI();
  vm.runInContext('globalThis.WorkBuddyLogin = { start() {}, stop() {} };', context);
  node('accountModal').classList = { add() {}, remove() {}, contains() { return true; } };
  node('enabled').checked = true;
  context.renderPlatformTabs();
  // The operator is looking at another channel while editing a Grok account.
  context.filterByPlatform('puter');

  const oauthAccount = {
    id: 12,
    account_type: 'grok',
    credential_type: 'oauth',
    grok_provider: 'build',
    oauth_access_token: 'access',
    enabled: true,
    weight: 1,
  };
  context.openModal(oauthAccount);
  assert.equal(node('accountType').value, 'grok', 'the edited account owns the modal type');
  assert.equal(node('credentialModeGroup').hidden, false, 'Grok credential mode must be shown');
  assert.equal(node('grokDeviceLoginGroup').hidden, false, 'the OAuth mode must offer device login');

  const ssoAccount = { ...oauthAccount, id: 13, credential_type: 'sso', client_cookie: 'sso=x' };
  context.openModal(ssoAccount);
  assert.equal(node('accountType').value, 'grok');
  assert.equal(node('ssoCredentialGroup').hidden, false, 'SSO mode must offer the cookie input');
  assert.equal(node('grokDeviceLoginGroup').hidden, true);
});

test('Grok credential modes expose only their own inputs', () => {
  const { context, node } = loadUI();

  // SSO Cookie mode: paste the cookie, keep the internal Console picker hidden.
  node('credentialType').value = 'sso';
  context.applyTokenLabels('grok');
  assert.equal(node('ssoCredentialGroup').hidden, false, 'SSO cookie input must be available');
  assert.equal(node('clientCookie').required, true);
  assert.equal(node('grokProviderGroup').hidden, true, 'the Console product entry must not be exposed');
  assert.equal(node('grokDeviceLoginGroup').hidden, true);
  assert.equal(node('oauthCredentialGroup').hidden, true);
  assert.equal(node('oauthRefreshGroup').hidden, true);
  assert.equal(node('oauthExpiresGroup').hidden, true);

  // Build CLI OAuth mode: official device login only, no manual token fields.
  node('credentialType').value = 'oauth';
  node('accountId').value = '';
  context.applyTokenLabels('grok');
  assert.equal(node('grokDeviceLoginGroup').hidden, false, 'device login must be offered');
  assert.equal(node('ssoCredentialGroup').hidden, true);
  assert.equal(node('oauthCredentialGroup').hidden, true, 'OAuth Access Token input must be gone');
  assert.equal(node('oauthRefreshGroup').hidden, true, 'OAuth Refresh Token input must be gone');
  assert.equal(node('oauthExpiresGroup').hidden, true, 'expiry input must be gone');
  assert.equal(node('grokProviderGroup').hidden, true);
  assert.equal(node('clientCookie').required, false);
});

test('Grok Build CLI OAuth cannot be created from the form', async () => {
  const { context, node } = loadUI();
  node('accountType').value = 'grok';
  node('accountId').value = '';
  node('credentialType').value = 'oauth';
  const notices = [];
  context.showToast = (message) => notices.push(message);
  context.fetch = () => { throw new Error('manual OAuth creation must not send a request'); };
  await context.saveAccount({ preventDefault() {} });
  assert.equal(notices.length, 1);
  assert.match(notices[0], /官方网页登录/);
});

test('Warp exposes only official login and preserves settings editing', () => {
  const { context, node } = loadUI();
  node('credentialType').value = 'oauth';
  context.applyTokenLabels('grok');
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

test('auto-sync refreshes stale accounts on every channel, once per page load', async () => {
  const { context } = loadUI();
  const stale = new Date(Date.now() - 90 * 60 * 1000).toISOString();
  const fresh = new Date(Date.now() - 2 * 60 * 1000).toISOString();

  vm.runInContext(`accounts = ${JSON.stringify([
    // WorkBuddy: stale credit-meter snapshot.
    { id: 1, account_type: 'workbuddy', enabled: true, email: 'wb@example.com', quota_supported: true, quota_limit: 350, quota_remaining: 100, workbuddy_quota: { synced_at: stale } },
    // WorkBuddy: fresh snapshot, must not be re-fetched.
    { id: 2, account_type: 'workbuddy', enabled: true, quota_supported: true, quota_limit: 350, quota_remaining: 100, workbuddy_quota: { synced_at: fresh } },
    // Grok SSO: stale web quota window.
    { id: 3, account_type: 'grok', credential_type: 'sso', grok_provider: 'web', enabled: true, client_cookie: 'sso=x', grok_web_quota: { synced_at: stale } },
    // Grok Build: stale billing snapshot.
    { id: 4, account_type: 'grok', credential_type: 'oauth', grok_provider: 'build', enabled: true, oauth_access_token: 'a', grok_billing: { synced_at: stale } },
    // Grok Build: fresh billing snapshot.
    { id: 5, account_type: 'grok', credential_type: 'oauth', grok_provider: 'build', enabled: true, oauth_access_token: 'a', grok_billing: { synced_at: fresh } },
    // Warp: no settings snapshot at all.
    { id: 6, account_type: 'warp', enabled: true, warp_authenticated: true },
    // Puter: no quota snapshot timestamp exists for this channel.
    { id: 7, account_type: 'puter', enabled: true, client_cookie: 'puter-token', usage_limit: 500, usage_current: 400 },
    // Disabled accounts are never auto-synced.
    { id: 8, account_type: 'workbuddy', enabled: false },
  ])}`, context);

  const checked = [];
  // Exercise the real checkAccount path with a stubbed transport: a successful
  // sync returns the account with a fresh snapshot timestamp.
  context.fetch = async (url) => {
    const parts = String(url).split('/');
    const id = Number(parts[3]);
    if (parts[4] !== 'check') {
      return { ok: true, status: 200, json: async () => vm.runInContext(`accounts.find(a => a.id === ${id})`, context) };
    }
    checked.push(id);
    const syncedAt = new Date().toISOString();
    const account = vm.runInContext(`accounts.find(a => a.id === ${id})`, context);
    const updated = { ...account };
    if (updated.account_type === 'workbuddy') updated.workbuddy_quota = { synced_at: syncedAt };
    if (updated.account_type === 'grok') {
      if (updated.credential_type === 'oauth') updated.grok_billing = { synced_at: syncedAt };
      else updated.grok_web_quota = { synced_at: syncedAt };
    }
    return { ok: true, status: 200, json: async () => updated };
  };
  context.showToast = () => {};

  await vm.runInContext('autoSyncStaleAccounts()', context);

  // Channels with a server-side snapshot use it; the timestamp-less channels
  // (Warp / Puter) are covered by the ledger.
  assert.deepEqual(checked, [1, 3, 4, 6, 7]);

  // A second call inside the same page load must not re-check anything.
  checked.length = 0;
  await vm.runInContext('autoSyncStaleAccounts()', context);
  assert.deepEqual(checked, []);

  // A reload right after a successful sync must not re-check either: the fresh
  // snapshot timestamps and the persisted ledger both say "up to date".
  vm.runInContext('resetAutoSyncLoadGuard()', context);
  checked.length = 0;
  await vm.runInContext('autoSyncStaleAccounts()', context);
  assert.deepEqual(checked, [], 'a reload right after a sync must not re-check anything');

  // Once a timestamp-less channel ages out, only that one is refreshed again.
  vm.runInContext('accountSyncLedger.set(7, Date.now() - 31 * 60 * 1000); resetAutoSyncLoadGuard()', context);
  checked.length = 0;
  await vm.runInContext('autoSyncStaleAccounts()', context);
  assert.deepEqual(checked, [7], 'only the aged-out timestamp-less account may refresh after a reload');
});

test('auto-sync retries an account whose previous attempt failed', async () => {
  const { context } = loadUI();
  vm.runInContext(`accounts = ${JSON.stringify([
    { id: 1, account_type: 'workbuddy', enabled: true, quota_supported: true, quota_limit: 10, quota_remaining: 5 },
  ])}`, context);

  vm.runInContext(`globalThis.__attempts = 0;
    globalThis.checkAccount = () => { globalThis.__attempts += 1; return Promise.resolve(globalThis.__attempts > 1); }`, context);

  await vm.runInContext('autoSyncStaleAccounts()', context);
  assert.equal(vm.runInContext('globalThis.__attempts', context), 1);
  // A failed attempt must be retried on the next page load, not remembered as done.
  await vm.runInContext('resetAutoSyncLoadGuard(); autoSyncStaleAccounts()', context);
  assert.equal(vm.runInContext('globalThis.__attempts', context), 2);
  // After it succeeds, the ledger keeps the next load from repeating it.
  await vm.runInContext('resetAutoSyncLoadGuard(); autoSyncStaleAccounts()', context);
  assert.equal(vm.runInContext('globalThis.__attempts', context), 2);
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
  context.autoSyncStaleAccounts = () => {};
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

test('WorkBuddy is OAuth-only in the modal: no manual credential field', () => {
  const { context, node } = loadUI();
  node('accountId').value = '';
  context.applyTokenLabels('workbuddy');
  assert.equal(node('workbuddyLoginGroup').hidden, false, 'official login must be presented');
  assert.equal(node('ssoCredentialGroup').hidden, true, 'the manual credential field must be gone');
  assert.equal(node('clientCookie').required, false);
  assert.equal(node('clientCookie').value, '');
  assert.equal(node('#accountForm button[type="submit"]').hidden, true,
    'a new WorkBuddy account is created by the login flow, not by the form');
  // Other channels keep their manual credential field.
  context.applyTokenLabels('puter');
  assert.equal(node('ssoCredentialGroup').hidden, false);
  assert.equal(node('#accountForm button[type="submit"]').hidden, false);

  // Editing keeps the login button (re-authorization) and the save button
  // (settings), but still no credential field.
  node('accountId').value = '11';
  context.applyTokenLabels('workbuddy');
  assert.equal(node('workbuddyLoginGroup').hidden, false);
  assert.equal(node('ssoCredentialGroup').hidden, true);
  assert.equal(node('#accountForm button[type="submit"]').hidden, false);

  context.applyTokenLabels('puter');
  node('accountId').value = '';
  context.applyTokenLabels('puter');
  assert.equal(node('workbuddyLoginGroup').hidden, true);
  assert.equal(node('puterWebLoginGroup').hidden, false);
});

test('WorkBuddy creation cannot be submitted from the form', async () => {
  const { context, node } = loadUI();
  node('accountType').value = 'workbuddy';
  node('accountId').value = '';
  node('clientCookie').value = 'refresh-token-typed-anyway';
  const notices = [];
  context.showToast = (message) => notices.push(message);
  context.fetch = () => { throw new Error('manual WorkBuddy creation must not send a request'); };
  await context.saveAccount({ preventDefault() {} });
  assert.equal(notices.length, 1);
  assert.match(notices[0], /官方网页登录/);
});

test('opening the WorkBuddy modal never starts a login on its own', () => {
  const { context, node } = loadUI();
  vm.runInContext(
    'globalThis.WorkBuddyLogin = { start() { globalThis.__wbStarted = (globalThis.__wbStarted || 0) + 1; }, stop() {} };',
    context,
  );
  node('accountModal').classList = { add() {}, remove() {}, contains() { return true; } };
  node('accountType').value = 'workbuddy';
  node('accountId').value = '';
  node('enabled').checked = true;

  context.openModal();
  assert.equal(vm.runInContext('globalThis.__wbStarted || 0', context), 0,
    'opening the add-account modal must not open the login page');
  assert.equal(node('workbuddyLoginGroup').hidden, false, 'the login button must be presented');
  assert.equal(node('workbuddyLoginStatus').hidden, true, 'no status should be shown before a click');

  // Editing an existing account must not navigate either.
  context.openModal(workBuddyAccount());
  assert.equal(vm.runInContext('globalThis.__wbStarted || 0', context), 0,
    'opening the edit modal must not open the login page');

  // Only an explicit click on the login button starts the flow.
  vm.runInContext('globalThis.WorkBuddyLogin.start()', context);
  assert.equal(vm.runInContext('globalThis.__wbStarted || 0', context), 1);
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
  // The credential field is hidden for this channel, so an edit submits no
  // client_cookie at all.
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

test('WorkBuddy rows show the metered credits, plan label and signed-in email', () => {
  const { context } = loadUI();
  const account = workBuddyAccount({
    email: 'operator@example.com',
    workbuddy_uid: '07ab88c8-5596-4257-8d21-e9fcbe3a3810',
    quota_supported: true,
    quota_limit: 350,
    quota_remaining: 147.28,
    quota_used: 202.72,
    quota_unit: 'credit',
    quota_plan: 'Free Plan Subscription',
    quota_consumed_units: 202,
    quota_reset_at: new Date(Date.now() + 12 * 86400000).toISOString(),
    usage_limit: 350,
    usage_current: 147.28,
    request_count: 0,
  });

  // The meter reports REMAINING; reading usage_current as "used" would invert it.
  const quota = context.getQuotaStats(account);
  assert.equal(quota.workbuddy, true);
  assert.equal(quota.limit, 350);
  assert.equal(quota.remaining, 147.28);
  assert.equal(quota.used, 350 - 147.28, 'used must be derived from the meter');
  assert.ok(quota.pctRemaining > 40 && quota.pctRemaining < 43, `pctRemaining=${quota.pctRemaining}`);

  const markup = context.buildQuotaMarkup(account);
  assert.match(markup, /147\.28 \/ 350/);
  assert.match(markup, /剩余/);
  assert.match(markup, /天后重置/);

  // 等级 shows the upstream plan label, not a guessed subscription tier.
  assert.match(context.buildSubscriptionMarkup(account), /Free Plan Subscription/);

  // 调用 falls back to meter consumption because this channel has no request counter.
  assert.equal(context.accountUsageCounter(account), 202);
  assert.equal(context.accountUsageCounter({ account_type: 'puter', request_count: 7 }), 7);

  // The token column identifies the account by its signed-in address.
  const tokenCell = context.formatTokenDisplay(account);
  assert.match(tokenCell, /operator@example\.com/);
  assert.doesNotMatch(tokenCell, /workbuddy_refresh_token/);
});

test('WorkBuddy without a meter snapshot says so instead of showing a fake quota', () => {
  const { context } = loadUI();
  const account = workBuddyAccount({ email: 'operator@example.com' });
  assert.equal(context.getSidebarQuotaStats(account), null);
  const quota = context.getQuotaStats(account);
  assert.equal(quota.unknown, true);
  assert.match(context.buildQuotaMarkup(account), /WorkBuddy 计量接口未返回数据/);
  assert.match(context.buildSubscriptionMarkup(account), /未同步/);
  // usage_current must never be interpreted as a remaining balance without the
  // explicit quota_* fields the server sends.
  assert.equal(context.getQuotaStats({ account_type: 'workbuddy', usage_limit: 350, usage_current: 147.28 }).unknown, true);
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
  assert.equal(node('ssoCredentialGroup').hidden, true, 'workbuddy is OAuth-only, no manual field');
});

test('every platform tab maps to its own provider login surface', () => {
  const { context, node } = loadUI();
  vm.runInContext('globalThis.WorkBuddyLogin = { start() {}, stop() {} };', context);
  node('accountModal').classList = { add() {}, remove() {}, contains() { return true; } };
  node('enabled').checked = true;
  context.renderPlatformTabs();

  const expectations = {
    warp: { warpDeviceLoginGroup: false, workbuddyLoginGroup: true, puterWebLoginGroup: true, ssoCredentialGroup: true },
    puter: { warpDeviceLoginGroup: true, workbuddyLoginGroup: true, puterWebLoginGroup: false, ssoCredentialGroup: false },
    workbuddy: { warpDeviceLoginGroup: true, workbuddyLoginGroup: false, puterWebLoginGroup: true, ssoCredentialGroup: true },
    grok: { warpDeviceLoginGroup: true, workbuddyLoginGroup: true, puterWebLoginGroup: true, ssoCredentialGroup: false },
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

test('a rejected credential shows the reason, not the raw error envelope', () => {
  const { context } = loadUI();
  // The server answers rejected credentials with the standard admin envelope.
  const envelope = JSON.stringify({
    error: { type: 'authentication_error', message: 'account was rejected by the upstream and was not saved: 401 unauthorized' },
    type: 'error',
  });
  assert.equal(
    vm.runInContext(`extractAdminErrorDetail(${JSON.stringify(envelope)})`, context),
    'account was rejected by the upstream and was not saved: 401 unauthorized',
    'JSON envelope must be unwrapped to its message',
  );
  assert.equal(
    vm.runInContext('extractAdminErrorDetail("missing sso token")', context),
    'missing sso token',
    'a plain-text body is already the detail',
  );
  assert.equal(vm.runInContext('extractAdminErrorDetail("")', context), '', 'an empty body stays empty');
});
