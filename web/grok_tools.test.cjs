const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
const styles = fs.readFileSync(path.join(__dirname, 'static/css/grok-tools.css'), 'utf8');
const render = source.slice(source.indexOf('  function renderChatSessions()'), source.indexOf('  function syncChatModelUI()'));

test('chat model picker constrains long labels and dropdowns to the viewport', () => {
  assert.match(styles, /\.model-chip\s*\{[^}]*max-width:\s*min\(360px, 100%\)/s);
  assert.match(styles, /\.model-label\s*\{[^}]*text-overflow:\s*ellipsis/s);
  assert.match(styles, /\.model-dropdown\s*\{[^}]*max-width:\s*min\(420px, calc\(100vw - 32px\)\)/s);
  assert.match(styles, /\.model-dropdown\s*\{[^}]*overflow-y:\s*auto/s);
  assert.match(styles, /@media \(max-width: 760px\)[\s\S]*?\.model-dropdown\s*\{[^}]*position:\s*fixed[^}]*left:\s*max\(12px/s);
});

test('legacy reasoning migrates all blocks without dropping answers or history', () => {
  const helpers = source.slice(source.indexOf('  function normalizeAssistantMessage('), source.indexOf('  function saveChatSessions('));
  const ctx = vm.createContext({}); vm.runInContext(helpers, ctx);
  const migrated = ctx.normalizeAssistantMessage({role:'assistant',content:'<think>one</think>A<think>two</think>B'});
  assert.equal(migrated.content, 'AB'); assert.equal(migrated.reasoning, 'one\n\ntwo');
  assert.match(ctx.assistantDisplay(migrated), /one\n\ntwo/);
  assert.equal(ctx.normalizeAssistantMessage(migrated), migrated);
  // New-format messages (reasoning present) are never re-scanned even if the body quotes think tags.
  const partial = ctx.normalizeAssistantMessage({role:'assistant',content:'A<think>second</think>B',reasoning:'first'});
  assert.equal(ctx.normalizeAssistantMessage(partial), partial);
  assert.equal(partial.content,'A<think>second</think>B'); assert.equal(partial.reasoning,'first');
  // Mid-body think tags in a body-only message are literal content (e.g. code examples).
  const literal = ctx.normalizeAssistantMessage({role:'assistant',content:'see <think>示例</think> below'});
  assert.equal(ctx.normalizeAssistantMessage(literal), literal);
  assert.equal(literal.content,'see <think>示例</think> below');
  // Legacy messages leading with reasoning still migrate, tolerating leading whitespace.
  const lead = ctx.normalizeAssistantMessage({role:'assistant',content:'<think>a</think>ANS'});
  assert.equal(lead.content,'ANS'); assert.equal(lead.reasoning,'a');
  const spaced = ctx.normalizeAssistantMessage({role:'assistant',content:'  <think>a</think>ANS'});
  assert.equal(spaced.content,'ANS'); assert.equal(spaced.reasoning,'a');
});

test('stream stores interleaved reasoning and partial failure exactly once', async () => {
  const request = source.slice(source.indexOf('  async function requestChatCompletion('), source.indexOf('  async function retryAssistantMessage('));
  for (const failed of [false,true]) {
    const events = [
      {type:'response.reasoning_summary_text.delta',delta:'first'},
      {type:'response.output_text.delta',delta:'answer'},
      {type:'response.reasoning_summary_text.delta',delta:'second'},
      {type:'response.output_text.delta',delta:'end'},
      {type:'response.output_item.added',item:{type:'x_search_call',id:'s',status:'in_progress',action:{query:'q'}}},
      {type:'response.output_item.done',item:{type:'x_search_call',id:'s',status:'completed',action:{query:'q'}}},
    ];
    let body = events.map(event => 'data: '+JSON.stringify(event)+'\n\n').join('');
    body += failed ? 'data: {"type":"response.failed","response":{"error":{"message":"interrupted"}}}\n\n' : 'data: [DONE]\n\n';
    const session = {messages:[]};
    const ctx = vm.createContext({ session, AbortController,TextDecoder,fetch:async()=>new Response(body), chatState:{modelsLoaded:true,model:'test-model'},
      toolInferencePrefix:()=>'/grok/v1',toolAuthHeaders:headers=>headers,
      setChatSendButtonState(){},updateChatStatus(){},buildResponsesPayload:()=>({}),handleUnauthorized:()=>false,
      requestAnimationFrame:()=>1,cancelAnimationFrame(){},trimChatSessionMessages:()=>0,saveChatSessions(){},renderChatSessions(){},
    });
    vm.runInContext(request,ctx); await ctx.requestChatCompletion(session,null);
    assert.equal(session.messages.length,1);
    assert.equal(session.messages[0].content,'answerend');
    assert.equal(session.messages[0].reasoning,'firstsecond');
    assert.equal(session.messages[0].tools[0].status,'completed');
  }
});

