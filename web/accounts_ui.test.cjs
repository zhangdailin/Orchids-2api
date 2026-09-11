// Run with: node --test web/accounts_ui.test.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadUI() {
  const elements = new Map();
  const node = (id) => {
    if (!elements.has(id)) elements.set(id, { value: '', hidden: false, required: false });
    return elements.get(id);
  };
  const context = vm.createContext({
    document: { getElementById: node, querySelector: node, addEventListener() {} },
  });
  for (const file of ['common.js', 'accounts.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js', file), 'utf8'), context);
  }
  return { context, node };
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
