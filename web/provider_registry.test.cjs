const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const root = __dirname;
const registrySource = fs.readFileSync(path.join(root, 'static/js/provider-registry.js'), 'utf8');
const accounts = fs.readFileSync(path.join(root, 'static/js/accounts.js'), 'utf8');
const models = fs.readFileSync(path.join(root, 'static/js/models.js'), 'utf8');
const payload = {
  defaultProviderKey: 'warp',
  providers: [
    { key: 'warp', label: 'Warp' }, { key: 'workbuddy', label: 'WorkBuddy' },
    { key: 'qoder', label: 'Qoder' }, { key: 'cline', label: 'Cline' },
    { key: 'grok', label: 'Grok' },
  ],
};

async function registry() {
  const window = {};
  vm.runInNewContext(registrySource, { window, Object, Promise });
  await window.OrchidsProviderRegistry.ready;
  return window.OrchidsProviderRegistry;
}

test('frontend provider registry is generated from the backend definitions', async () => {
  const value = await registry();
  assert.deepEqual(Array.from(value.keys), payload.providers.map((item) => item.key));
  assert.deepEqual(Array.from(value.channels), payload.providers.map((item) => item.label));
  assert.equal(value.defaultProviderKey, 'warp');
  assert.match(registrySource, /Code generated from internal\/channel definitions/);
  assert.match(registrySource, /key":"cline"/);
});

test('accounts and models consume the backend-fed provider registry', () => {
  assert.match(accounts, /OrchidsProviderRegistry\?\.keys/);
  assert.match(accounts, /OrchidsProviderRegistry\?\.providers/);
  assert.match(models, /OrchidsProviderRegistry\?\.channels/);
  assert.doesNotMatch(accounts, /\["warp", "workbuddy", "qoder", "cline", "grok"\]/);
  assert.doesNotMatch(models, /\["Warp", "WorkBuddy", "Qoder", "Cline", "Grok"\]/);
});

test('all provider pages load the registry before page code', () => {
  for (const [name, script] of [['accounts', 'accounts.js'], ['models', 'models.js']]) {
    const html = fs.readFileSync(path.join(root, `templates/pages/${name}.html`), 'utf8');
    assert.ok(html.indexOf('provider-registry.js') >= 0, `${name} missing registry`);
    assert.ok(html.indexOf('provider-registry.js') < html.indexOf(script), `${name} loads registry too late`);
  }
});