function capabilityContext(response) {
  const start = source.indexOf('  async function loadGrokCapabilities()');
  const end = source.indexOf('  function formatDateTime(', start);
  const loadCapabilities = source.slice(start, end);
  const badges = new Map();
  const state = { loaded: false, failed: false, counts: { build: 0, web: 0, console: 0 } };
  const context = vm.createContext({
    fetch: async () => response,
    console: { debug() {} },
    grokCapabilityState: state,
    handleUnauthorized: () => false,
    hasGrokCapability(tab) {
      if (!state.loaded || state.failed) return true;
      if (tab === 'cache') return true;
      if (context.chatState.modelsLoaded) return context.chatState.capabilities[tab] === true;
      if (tab === 'chat') return state.counts.build + state.counts.web + state.counts.console > 0;
      return Boolean(context.chatState.capabilities[tab]) || state.counts.web + state.counts.console > 0;
    },
    applyGrokCapabilityControls() {},
    updateGrokCapabilityPresentation() {},
    currentGrokToolTab: () => 'chat',
    document: {
      querySelector(selector) {
        if (selector === '#grokToolsTabs .tab-item.active') return { dataset: { tab: 'chat' } };
        if (selector.startsWith('[data-capability=')) {
          if (!badges.has(selector)) badges.set(selector, { textContent: '', classList: { toggle() {} } });
          return badges.get(selector);
        }
        return null;
      },
      getElementById() { return null; },
    },
    window: { location: {} },
    chatState: { modelsLoaded: false, capabilities: {} },
  });
  vm.runInContext(`${loadCapabilities}\nresult = grokCapabilityState;`, context);
  context.result = state;
  return context;
}

function streamContext(session, body, statuses) {
  const request = source.slice(source.indexOf('  async function requestChatCompletion('), source.indexOf('  async function retryAssistantMessage('));
  const ctx = vm.createContext({ session, AbortController, TextDecoder, fetch:async()=>new Response(body), chatState:{modelsLoaded:true,model:'test-model'},
    toolInferencePrefix:()=>'/grok/v1', toolAuthHeaders:headers=>headers,
    setChatSendButtonState(){}, updateChatStatus:(text,type)=>statuses.push([text,type]), buildResponsesPayload:()=>({}), handleUnauthorized:()=>false,
    requestAnimationFrame:()=>1, cancelAnimationFrame(){}, trimChatSessionMessages:()=>0, saveChatSessions(){}, renderChatSessions(){} });
  vm.runInContext(request, ctx);
  return ctx;
}

test('failed search keeps its failure status after the finish sweep', async () => {
  const statuses = [];
  const frames = [
    {type:'response.output_item.added',item:{type:'web_search_call',id:'s1',status:'in_progress',action:{query:'q'}}},
    {type:'response.output_item.done',item:{type:'web_search_call',id:'s1',status:'failed',action:{query:'q'}}},
    {type:'response.completed',response:{output:[]}},
  ];
  const body = frames.map(f=>'data: '+JSON.stringify(f)+'\n\n').join('')+'data: [DONE]\n\n';
  const session = {messages:[]};
  await streamContext(session, body, statuses).requestChatCompletion(session, null);
  assert.equal(session.messages.length,1);
  assert.equal(session.messages[0].tools[0].status,'failed');
});

