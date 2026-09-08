const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
const render = source.slice(source.indexOf('  function renderChatSessions()'), source.indexOf('  function syncChatModelUI()'));

function element() {
  return { children: [], dataset: {}, listeners: {}, innerHTML: '',
    appendChild(child) { this.children.push(child); },
    addEventListener(name, fn) { this.listeners[name] = fn; } };
}

test('Grok session rendering resolves its container and wires the selected session', () => {
  const list = element();
  let selected;
  const context = vm.createContext({
    document: { getElementById(id) { assert.equal(id, 'grokSessionList'); return list; }, createElement: element },
    chatState: { sessions: [{ id: 'one', title: 'Test chat', updatedAt: 1 }], activeId: 'one' },
    relativeTime: () => 'now', switchChatSession: id => { selected = id; }, isMobileChatSidebar: () => false,
  });
  vm.runInContext(render + '\nrenderChatSessions();', context);
  assert.equal(list.children.length, 1);
  assert.equal(list.children[0].children[0].textContent, 'Test chat');
  assert.equal(list.children[0].className, 'session-item active');
  list.children[0].listeners.click();
  assert.equal(selected, 'one');
});

test('Grok startup tolerates a missing session container', () => {
  const context = vm.createContext({ document: { getElementById: () => null } });
  assert.doesNotThrow(() => vm.runInContext(render + '\nrenderChatSessions();', context));
});
