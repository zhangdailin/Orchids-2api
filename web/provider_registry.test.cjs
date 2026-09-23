const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const root = __dirname;
const registrySource = fs.readFileSync(path.join(root, 'static/js/provider-registry.js'), 'utf8');
const accounts = fs.readFileSync(path.join(root, 'static/js/accounts.js'), 'utf8');
const models = fs.readFileSync(path.join(root, 'static/js/models.js'), 'utf8');

function registry() {
  const window = {};
  vm.runInNewContext(registrySource, { window, Object });
  return window.OrchidsProviderRegistry;
}

test('accounts and models consume one provider registry in exact order', () => {
  const value = registry();
  assert.deepEqual(Array.from(value.keys), ['warp', 'puter', 'workbuddy', 'qoder', 'cline', 'grok']);
  assert.deepEqual(Array.from(value.channels), ['Warp', 'Puter', 'WorkBuddy', 'Qoder', 'Cline', 'Grok']);
  assert.match(accounts, /OrchidsProviderRegistry\?\.keys/);
  assert.match(accounts, /OrchidsProviderRegistry\?\.providers/);
  assert.match(models, /OrchidsProviderRegistry\?\.channels/);
  assert.doesNotMatch(accounts, /\["warp", "puter", "workbuddy", "qoder", "cline", "grok"\]/);
  assert.doesNotMatch(models, /\["Warp", "Puter", "WorkBuddy", "Qoder", "Cline", "Grok"\]/);
});

test('all provider pages load the same registry before page code', () => {
  for (const [name, script] of [['accounts', 'accounts.js'], ['models', 'models.js'], ['grok-tools', 'grok-tools.min.js']]) {
    const html = fs.readFileSync(path.join(root, `templates/pages/${name}.html`), 'utf8');
    assert.ok(html.indexOf('provider-registry.js') >= 0, `${name} missing registry`);
    assert.ok(html.indexOf('provider-registry.js') < html.indexOf(script), `${name} loads registry too late`);
  }
});