test('search status follows the item status and done flag', async () => {
  const statuses = [];
  const frames = [
    {type:'response.output_item.added',item:{type:'x_search_call',id:'a',action:{query:'q1'}}},
    {type:'response.output_item.done',item:{type:'x_search_call',id:'b',status:'completed',action:{query:'q2'}}},
  ];
  const body = frames.map(f=>'data: '+JSON.stringify(f)+'\n\n').join('')+'data: [DONE]\n\n';
  const session = {messages:[]};
  await streamContext(session, body, statuses).requestChatCompletion(session, null);
  const mapped = session.messages[0].tools.map(t=>t.status);
  assert.equal(mapped[0],'in_progress'); assert.equal(mapped[1],'completed');
});

test('length-truncated replies are flagged instead of reported as done', async () => {
  const statuses = [];
  const frames = [
    {type:'response.output_text.delta',delta:'partial'},
    {type:'response.incomplete',response:{output:[]}},
  ];
  const body = frames.map(f=>'data: '+JSON.stringify(f)+'\n\n').join('')+'data: [DONE]\n\n';
  const session = {messages:[]};
  await streamContext(session, body, statuses).requestChatCompletion(session, null);
  assert.equal(session.messages.length,1);
  assert.equal(session.messages[0].content,'partial');
  const last = statuses[statuses.length-1];
  assert.match(last[0], /截断/);
  assert.equal(last[1],'error');
  assert.ok(!statuses.some(([text])=>text==='完成'));
});

test('CRLF SSE frames are parsed end to end', async () => {
  const statuses = [];
  const events = [{type:'response.reasoning_summary_text.delta',delta:'first'},{type:'response.output_text.delta',delta:'answer'},{type:'response.reasoning_summary_text.delta',delta:'second'},{type:'response.output_text.delta',delta:'end'}];
  const body = events.map(e=>'data: '+JSON.stringify(e)+'\r\n\r\n').join('')+'data: [DONE]\r\n\r\n';
  const session = {messages:[]};
  await streamContext(session, body, statuses).requestChatCompletion(session, null);
  assert.equal(session.messages.length,1);
  assert.equal(session.messages[0].content,'answerend');
  assert.equal(session.messages[0].reasoning,'firstsecond');
  assert.equal(statuses[statuses.length-1][0],'完成');
});

test('trailing DONE without a blank line still completes', async () => {
  const statuses = [];
  const body = 'data: '+JSON.stringify({type:'response.output_text.delta',delta:'hi'})+'\n\ndata: [DONE]\n';
  const session = {messages:[]};
  await streamContext(session, body, statuses).requestChatCompletion(session, null);
  assert.equal(session.messages.length,1);
  assert.equal(session.messages[0].content,'hi');
  assert.equal(statuses[statuses.length-1][1],'ok');
});

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

test('Grok responses payload carries per-session reasoning, search, and cache settings', () => {
  const session = {
    promptCacheKey: 'grok-tools-test', reasoningEffort: 'low', webSearch: true, xSearch: true,
    messages: [{ role: 'user', content: 'hello' }],
  };
  const result = buildPayloadContext({ model: 'grok-4.6' }, session);
  assert.equal(result.model, 'grok-4.6');
  assert.equal(result.stream, true);
  assert.equal(result.store, false);
  // The upstream page always pairs a reasoning request with a summary request.
  assert.deepEqual(JSON.parse(JSON.stringify(result.reasoning)), { effort: 'low', summary: 'auto' });
  assert.equal(result.prompt_cache_key, 'grok-tools-test');
  assert.deepEqual(JSON.parse(JSON.stringify(result.tools)), [{ type: 'web_search' }, { type: 'x_search' }]);
  assert.deepEqual(JSON.parse(JSON.stringify(result.input)), [
    { type: 'message', role: 'user', content: [{ type: 'input_text', text: 'hello' }] },
  ]);
});

function buildPayloadContext(chatState, session) {
  const start = source.indexOf('  function buildResponsesPayload()');
  const end = source.indexOf('  function syncChatSessionSettings(', start);
  const context = vm.createContext({
    chatState,
    activeChatSession: () => session,
    document: { getElementById(id) {
      return { value: id === 'grokTempRange' ? '0.8' : id === 'grokTopPRange' ? '0.95' : '' };
    } },
  });
  vm.runInContext(`${source.slice(start, end)}\nresult = buildResponsesPayload();`, context);
  return context.result;
}

