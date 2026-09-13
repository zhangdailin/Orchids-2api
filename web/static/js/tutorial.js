/* 使用教程 — help centre behaviour.
 *
 * Loaded after common.js (which owns copyToClipboard + the toast) and after the
 * markup. This file is the single source of truth for channel content: the quick
 * start zone and the channel cards are both rendered from the registry below, so
 * no channel block is duplicated in the template and the caveat text lives in one
 * place. Plain ES5 — no framework, no build step.
 */
(function () {
  'use strict';

  // The API root the operator is currently looking at, e.g. http://host:3002
  var API_BASE = window.location.origin.replace(/\/$/, '');

  // How long the inline 已复制 confirmation stays in its reserved slot.
  var FLASH_MS = 1600;

  // Channels in the order the quick-start picker offers them.
  var CHANNELS = [
    {
      key: 'warp',
      label: 'Warp',
      badge: 'badge-warp',
      path: '/warp/v1',
      models: ['claude-4-5-sonnet'],
      protocols: ['claude', 'openai'],
      claudeCode: true,
      accountCredential: '在「账号管理」页面登录后由服务端保存，页面不显示会话密钥。'
    },
    {
      key: 'puter',
      label: 'Puter',
      badge: 'badge-puter',
      path: '/puter/v1',
      models: ['claude-opus-4-5'],
      protocols: ['claude', 'openai'],
      claudeCode: true,
      accountCredential: '当前只需要填写 auth_token。'
    },
    {
      key: 'workbuddy',
      label: 'WorkBuddy',
      badge: 'badge-workbuddy',
      path: '/workbuddy/v1',
      models: ['default-model', 'hy3'],
      protocols: ['claude', 'openai'],
      claudeCode: true,
      accountCredential: '只支持官方网页登录；批量导入仍可通过 API 使用会话 refreshToken。'
    },
    {
      key: 'qoder',
      label: 'Qoder',
      badge: 'badge-qoder',
      path: '/qoder/v1',
      models: ['Qwen3.7-Max', 'DeepSeek-V4-Pro'],
      protocols: ['claude', 'openai'],
      claudeCode: true,
      accountCredential: '只支持官方设备授权登录（qoder.com）；不提供 PAT 入口。'
    },
    {
      key: 'grok',
      label: 'Grok',
      badge: 'badge-grok',
      path: '/grok/v1',
      models: ['grok-3'],
      protocols: ['openai'],
      claudeCode: false,
      accountCredential: '在「账号管理」页面登录后由服务端保存，页面不显示会话密钥。'
    }
  ];

  var PROTOCOL_LABELS = { claude: 'Claude', openai: 'OpenAI' };

  var CLIENT_CREDENTIAL_PARTS = [
    '面板 API Key：',
    { code: 'Authorization: Bearer sk-...' },
    '；Anthropic Messages 客户端也可发送 ',
    { code: 'x-api-key: sk-...' },
    '。'
  ];

  // ---------------------------------------------------------------------------
  // Content: one plain switch per channel
  // ---------------------------------------------------------------------------

  // Claude Code ignores the /v1 suffix. The rule is stated once and reused by
  // every channel whose protocol list includes Claude.
  function claudeCodeCaveat(channel) {
    return {
      title: 'Claude Code 用户注意',
      parts: [
        '配置 API 地址时请使用 ',
        { code: API_BASE + '/' + channel.key, tone: 'amber' },
        '，',
        { strong: '不要', tone: 'red' },
        '添加 ',
        { code: '/v1', tone: 'red' },
        ' 后缀。'
      ]
    };
  }

  function channelCaveats(key) {
    switch (key) {
      case 'puter':
        return [
          {
            title: 'Puter 账号说明',
            parts: ['Puter 账号当前只需要填写 ', { code: 'auth_token', tone: 'amber' }, '。']
          },
          {
            title: 'Puter 模型说明',
            parts: [
              'Puter 当前采用参考仓库里的“无前缀主模型”清单，已包含 Claude / OpenAI / Gemini / Grok / DeepSeek / Mistral 等主模型族；但像 ',
              { code: 'openrouter:', tone: 'amber' },
              '、',
              { code: 'togetherai:', tone: 'amber' },
              ' 这类聚合源前缀模型暂时不直接暴露到模型管理里。'
            ]
          }
        ];
      case 'workbuddy':
        return [
          {
            title: 'WorkBuddy 账号说明',
            parts: [
              '国际版（',
              { code: 'www.workbuddy.ai', tone: 'amber' },
              '）',
              { strong: '只支持官方登录', tone: 'amber' },
              '：在账号页面手动点击「使用 WorkBuddy 官方网页登录」，在官方页面完成授权后账号会自动保存，服务端不接触密码，并会自动同步该账号的模型目录与额度；编辑已有账号时也可用它重新授权。该渠道不提供手填凭证（批量导入仍可通过 API 使用会话 refreshToken）。'
            ]
          }
        ];
      case 'qoder':
        return [
          {
            title: 'Qoder 账号说明',
            parts: [
              '该渠道',
              { strong: '只支持 OAuth 设备授权登录', tone: 'amber' },
              '：在账号页面点击「使用 Qoder 官方网页登录」，在 ',
              { code: 'qoder.com', tone: 'amber' },
              ' 完成授权后账号会自动保存，服务端不接触密码，并会自动同步该账号的模型目录。',
              { strong: '该渠道不提供 PAT（个人访问令牌）入口', tone: 'amber' },
              '，也不接受手填凭证。'
            ]
          }
        ];
      case 'grok':
        return [
          {
            title: 'Grok 路由说明',
            parts: ['Grok 与其他渠道保持一致，使用 ', { code: '/grok/v1', tone: 'amber' }, ' 前缀。']
          }
        ];
      case 'warp':
      default:
        return [];
    }
  }

  function quickNote(channel) {
    switch (channel.key) {
      case 'puter':
        return '账号只需要填写 auth_token。';
      case 'workbuddy':
        return '账号走「使用 WorkBuddy 官方网页登录」，不提供手填凭证。';
      case 'qoder':
        return '账号走「使用 Qoder 官方网页登录」（OAuth 设备授权），不提供 PAT 入口。';
      case 'grok':
        return '与其他渠道保持一致，使用 /grok/v1 前缀。';
      case 'warp':
      default:
        return '客户端地址带 /v1 后缀；Claude Code 用不带 /v1 的地址。';
    }
  }

  // ---------------------------------------------------------------------------
  // Small DOM helpers. Values are assigned as text nodes and attributes, so no
  // markup string is ever built from data.
  // ---------------------------------------------------------------------------
  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) { node.className = className; }
    if (text !== undefined && text !== null) { node.textContent = String(text); }
    return node;
  }

  // Renders a caveat body from a list of strings and {code|strong, tone} parts,
  // so the emphasis of the original callouts survives without HTML strings.
  function rich(parts) {
    var fragment = document.createDocumentFragment();
    var i;
    for (i = 0; i < parts.length; i++) {
      var part = parts[i];
      if (typeof part === 'string') {
        fragment.appendChild(document.createTextNode(part));
      } else if (part.code) {
        fragment.appendChild(el('code', 'doc-inline-code' + (part.tone ? ' doc-inline-code-' + part.tone : ''), part.code));
      } else if (part.strong) {
        fragment.appendChild(el('span', 'doc-strong' + (part.tone ? ' doc-strong-' + part.tone : ''), part.strong));
      }
    }
    return fragment;
  }

  function findChannel(key) {
    var i;
    for (i = 0; i < CHANNELS.length; i++) {
      if (CHANNELS[i].key === key) { return CHANNELS[i]; }
    }
    return null;
  }

  function clientAddress(channel) {
    return API_BASE + channel.path;
  }

  function claudeCodeAddress(channel) {
    return API_BASE + '/' + channel.key;
  }

  function protocolLabels(channel) {
    var labels = [];
    var i;
    for (i = 0; i < channel.protocols.length; i++) {
      labels.push(PROTOCOL_LABELS[channel.protocols[i]] || channel.protocols[i]);
    }
    return labels;
  }

  function exampleConfig(channel) {
    return JSON.stringify({
      base_url: clientAddress(channel),
      api_key: 'sk-...',
      model: channel.models[0]
    }, null, 2);
  }

  function setText(id, text) {
    var node = document.getElementById(id);
    if (node) { node.textContent = text; }
  }

  function copyButton(label, targetId) {
    var button = el('button', 'tut-copy');
    button.type = 'button';
    button.setAttribute('data-copy-target', targetId);
    button.appendChild(el('span', 'tut-copy-label', label));
    var flag = el('span', 'tut-copy-flag', '');
    flag.setAttribute('aria-live', 'polite');
    button.appendChild(flag);
    return button;
  }

  function urlValue(id, url) {
    var wrap = el('div', 'tut-copy-row');
    var code = el('code', 'tut-url', url);
    code.id = id;
    wrap.appendChild(code);
    wrap.appendChild(copyButton('复制地址', id));
    return wrap;
  }

  function configValue(id, text) {
    var wrap = el('div', 'tut-config');
    var pre = el('pre', 'tut-code', text);
    pre.id = id;
    wrap.appendChild(pre);
    wrap.appendChild(copyButton('复制配置', id));
    return wrap;
  }

  function row(label, value) {
    var line = el('div', 'tut-row');
    line.appendChild(el('span', 'tut-row-label', label));
    var cell = el('div', 'tut-row-value');
    cell.appendChild(value);
    line.appendChild(cell);
    return line;
  }

  function rowText(label, text) {
    var cell = el('div', 'tut-row-value');
    cell.appendChild(document.createTextNode(text));
    return row(label, cell);
  }

  function rowParts(label, parts) {
    var cell = el('div', 'tut-row-value');
    cell.appendChild(rich(parts));
    return row(label, cell);
  }

  // Codes rendered one per element with a shared class, e.g. the model list.
  function codeRow(codes, className) {
    var wrap = el('div', 'tut-tag-row');
    var i;
    for (i = 0; i < codes.length; i++) {
      wrap.appendChild(el('code', className, codes[i]));
    }
    return wrap;
  }

  function protocolRow(channel) {
    var wrap = el('div', 'tut-tag-row');
    var labels = protocolLabels(channel);
    var i;
    for (i = 0; i < channel.protocols.length; i++) {
      wrap.appendChild(el('span', 'tag protocol-tag protocol-tag-' + channel.protocols[i], labels[i]));
    }
    return wrap;
  }

  function caveatsBlock(items) {
    var details = el('details', 'tut-caveats');
    details.appendChild(el('summary', 'tut-caveats-summary', '注意事项（' + items.length + '）'));
    var body = el('div', 'tut-caveat-body');
    var i;
    for (i = 0; i < items.length; i++) {
      var item = el('div', 'tut-caveat');
      item.appendChild(el('span', 'tut-caveat-title', items[i].title));
      var text = el('p', 'tut-caveat-text');
      text.appendChild(rich(items[i].parts));
      item.appendChild(text);
      body.appendChild(item);
    }
    details.appendChild(body);
    return details;
  }

  // ---------------------------------------------------------------------------
  // Rendering
  // ---------------------------------------------------------------------------
  function renderPicker() {
    var host = document.getElementById('tutPicker');
    if (!host) { return; }
    var i;
    for (i = 0; i < CHANNELS.length; i++) {
      var channel = CHANNELS[i];
      var button = el('button', 'tut-picker-btn');
      button.type = 'button';
      button.setAttribute('data-channel', channel.key);
      button.setAttribute('aria-pressed', 'false');
      button.appendChild(el('span', 'tag ' + channel.badge, channel.label));
      host.appendChild(button);
    }
  }

  function renderCard(channel) {
    var card = el('article', 'tut-card');

    var title = el('h3', 'tut-card-title');
    title.appendChild(el('span', 'tag ' + channel.badge, channel.label));
    card.appendChild(title);

    card.appendChild(row('接入地址', urlValue('tutCardUrl-' + channel.key, clientAddress(channel))));

    if (channel.claudeCode) {
      card.appendChild(row('Claude Code', urlValue('tutCardClaude-' + channel.key, claudeCodeAddress(channel))));
    }

    card.appendChild(row('协议', protocolRow(channel)));
    card.appendChild(rowParts('客户端凭据', CLIENT_CREDENTIAL_PARTS));
    card.appendChild(rowText('账号凭据', channel.accountCredential));
    card.appendChild(row('模型示例', codeRow(channel.models, 'doc-model-code')));
    card.appendChild(row('示例配置', configValue('tutCardConfig-' + channel.key, exampleConfig(channel))));

    var items = [];
    if (channel.claudeCode) { items.push(claudeCodeCaveat(channel)); }
    items = items.concat(channelCaveats(channel.key));
    card.appendChild(caveatsBlock(items));

    return card;
  }

  function renderCards() {
    var host = document.getElementById('tutChannels');
    if (!host) { return; }
    var i;
    for (i = 0; i < CHANNELS.length; i++) {
      host.appendChild(renderCard(CHANNELS[i]));
    }
  }

  function markPicker(activeKey) {
    var buttons = document.getElementById('tutPicker');
    if (!buttons) { return; }
    var items = buttons.getElementsByClassName('tut-picker-btn');
    var i;
    for (i = 0; i < items.length; i++) {
      var isActive = items[i].getAttribute('data-channel') === activeKey;
      items[i].className = isActive ? 'tut-picker-btn is-active' : 'tut-picker-btn';
      items[i].setAttribute('aria-pressed', isActive ? 'true' : 'false');
    }
  }

  function updateQuickStart(channel) {
    setText('tutQuickUrl', clientAddress(channel));
    setText('tutQuickConfig', exampleConfig(channel));
    setText('tutQuickNote', quickNote(channel) + ' 协议：' + protocolLabels(channel).join(' / ') + '。');

    var claudeRow = document.getElementById('tutQuickClaudeRow');
    if (claudeRow) {
      if (channel.claudeCode) {
        setText('tutQuickClaudeUrl', claudeCodeAddress(channel));
        claudeRow.hidden = false;
      } else {
        claudeRow.hidden = true;
      }
    }
  }

  function selectChannel(key) {
    var channel = findChannel(key) || CHANNELS[0];
    markPicker(channel.key);
    updateQuickStart(channel);
  }

  // ---------------------------------------------------------------------------
  // Copying: one delegated handler for buttons, plus the legacy click-to-copy
  // ---------------------------------------------------------------------------
  function flash(button) {
    var flags = button.getElementsByClassName('tut-copy-flag');
    if (!flags.length) { return; }
    var flag = flags[0];
    flag.textContent = '已复制';
    if (button.className.indexOf('is-copied') === -1) {
      button.className = button.className + ' is-copied';
    }
    if (button.__tutFlashTimer) {
      window.clearTimeout(button.__tutFlashTimer);
    }
    button.__tutFlashTimer = window.setTimeout(function () {
      flag.textContent = '';
      button.className = button.className.replace(' is-copied', '');
      button.__tutFlashTimer = null;
    }, FLASH_MS);
  }

  function copyFrom(button) {
    var id = button.getAttribute('data-copy-target');
    var target = id ? document.getElementById(id) : null;
    if (!target) { return; }
    if (typeof copyToClipboard === 'function') {
      copyToClipboard(target.textContent || '');
    }
    flash(button);
  }

  function ancestorWith(node, attribute) {
    while (node && node.nodeType === 1) {
      if (node.getAttribute && node.getAttribute(attribute)) { return node; }
      node = node.parentNode;
    }
    return null;
  }

  function onClick(event) {
    var node = event.target || event.srcElement;

    var copier = ancestorWith(node, 'data-copy-target');
    if (copier) {
      copyFrom(copier);
      return;
    }

    var picker = ancestorWith(node, 'data-channel');
    if (picker) {
      selectChannel(picker.getAttribute('data-channel'));
      return;
    }

    // The speed table keeps its old behaviour: clicking the address copies it
    // and lights up the copy button sitting beside it.
    if (node && node.nodeType === 1 && node.getAttribute && node.getAttribute('data-api-path')) {
      var parent = node.parentNode;
      var buttons = parent && parent.getElementsByClassName ? parent.getElementsByClassName('tut-copy') : [];
      if (buttons && buttons.length) {
        copyFrom(buttons[0]);
      } else if (typeof copyToClipboard === 'function') {
        copyToClipboard(node.textContent || '');
      }
    }
  }

  // ---------------------------------------------------------------------------
  // Wiring
  // ---------------------------------------------------------------------------
  function fillApiSpans() {
    var i;
    var bases = document.querySelectorAll('[data-api-base]');
    for (i = 0; i < bases.length; i++) {
      bases[i].textContent = API_BASE;
    }
    var paths = document.querySelectorAll('[data-api-path]');
    for (i = 0; i < paths.length; i++) {
      var node = paths[i];
      var url = API_BASE + (node.getAttribute('data-api-path') || '');
      node.textContent = url;
      node.title = '点击复制';
    }
  }

  function init() {
    fillApiSpans();
    renderPicker();
    renderCards();
    selectChannel(CHANNELS[0].key);
    document.addEventListener('click', onClick, false);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init, false);
  } else {
    init();
  }
})();
