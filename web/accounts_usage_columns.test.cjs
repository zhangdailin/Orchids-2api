// Run with: node --test web/accounts_usage_columns.test.cjs
//
// The account table gained the three columns the reference project shows:
// today/lifetime tokens, the cooldown recovery time and the creation date.
// These pin the arithmetic behind them, which is the part that can be silently
// wrong: a daily figure that never rolls, and a recovery line printed for an
// account that is not held.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadUI() {
  const storage = new Map();
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
      get innerHTML() { return this.__innerHTML !== undefined ? this.__innerHTML : this.textContent; },
      set innerHTML(value) { this.__innerHTML = value; },
      style: {},
      dataset: {},
      children,
      reset() {},
      addEventListener(type, fn) { (listeners[type] ||= []).push(fn); },
      click() { (listeners.click || []).forEach((fn) => fn({ target: this })); },
      appendChild(child) { children.push(child); return child; },
      insertAdjacentHTML(_position, html) { this.__innerHTML = (this.__innerHTML || '') + html; },
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
      createDocumentFragment: () => makeElement('fragment'),
      querySelectorAll: () => [],
      addEventListener() {},
    },
    window: {
      setInterval: () => 0,
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
    setTimeout: (fn) => { fn(); return 0; },
  });
  for (const file of ['common.js', 'accounts.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'static/js', file), 'utf8'), context);
  }
  return { context, node };
}

const strip = (html) => String(html || '').replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();
const title = (html) => {
  const match = String(html || '').match(/title="([^"]*)"/);
  return match ? match[1] : '';
};