test('Grok responses payload never asks for a summary when reasoning is disabled', () => {
  const result = buildPayloadContext(
    { model: 'grok-4.5' },
    { reasoningEffort: 'none', messages: [{ role: 'user', content: 'hello' }] },
  );
  assert.deepEqual(JSON.parse(JSON.stringify(result.reasoning)), { effort: 'none' });
});

test('chat persistence is scoped, debounced, and evicts oldest sessions to limits', () => {
  const start = source.indexOf('  const chatSessionLimit = 50;');
  const end = source.indexOf('  function activeChatSession()', start);
  const writes = [];
  const timers = [];
  const sessions = Array.from({ length: 55 }, (_, i) => ({ id: `s${i}`, updatedAt: i, messages: [{ role: 'user', content: 'x'.repeat(100) }] }));
  const context = vm.createContext({
    chatState: { sessions, activeId: 's54', model: 'm', persistenceTimer: null },
    chatStorageKey: 'history', toolHistoryScope: () => 'key-fingerprint',
    localStorage: { setItem: (key, value) => writes.push([key, value]), getItem: () => null },
    TextEncoder, setTimeout: fn => { timers.push(fn); return timers.length; }, clearTimeout() {},
    updateChatStatus() {}, createChatSession: () => ({ id: 'new', messages: [] }),
    createPromptCacheKey: () => 'cache', normalizeAssistantMessage: value => value,
  });
  vm.runInContext(source.slice(start, end), context);
  context.saveChatSessions(); context.saveChatSessions();
  assert.equal(writes.length, 0, 'writes must be debounced');
  timers.at(-1)();
  assert.equal(writes.length, 1);
  assert.equal(writes[0][0], 'history:key-fingerprint');
  const stored = JSON.parse(writes[0][1]);
  assert.equal(stored.sessions.length, 50);
  assert.equal(stored.sessions[0].id, 's54');
  assert.equal(stored.sessions.at(-1).id, 's5');
  assert.ok(new TextEncoder().encode(writes[0][1]).byteLength <= 4 * 1024 * 1024);
});

test('chat persistence evicts oldest sessions until payload is about 4 MiB', () => {
  const start = source.indexOf('  const chatSessionLimit = 50;');
  const end = source.indexOf('  function activeChatSession()', start);
  const huge = '界'.repeat(800000);
  const context = vm.createContext({
    chatState: { sessions: Array.from({length: 8}, (_, i) => ({id:`s${i}`, updatedAt:i, messages:[{role:'user',content:huge}]})), activeId:'s7', model:'m', persistenceTimer:null },
    chatStorageKey:'history', toolHistoryScope:()=> 'admin', TextEncoder,
    localStorage:{setItem(){},getItem(){return null;}}, setTimeout, clearTimeout,
    updateChatStatus(){}, createChatSession:()=>({id:'new',messages:[]}), createPromptCacheKey:()=> 'cache', normalizeAssistantMessage:v=>v,
  });
  vm.runInContext(source.slice(start, end), context);
  const bounded = context.boundedChatPayload();
  assert.ok(new TextEncoder().encode(bounded.payload).byteLength <= 4 * 1024 * 1024);
  assert.ok(bounded.sessions.length < 8);
  assert.equal(bounded.sessions[0].id, 's7');
});

