// Run with: node --test web/layout_phone.test.cjs
//
// The content column's width is derived from a custom property that subtracts the
// sidebar rail:
//
//   --content-max: min(3200px, calc(100% - var(--sidebar-w)));
//   .main-content { width: 100%; max-width: var(--content-max); }
//
// That subtraction is only valid while the sidebar is a fixed rail. Below the
// 900px breakpoint the sidebar becomes an off-canvas drawer and occupies no room
// in the flow, so subtracting 248px from a 320px viewport asks for 72px of
// content — and at 390px it asked for 142px. Every admin page rendered as a
// narrow strip of wrapped text beside an empty screen, which is the phone
// breakage these assertions pin down.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const MAIN_CSS = path.join(__dirname, 'static', 'css', 'main.css');
const css = fs.readFileSync(MAIN_CSS, 'utf8');

const SIDEBAR_MIN = 248; // the clamp() floor of --sidebar-w
const DRAWER_BREAKPOINT = 900; // max-width at which .sidebar leaves the flow

// The declared value of a custom property, comments stripped.
function customProperty(name) {
  const match = css.match(new RegExp(`--${name}:\\s*([^;]+);`));
  assert.ok(match, `--${name} is declared in main.css`);
  return match[1].replace(/\/\*[\s\S]*?\*\//g, '').trim();
}

// Every `@media (max-width: Npx) { ... }` block in a stylesheet, with its width and
// body, by source order. These stylesheets nest no media queries, so brace counting
// is enough.
function mediaBlocks(source) {
  const blocks = [];
  const opener = /@media[^{]*max-width:\s*(\d+)px[^{]*\{/g;
  let match;
  while ((match = opener.exec(source)) !== null) {
    let depth = 1;
    let i = opener.lastIndex;
    while (i < source.length && depth > 0) {
      if (source[i] === '{') depth++;
      else if (source[i] === '}') depth--;
      i++;
    }
    blocks.push({ width: Number(match[1]), source: match[0], body: source.slice(opener.lastIndex, i - 1) });
  }
  return blocks;
}

// The bodies of every max-width block at or below a viewport, by source order.
function maxWidthBlocks(viewport) {
  return mediaBlocks(css).filter((b) => b.width >= viewport).map((b) => b.body);
}

function has(block, pattern) {
  return pattern.test(block);
}

test('the phone drawer breakpoint releases the content column from the sidebar subtraction', () => {
  const blocks = maxWidthBlocks(DRAWER_BREAKPOINT);
  assert.ok(
    blocks.some((b) => has(b, /\.main-content\s*\{[^}]*max-width:\s*none/)),
    'a max-width:900px block must set .main-content { max-width: none } so ' +
      'min(3200px, 100% - 248px) cannot cap a phone column at 142px',
  );
  assert.ok(
    blocks.some((b) => has(b, /\.main-content\s*\{[^}]*width:\s*100%/)),
    'the same block must set .main-content { width: 100% }',
  );
});

test('no phone breakpoint re-introduces the sidebar subtraction', () => {
  const value = customProperty('content-max');
  if (!value.includes('--sidebar-w')) return; // nothing to guard

  // Walk the drawer breakpoint and narrower, then the phone breakpoints inside it:
  // none of them may apply a column cap that subtracts the rail, because the rail
  // is not in the flow there.
  for (const viewport of [DRAWER_BREAKPOINT, 640]) {
    for (const block of maxWidthBlocks(viewport)) {
      const override = block.match(/--content-max:\s*([^;]+)/);
      if (!override) continue;
      const applied = override[1].replace(/\/\*[\s\S]*?\*\//g, '').trim();
      assert.ok(
        !applied.includes('--sidebar-w'),
        `a max-width:${viewport}px block sets --content-max: ${applied}; at that ` +
          'width the sidebar is an off-canvas drawer and subtracting its 248px ' +
          'leaves a negative column',
      );
    }
  }
});

test('every admin page links a cache-busted main.css', () => {
  const pageDir = path.join(__dirname, 'templates', 'pages');
  const pages = fs.readdirSync(pageDir).filter((f) => f.endsWith('.html'));
  assert.ok(pages.length > 0, 'page templates exist');

  // main.css is served with `immutable` when the URL carries a version, so a
  // stylesheet fix that does not change the query string never reaches a browser
  // that already fetched the broken bytes.
  const versions = new Set();
  for (const page of [...pages.map((p) => path.join(pageDir, p)), path.join(__dirname, 'static', 'login.html')]) {
    const html = fs.readFileSync(page, 'utf8');
    const match = html.match(/main\.css\?v=([^"']+)/);
    assert.ok(match, `${path.basename(page)} links a versioned main.css`);
    versions.add(match[1]);
  }
  assert.equal(versions.size, 1, `all pages share one main.css version, got ${[...versions].join(', ')}`);
});

// ---------------------------------------------------------------------------
// Two boxes were sized by their content instead of by their card. Both escaped,
// and the shell's `overflow-x: hidden` (the phone rule that stops one wide row
// from zooming the whole console out) meant the extra width was silently cut
// off rather than scrolled to. Neither is visible in a DOM assertion, so these
// pin the declarations that keep the boxes inside their parents.
// ---------------------------------------------------------------------------

const opsCss = fs.readFileSync(path.join(__dirname, 'static', 'css', 'ops.css'), 'utf8');
const grokCss = fs.readFileSync(path.join(__dirname, 'static', 'css', 'grok-tools.css'), 'utf8');
const opsJs = fs.readFileSync(path.join(__dirname, 'static', 'js', 'ops.js'), 'utf8');

test('the trend charts are width-constrained instead of sized by their aspect ratio', () => {
  const rule = opsCss.match(/\.ops-chart\s*\{([^}]*)\}/);
  assert.ok(rule, '.ops-chart is declared in ops.css');
  // Comments are stripped first: the rule documents itself, and prose about width
  // must not be mistaken for the declaration.
  const body = rule[1].replace(/\/\*[\s\S]*?\*\//g, ' ');
  // With width:auto, `aspect-ratio` + `min-height` let Chrome derive the width:
  // 2.6/1 and min-height 190px produced a 494px box inside a 336px card.
  assert.ok(
    /(^|;)\s*width:\s*100%/.test(body),
    '.ops-chart must state width:100%; otherwise min-height and aspect-ratio ' +
      'size it from the ratio (measured: 494px wide inside a 336px card)',
  );
  assert.match(body, /aspect-ratio:/, '.ops-chart still sizes its height from the ratio');
});

test('the capability strip wraps instead of running past its banner', () => {
  const blocks = [];
  const opener = /@media[^{]*max-width:\s*(\d+)px[^{]*\{/g;
  let m;
  while ((m = opener.exec(grokCss)) !== null) {
    let depth = 1;
    let i = opener.lastIndex;
    while (i < grokCss.length && depth > 0) {
      if (grokCss[i] === '{') depth++;
      else if (grokCss[i] === '}') depth--;
      i++;
    }
    blocks.push({ width: Number(m[1]), body: grokCss.slice(opener.lastIndex, i - 1) });
  }
  const phone = blocks.filter((b) => b.width <= 760);
  assert.ok(phone.length > 0, 'grok-tools.css has a phone breakpoint');
  assert.ok(
    phone.some((b) => /\.capability-badges\s*\{[^}]*flex-wrap:\s*wrap/.test(b.body)),
    'the badges are five fixed ~68px chips (356px of nowrap flex) and the banner is ' +
      '336px at 360px, so .capability-badges must wrap inside a phone breakpoint',
  );
});

test('the phone card layouts are driven by the labels the rows actually carry', () => {
  // ops.js stamps a class and a header label on every value cell; ops.css turns the
  // row into a card and prints that label with ::before. This asserts the two halves
  // of that contract still exist, because either one alone renders a card of
  // unlabelled numbers.
  for (const name of ['MATRIX_CELLS', 'ALERT_CELLS']) {
    assert.match(opsJs, new RegExp(`const ${name} = \\[`), `${name} is declared in ops.js`);
  }
  assert.match(opsJs, /td\.dataset\.label = cell\.label;/, 'matrix cells carry their header label');
  assert.match(opsJs, /td\.dataset\.label = cell\.label/, 'alert cells carry their header label');

  const cards = mediaBlocks(opsCss).filter((b) => /\.ops-table tbody tr\s*\{[^}]*display:\s*grid/.test(b.body));
  assert.equal(cards.length, 1, 'exactly one breakpoint turns a matrix/alert row into a card');
  const card = cards[0];
  assert.match(card.body, /content:\s*attr\(data-label\)/, 'the card prints the cell label');
  assert.match(card.body, /\.ops-table thead\s*\{\s*display:\s*none/, 'the header row goes away');
  assert.match(card.body, /\.ops-table,\s*[\s\S]*?\.ops-table td\s*\{\s*display:\s*block/, 'cells stop being table cells');
  for (const cls of ['ops-alert-time', 'ops-alert-status', 'ops-alert-level', 'ops-alert-channel', 'ops-alert-target', 'ops-alert-detail']) {
    assert.ok(card.body.includes(`.${cls}`), `the phone card styles the ${cls} cell`);
  }

  // The matrix does not degrade gracefully below this width: at 700px its 40 history
  // blocks claim the room the columns need and every cell computes to 0px wide. That
  // makes the card breakpoint load bearing, so pin it to the drawer breakpoint the
  // shell already changes shape at.
  assert.equal(
    card.width,
    DRAWER_BREAKPOINT,
    `the card layout must start at the ${DRAWER_BREAKPOINT}px drawer breakpoint (got ${card.width}px); ` +
      'below it the matrix columns collapse to zero width',
  );
});