function dayStamp(offsetDays = 0) {
  const d = new Date(Date.now() + offsetDays * 86400000);
  const pad = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

// --- 今日/累计 Tokens -------------------------------------------------------

test('the daily token figure is counted only when its stamp is today', () => {
  const { context } = loadUI();
  const acc = { account_type: 'cline', tokens_today: 23849, tokens_date: dayStamp(), usage_total: 23849 };
  assert.equal(context.accountTokensToday(acc), 23849);
  // A stamp from yesterday is yesterday's spend: it must not be shown as today.
  assert.equal(context.accountTokensToday({ ...acc, tokens_date: dayStamp(-1) }), 0);
  // An account that predates the counter has no date; claiming a daily figure
  // would invent one, so the answer is "not counted".
  assert.equal(context.accountTokensToday({ ...acc, tokens_date: '' }), 0);
});

test('the tokens cell abbreviates both figures and names them in the tooltip', () => {
  const { context } = loadUI();
  const acc = { account_type: 'cline', tokens_today: 23849, tokens_date: dayStamp(), usage_total: 1234567 };
  const markup = context.buildTokensMarkup(acc);
  assert.equal(strip(markup), '23.8K / 1.23M');
  // The tooltip states the numbers are the gateway's own count, so a local
  // estimate is never read as a balance the upstream reported.
  assert.match(title(markup), /本地统计/);
  assert.match(title(markup), /23,849/);
});

test('an account with no spend still renders a comparable cell', () => {
  const { context } = loadUI();
  assert.equal(strip(context.buildTokensMarkup({ account_type: 'cline' })), '0 / 0');
});

// --- 冷却恢复时间 -----------------------------------------------------------

test('the recovery line appears only for an account that is actually held', () => {
  const { context } = loadUI();
  // A healthy account must not carry a recovery time: printing one there would
  // say "held" while the badge says 正常.
  const healthy = { id: 1, account_type: 'cline', enabled: true, has_credential: true, status_code: '' };
  assert.equal(context.buildCooldownMarkup(healthy), '');
  // A cooled account with a stated deadline says when it comes back. The wait
  // is rendered from the same instant as the clock, so the two cannot disagree:
  // 3h minus one second of test runtime must still read as 3 小时, not 2.
  const cooled = { ...healthy, status_code: '429', quota_reset_at: new Date(Date.now() + 3 * 3600000).toISOString() };
  const markup = context.buildCooldownMarkup(cooled);
  assert.match(markup, /预计/);
  assert.match(strip(markup), /恢复/);
  assert.match(strip(markup), /3 小时后/);
});

test('the recovery deadline is the one that still holds the account', () => {
  const { context } = loadUI();
  const acc = {
    id: 2, account_type: 'grok', enabled: true, has_credential: true, status_code: '429',
    quota_reset_at: new Date(Date.now() + 3600000).toISOString(),
    quality_cooldown_until: new Date(Date.now() + 5 * 3600000).toISOString(),
  };
  // The nearer deadline has already passed relative to the further one, so
  // quoting it would understate the wait.
  assert.match(strip(context.buildCooldownMarkup(acc)), /5 小时后/);
});

test('an expired cooldown says it is due rather than promising a future time', () => {
  const { context } = loadUI();
  const acc = {
    id: 3, account_type: 'cline', enabled: true, has_credential: true, status_code: '429',
    quota_reset_at: new Date(Date.now() - 60000).toISOString(),
  };
  assert.match(strip(context.buildCooldownMarkup(acc)), /已到期/);
});

test('the status cell carries the recovery line on both the table and the card', () => {
  const { context } = loadUI();
  const source = fs.readFileSync(path.join(__dirname, 'static/js/accounts.js'), 'utf8');
  // Desktop builds the cell by hand; the mobile card reuses buildStatusMarkup.
  // Both have to carry it, or one layout drops the recovery time.
  assert.match(source, /cooldown\.innerHTML = buildCooldownMarkup\(acc\)/);
  assert.match(source, /if \(cooldown\.innerHTML\) tdStatus\.appendChild\(cooldown\)/);
  assert.match(source, /\$\{buildCooldownMarkup\(acc\)\}/);
  assert.equal(typeof context.buildCooldownMarkup, 'function');
});

// --- 创建时间 ---------------------------------------------------------------

test('the creation time renders as a calendar date', () => {
  const { context } = loadUI();
  assert.equal(strip(context.buildCreatedMarkup({ created_at: '2026-09-20T15:27:37.19946088Z' })), '2026-09-20');
  // A row with no creation time must not render the epoch.
  assert.equal(strip(context.buildCreatedMarkup({})), '-');
  assert.equal(strip(context.buildCreatedMarkup({ created_at: '0001-01-01T00:00:00Z' })), '-');
});

// --- 表格与卡片都带齐三列 ---------------------------------------------------

test('the desktop header and the mobile card both carry the three columns', () => {
  const source = fs.readFileSync(path.join(__dirname, 'static/js/accounts.js'), 'utf8');
  for (const label of ['今日/累计 Tokens', '创建时间']) {
    assert.ok(source.includes(label), `accounts.js never renders ${label}`);
  }
  // The mobile card is a template string, so a column present only in the
  // desktop renderer is a column a phone never sees. The end marker is the
  // call site, not the first textual hit — the function's own definition
  // contains the same substring.
  const start = source.indexOf('<div class="account-mobile-grid">');
  const end = source.indexOf('${buildMobileEmailMarkup(acc)}', start);
  assert.ok(start > 0 && end > start, 'the mobile card template was not found');
  const card = source.slice(start, end);
  assert.match(card, /今日\/累计 Tokens/);
  assert.match(card, /创建时间/);
});

// --- Cline 等级 / 配额 ------------------------------------------------------

test('the Cline tier badge reads the plan the upstream actually answered', () => {
  const { context } = loadUI();
  // "free" is a verdict the plan endpoint returned, not an inference from the
  // catalog: the feed publishes the free list to every account, subscriber
  // included, so it cannot tell the two apart.
  const free = context.subscriptionBadge({ account_type: 'cline', cline_plan: 'free', cline_model_ids: ['a'] });
  assert.equal(free.text, '免费');
  assert.match(free.tip, /没有套餐记录/);
  // A subscriber keeps the plan name; flattening it to 免费 would hide it.
  const paid = context.subscriptionBadge({ account_type: 'cline', cline_plan: 'Cline Pass', cline_model_ids: ['a'] });
  assert.equal(paid.text, 'Cline Pass');
});

test('a Cline account whose tier was never read is not labelled free', () => {
  const { context } = loadUI();
  const badge = context.subscriptionBadge({ account_type: 'cline', cline_model_ids: ['a', 'b'] });
  assert.notEqual(badge.text, '免费');
  assert.equal(badge.text, '未同步');
});