test('stale aborted SSE cannot overwrite a newer request', async () => {
  const request = source.slice(source.indexOf('  async function requestChatCompletion('), source.indexOf('  async function retryAssistantMessage('));
  let releaseOld;
  let calls = 0;
  const oldRead = new Promise(resolve => { releaseOld = resolve; });
  const oldResponse = { ok:true, body:{ getReader:()=>({ read:()=>oldRead }) } };
  const freshBody = 'data: '+JSON.stringify({type:'response.output_text.delta',delta:'new'})+'\n\ndata: [DONE]\n\n';
  const session = {messages:[]};
  const state = {modelsLoaded:true, model:'m', requestGeneration:0};
  const ctx = vm.createContext({ session, AbortController, TextDecoder, chatState:state,
    fetch:async()=> ++calls === 1 ? oldResponse : new Response(freshBody),
    toolInferencePrefix:()=>'/grok/v1',toolAuthHeaders:h=>h,
    setChatSendButtonState(){},updateChatStatus(){},buildResponsesPayload:()=>({}),handleUnauthorized:()=>false,
    requestAnimationFrame:()=>1,cancelAnimationFrame(){},trimChatSessionMessages:()=>0,saveChatSessions(){},renderChatSessions(){},
  });
  vm.runInContext(request, ctx);
  const old = ctx.requestChatCompletion(session, null);
  const fresh = ctx.requestChatCompletion(session, null);
  await fresh;
  releaseOld({ value: new TextEncoder().encode('data: '+JSON.stringify({type:'response.output_text.delta',delta:'old'})+'\n\ndata: [DONE]\n\n'), done:false });
  await old;
  assert.deepEqual(session.messages.map(item => item.content), ['new']);
  assert.equal(state.sending, false);
});

test('Grok model metadata rebuilds reasoning choices and disables unsupported backend search', () => {
  const sync = source.slice(source.indexOf('  function syncChatModelUI()'), source.indexOf('  function renderChatModelDropdown()'));
  const session = { reasoningEffort: 'none', webSearch: true };
  const effort = element();
  effort.value = '';
  effort.replaceChildren = function(...children) { this.children = children; };
  const webSearch = { checked: true, disabled: false };
  const label = {};
  const document = {
    getElementById(id) { return { grokReasoningEffort: effort, grokWebSearch: webSearch, grokModelLabel: label }[id] || null; },
    createElement() { return {}; },
  };
  const route = { id: 'grok-4.6', provider: 'build', reasoning_efforts: ['low', 'high'], default_reasoning_effort: 'high', supports_reasoning_effort: true, supports_backend_search: false };
  const context = vm.createContext({ document, chatState: { model: route.id, routes: [route] }, activeChatSession: () => session });
  vm.runInContext(`${sync}\nsyncChatModelUI();`, context);
  assert.deepEqual(effort.children.map(option => option.value), ['', 'low', 'high']);
  assert.equal(effort.value, 'high');
  assert.equal(effort.disabled, false);
  assert.equal(session.reasoningEffort, 'high');
  assert.equal(webSearch.disabled, true);
  assert.equal(webSearch.checked, false);
  assert.equal(session.webSearch, false);
});

test('Grok payload omits Web search when route explicitly rejects backend search', () => {
  const route = { id: 'grok-4.6', supports_backend_search: false };
  const result = buildPayloadContext({ model: route.id, routes: [route] }, { webSearch: true, xSearch: true, messages: [{ role: 'user', content: 'hi' }] });
  assert.deepEqual(JSON.parse(JSON.stringify(result.tools)), [{ type: 'x_search' }]);
});


test('client key auth helpers scope history without persisting the secret', () => {
  const helper = source.slice(source.indexOf('  const toolAuthState'), source.indexOf('  const chatState'));
  const context = vm.createContext({});
  vm.runInContext(helper, context);
  vm.runInContext('toolAuthState.mode="client"; toolAuthState.apiKey="sk-secret";', context);
  assert.equal(context.toolHistoryScope().startsWith('key-'), true);
  assert.equal(context.toolHistoryScope().includes('sk-secret'), false);
  assert.equal(context.toolAuthHeaders({Accept:'x'}).Authorization, 'Bearer sk-secret');
  assert.equal(context.toolInferencePrefix(), '/v1');
});

