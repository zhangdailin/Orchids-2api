// Run with: node --test web/ops_render.test.cjs
// Renders the operations overview against a realistic payload in a minimal DOM.
// The asset tests above only inspect the source text; this one actually executes
// the page script, which is how a "card stays empty" bug is caught.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function makeElement(id) {
  const classes = new Set();
  return {
    id,
    textContent: '',
    innerHTML: '',
    value: '',
    hidden: false,
    children: [],
    dataset: {},
    style: {},
    className: '',
    classList: {
      add: (name) => classes.add(name),
      remove: (name) => classes.delete(name),
      toggle: (name, on) => (on ? classes.add(name) : classes.delete(name)),
      contains: (name) => classes.has(name),
    },
    replaceChildren(...nodes) {
      this.children = nodes;
      this.textContent = '';
    },
    appendChild(node) {
      this.children.push(node);
      return node;
    },
    addEventListener() {},
    querySelector(selector) {
      if (selector === 'tbody') {
        this.tbody = this.tbody || makeElement('tbody');
        return this.tbody;
      }
      return makeElement(selector);
    },
    querySelectorAll() {
      return [];
    },
  };
}

const livePayload = JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'live_payload.json'), 'utf8'));

const realPayload = {
  available: true,
  window_minutes: 180,
  since: '2026-09-12T11:48:19Z',
  until: '2026-09-12T14:48:19Z',
  retention_hours: 192,
  channels: ['grok', 'puter', 'warp', 'workbuddy'],
  excluded_aggregates: ['http', 'probe'],
  totals: {
    requests: 253,
    success: 230,
    failed: 23,
    probes: 5,
    success_rate: 0.909,
    rpm: 1.4,
    duration_p95_ms: 8536,
    first_token_p95_ms: 3550,
    samples: 240,
  },
  concurrency: { accounts_refreshing: 2 },
  alerts: [{ key: 'success-rate:warp', severity: 'warning', channel: 'warp', title: 'warp 成功率 89%', detail: '窗口内 40 次请求' }],
  coverage: {
    entries: 10011,
    oldest: '2026-06-23T10:53:53Z',
    newest: '2026-09-12T14:48:12Z',
    counts: { request: 1974, operation: 10, system: 16 },
  },
  series: [
    { minute: '2026-09-12T13:53:00Z', requests: 3, success: 1, failed: 2, probes: 0 },
    { minute: '2026-09-12T13:54:00Z', requests: 5, success: 5, failed: 0, probes: 1 },
  ],
  matrix: [
    {
      channel: 'grok',
      accounts_enabled: 6,
      accounts_available: 6,
      accounts_needing_login: 0,
      model_cooldowns: 1,
      has_sample: true,
      summary: { requests: 20, success: 20, failed: 0, probes: 0, success_rate: 1, rpm: 0.1, duration_p95_ms: 8536, first_token_p95_ms: 3550, samples: 20 },
      models: [{ model: 'grok-4.6', requests: 8, success: 8, failed: 0, success_rate: 1, duration_p95_ms: 8536, samples: 8 }],
      series: [{ minute: '2026-09-12T13:53:00Z', requests: 2, success: 2, failed: 0, probes: 0 }],
    },
    {
      channel: 'warp',
      accounts_enabled: 1,
      accounts_available: 1,
      accounts_needing_login: 0,
      model_cooldowns: 0,
      has_sample: false,
      summary: { requests: 0, success: 0, failed: 0, probes: 0, success_rate: 0, rpm: 0, duration_p95_ms: 0, first_token_p95_ms: 0, samples: 0 },
      models: [],
      series: [],
    },
  ],
};