test('chat branch helpers confirm trailing destructive changes and rotate cache keys', () => {
  const start = source.indexOf('  function chatMessageIndex(');
  const end = source.indexOf('  function startEditChatMessage(', start);
  const confirms = [];
  const session = { messages: [{role:'user'}, {role:'assistant'}, {role:'user'}], promptCacheKey: 'old', updatedAt: 0 };
  let saves = 0; let sessionRenders = 0; let threadRenders = 0;
  const context = vm.createContext({
    window: { confirm: text => { confirms.push(text); return true; } },
    createPromptCacheKey: () => 'new-cache', Date: { now: () => 42 },
    saveChatSessions: () => { saves += 1; }, renderChatSessions: () => { sessionRenders += 1; }, rerenderChatThread: () => { threadRenders += 1; },
    chatState: { sending: false }, activeChatSession: () => session,
  });
  vm.runInContext(source.slice(start, end), context);
  assert.equal(context.confirmTrailingMessages('编辑这条消息', 0), true);
  assert.equal(confirms.length, 0);
  assert.equal(context.confirmTrailingMessages('编辑这条消息', 2), true);
  assert.match(confirms[0], /后续 2 条消息/);
  context.truncateChatBranch(session, 1);
  assert.equal(session.messages.length, 1);
  assert.equal(session.promptCacheKey, 'new-cache');
  assert.equal(session.updatedAt, 42);
  assert.deepEqual([saves, sessionRenders, threadRenders], [1, 1, 1]);
});

test('message delete confirms and truncates the selected branch', () => {
  const start = source.indexOf('  function chatMessageIndex(');
  const end = source.indexOf('  function startEditChatMessage(', start);
  const session = { messages: [{role:'user'}, {role:'assistant'}, {role:'user'}], promptCacheKey: 'old' };
  let prompt = ''; let saved = 0;
  const context = vm.createContext({
    window: { confirm: text => { prompt = text; return true; } }, chatState: { sending: false },
    activeChatSession: () => session, createPromptCacheKey: () => 'rotated', Date,
    saveChatSessions: () => { saved += 1; }, renderChatSessions() {}, rerenderChatThread() {},
  });
  vm.runInContext(source.slice(start, end), context);
  const rows = [{}, {}, {}];
  const log = { querySelectorAll: () => rows };
  rows.forEach(row => { row.closest = () => log; });
  context.deleteChatMessage(rows[1]);
  assert.match(prompt, /后续 1 条消息/);
  assert.equal(session.messages.length, 1);
  assert.equal(session.promptCacheKey, 'rotated');
  assert.equal(saved, 1);
});

test('clear-current-session confirms, preserves the session, and rotates its cache key', () => {
  const start = source.indexOf('  function clearCurrentChatSession(');
  const end = source.indexOf('  function newChatSession(', start);
  const session = { id: 'keep-me', messages: [{role:'user'}, {role:'assistant'}], promptCacheKey: 'old' };
  let prompt = ''; let status = '';
  const context = vm.createContext({
    window: { confirm: text => { prompt = text; return true; } }, chatState: { sending: false },
    activeChatSession: () => session, createPromptCacheKey: () => 'rotated', Date,
    saveChatSessions() {}, renderChatSessions() {}, rerenderChatThread() {}, updateChatStatus: text => { status = text; },
  });
  vm.runInContext(source.slice(start, end), context);
  context.clearCurrentChatSession();
  assert.match(prompt, /2 条消息/);
  assert.equal(session.id, 'keep-me');
  assert.equal(session.messages.length, 0);
  assert.equal(session.promptCacheKey, 'rotated');
  assert.match(status, /已清空/);
});

test('editing a user message truncates its branch, saves, rotates cache, and regenerates', async () => {
  const start = source.indexOf('  function startEditChatMessage(');
  const end = source.indexOf('  function startEditAssistantMessage(', start);
  const messages = [
    { role: 'user', content: 'old' }, { role: 'assistant', content: 'a' },
    { role: 'user', content: 'later' }, { role: 'assistant', content: 'b' },
  ];
  const session = { messages, promptCacheKey: 'old-cache', updatedAt: 0 };
  const elements = [];
  const makeElement = tag => {
    const el = { tag, value: '', className: '', children: [], listeners: {},
      appendChild(child) { this.children.push(child); }, addEventListener(name, fn) { this.listeners[name] = fn; },
      focus() {}, select() {}, click() { return this.listeners.click?.(); },
    };
    elements.push(el); return el;
  };
  const bubble = makeElement('div');
  const actions = makeElement('div');
  const row = { querySelector: selector => selector === '.message-bubble' ? bubble : actions };
  let requested = 0; let saved = 0; let confirms = 0;
  const context = vm.createContext({
    chatState: { sending: false }, activeChatSession: () => session, chatMessageIndex: () => 0,
    document: { createElement: makeElement }, showToast() {}, rerenderChatThread() {}, renderChatSessions() {},
    confirmTrailingMessages: (_action, count) => { confirms = count; return true; },
    createPromptCacheKey: () => 'new-cache', Date: { now: () => 99 }, saveChatSessions: () => { saved += 1; },
    appendChatMessage: () => ({}), requestChatCompletion: async () => { requested += 1; },
  });
  vm.runInContext(source.slice(start, end), context);
  context.startEditChatMessage(row, 'old');
  const textarea = elements.find(el => el.tag === 'textarea');
  textarea.value = 'edited';
  const saveButton = elements.find(el => el.tag === 'button' && el.className === 'btn btn-primary');
  await saveButton.listeners.click();
  assert.equal(confirms, 3);
  assert.deepEqual(JSON.parse(JSON.stringify(session.messages)), [{ role: 'user', content: 'edited' }]);
  assert.equal(session.promptCacheKey, 'new-cache');
  assert.equal(session.updatedAt, 99);
  assert.equal(saved, 1);
  assert.equal(requested, 1);
});

test('retrying a historical assistant confirms branch truncation before regenerating', async () => {
  const start = source.indexOf('  async function retryAssistantMessage(');
  const end = source.indexOf('  function rerenderChatThread(', start);
  const original = [
    { role: 'user', content: 'u1' }, { role: 'assistant', content: 'a1' },
    { role: 'user', content: 'u2' }, { role: 'assistant', content: 'a2' },
  ];
  for (const accepted of [false, true]) {
    const session = { messages: original.map(item => ({...item})), promptCacheKey: 'old' };
    let trailing = -1; let requested = 0;
    const context = vm.createContext({
      chatState: { sending: false }, activeChatSession: () => session, chatMessageIndex: () => 1,
      confirmTrailingMessages: (_action, count) => { trailing = count; return accepted; }, showToast() {},
      createPromptCacheKey: () => 'new', Date, saveChatSessions() {}, renderChatSessions() {}, rerenderChatThread() {},
      appendChatMessage: () => ({}), requestChatCompletion: async () => { requested += 1; },
    });
    vm.runInContext(source.slice(start, end), context);
    await context.retryAssistantMessage({});
    assert.equal(trailing, 2);
    assert.equal(session.messages.length, accepted ? 1 : 4);
    assert.equal(requested, accepted ? 1 : 0);
    assert.equal(session.promptCacheKey, accepted ? 'new' : 'old');
  }
});

test('assistant local edit clears reasoning and tools and message controls expose delete and clear', () => {
  const assistantEdit = source.slice(source.indexOf('  function startEditAssistantMessage('), source.indexOf('  async function requestChatCompletion('));
  assert.match(assistantEdit, /msg\.reasoning = "";/);
  assert.match(assistantEdit, /msg\.tools = \[\];/);
  assert.match(assistantEdit, /confirmTrailingMessages\("编辑这条回复", trailing\)/);
  assert.match(assistantEdit, /session\.messages = messages\.slice\(0, rowIndex \+ 1\)/);
  assert.match(source, /deleteBtn\.addEventListener\("click", \(\) => deleteChatMessage\(row\)\)/);
  assert.match(source, /clearBtn\.addEventListener\("click", clearCurrentChatSession\)/);
  const template = fs.readFileSync(path.join(__dirname, 'templates/pages/grok-tools.html'), 'utf8');
  assert.match(template, /id="grokChatClearBtn"/);
});

test('a JSON 401 from a Grok handler is not mistaken for an expired console session', () => {
  const handler = source.slice(source.indexOf('  function handleUnauthorized('), source.indexOf('  function currentGrokToolTab('));
  // Only the session middleware answers plain text; handler denials are JSON.
  assert.match(handler, /contentType\.includes\("text\/plain"\)/);
  assert.doesNotMatch(handler, /url\.includes\("\/api\/"\)/);
  assert.match(handler, /window\.location\.href = "\/admin\/login\.html\?next="/);
  // Admin tool inference now lives on the session-authenticated namespace.
  assert.match(source, /return toolAuthState\.mode === "client" \? "\/v1" : "\/api\/grok\/tools\/v1"/);
});