function renderPage(payload) {
  const elements = new Map();
  const node = (id) => {
    if (!elements.has(id)) elements.set(id, makeElement(id));
    return elements.get(id);
  };
  const context = vm.createContext({
    console,
    document: {
      readyState: 'complete',
      getElementById: node,
      createElement: (tag) => makeElement(tag),
      querySelector: node,
      querySelectorAll: () => [],
      addEventListener() {},
    },
    window: { setInterval: () => 0, clearInterval() {}, addEventListener() {} },
    setInterval: () => 0,
    setTimeout: (fn) => { fn(); return 0; },
    URLSearchParams,
    fetch: async () => ({
      ok: true,
      status: 200,
      json: async () => payload,
    }),
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js/ops.js'), 'utf8'), context);
  return { context, node };
}

test('the data-coverage card is filled in from the payload', async () => {
  const { node } = renderPage(realPayload);
  // The page fetches on load; let the promise chain settle.
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  const coverage = node('opsCoverage').textContent;
  assert.ok(coverage.length > 0, 'the coverage card is empty');
  assert.notEqual(coverage, '—', 'the coverage card still shows its placeholder');
  assert.match(coverage, /10011/, 'the retained entry count is missing');
  assert.match(coverage, /2026-06-23/, 'the oldest retained record is missing');
  assert.match(coverage, /保留窗口内/, 'the coverage caveat is missing');
  assert.match(coverage, /http/, 'the excluded aggregate is not explained');
  assert.match(coverage, /probe/, 'the excluded aggregate is not explained');

  const label = node('opsWindowLabel').textContent;
  assert.match(label, /180/, 'the window label is not filled in');
  assert.match(label, /192/, 'the retention hours are not shown');
});

test('the KPI cards render the figures and mark no-sample ones', async () => {
  const { node } = renderPage(realPayload);
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  const cards = node('opsKpis').children;
  assert.ok(cards.length >= 6, `expected KPI cards, got ${cards.length}`);
  const values = cards.map((card) => (card.children[1] ? card.children[1].textContent : ''));
  assert.ok(values.includes('248'), `real request count missing from ${values.join(',')}`);
  assert.ok(values.some((value) => /90\.9%/.test(value)), `success rate missing from ${values.join(',')}`);
});

test('the matrix renders provider rows plus their model rows', async () => {
  const { node } = renderPage(realPayload);
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  const rows = node('opsMatrix').querySelector('tbody').children;
  assert.equal(rows.length, 3, `expected grok + grok-4.6 + warp rows, got ${rows.length}`);
  const labels = rows.map((row) => (row.children[0] ? row.children[0].textContent : ''));
  assert.deepEqual(labels, ['grok', 'grok-4.6', 'warp']);
  // The traffic-free channel must read 暂无样本, never a healthy 100%.
  const warpCells = rows[2].children.map((cell) => cell.textContent);
  assert.ok(warpCells.includes('暂无样本'), `warp row shows ${warpCells.join('|')}`);
});

test('the live production payload renders every card', async () => {
  const payload = JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'live_payload.json'), 'utf8'));
  const { node } = renderPage(payload);
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  assert.ok(node('opsKpis').children.length > 0, 'KPI cards are empty with the live payload');
  assert.notEqual(node('opsCoverage').textContent, '—', 'the coverage card is empty with the live payload');
  assert.ok(node('opsCoverage').textContent.length > 0, 'the coverage card is empty with the live payload');
  assert.ok(node('opsTrend').children.length > 0, 'the trend is empty with the live payload');
  assert.ok(node('opsMatrix').querySelector('tbody').children.length > 0, 'the matrix is empty with the live payload');
  assert.ok(node('opsWindowLabel').textContent.length > 0, 'the window label is empty with the live payload');
});
test('a failed fetch still explains itself instead of leaving the card blank', async () => {
  const elements = new Map();
  const node = (id) => {
    if (!elements.has(id)) elements.set(id, makeElement(id));
    return elements.get(id);
  };
  const context = vm.createContext({
    console,
    document: { readyState: 'complete', getElementById: node, createElement: (tag) => makeElement(tag), querySelector: node, querySelectorAll: () => [], addEventListener() {} },
    window: { setInterval: () => 0, clearInterval() {}, addEventListener() {} },
    setInterval: () => 0,
    setTimeout: (fn) => { fn(); return 0; },
    URLSearchParams,
    fetch: async () => ({ ok: false, status: 401, json: async () => ({}) }),
  });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js/ops.js'), 'utf8'), context);
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));

  const coverage = node('opsCoverage').textContent;
  assert.notEqual(coverage, '—', 'a failed fetch left the card at its placeholder');
  assert.match(coverage, /401/, 'the failure reason is not shown');
  assert.ok(node('opsKpis').children.length > 0, 'the failure is not surfaced in the KPIs');
});

test('an aggregation-disabled response still fills the coverage card', async () => {
  const { node } = renderPage({ available: false, note: '指标聚合需要 Redis；当前部署未启用。', window_minutes: 180, coverage: {}, excluded_aggregates: [] });
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  assert.match(node('opsCoverage').textContent, /Redis/, 'the disabled reason is not shown');
});