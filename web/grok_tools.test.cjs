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

test('Grok capability fallback reads aggregate availability including internal Console capacity', async () => {
  let requested = '';
  const context = capabilityContext({
    ok: true,
    status: 200,
    json: async () => ({ counts: { build: 1, web: 0, console: 2 } }),
  });
  const originalFetch = context.fetch;
  context.fetch = async (url) => { requested = url; return originalFetch(url); };
  await context.loadGrokCapabilities();
  assert.equal(requested, '/api/grok/availability');
  assert.deepEqual(JSON.parse(JSON.stringify(context.result.counts)), { build: 1, web: 0, console: 2 });
  assert.equal(context.result.loaded, true);
  assert.equal(context.result.failed, false);
  assert.equal(context.hasGrokCapability('chat'), true);
  assert.equal(context.hasGrokCapability('video'), true);
});

test('Grok capability fallback fails open for invalid and failed availability responses', async () => {
  for (const response of [
    { ok: true, status: 200, json: async () => ({ counts: { build: 1, web: -1, console: 0 } }) },
    { ok: false, status: 503, json: async () => ({}) },
  ]) {
    const context = capabilityContext(response);
    await context.loadGrokCapabilities();
    assert.equal(context.result.loaded, true);
    assert.equal(context.result.failed, true);
    assert.equal(context.hasGrokCapability('video'), true);
  }
});
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

test('video operations use their matching API and preserve native request fields', async () => {
  const fn = source.slice(source.indexOf('  async function createVideoTask('),source.indexOf('  async function stopVideoTask('));
  for (const action of ['generate','edit','extend']) {
    let sent;
    const values = {videoAction:action,videoRatio:'16:9',videoReferenceURL:'',videoReferenceVoice:'',videoSourceURL:'https://example.com/source.mp4'};
    const ctx = vm.createContext({chatState:{routes:[{id:'grok-imagine-video',provider:'console'}]},
      toolInferencePrefix:()=>'/grok/v1',toolAuthHeaders:headers=>headers,
      stagedVideoInput:async(_id,url)=>({url}), videoState:{referenceFileID:'',sourceFileID:''},
      document:{getElementById:id=>({value:values[id]})},handleUnauthorized:()=>false,
      fetch:async(path,options)=>{sent={path,body:JSON.parse(options.body)};return {ok:true,json:async()=>({request_id:'video_1'})};},
    });
    vm.runInContext(fn,ctx);
    assert.equal(await ctx.createVideoTask({model:'grok-imagine-video',prompt:'test',seconds:6,resolution_name:'720p',input_references:[]}), 'video_1');
    assert.equal(sent.path, '/grok/v1/videos/'+({generate:'generations',edit:'edits',extend:'extensions'})[action]);
    if(action==='generate') assert.equal(sent.body.aspect_ratio,'16:9');
    else {assert.equal(sent.body.video.url,values.videoSourceURL);assert.equal(sent.body.resolution,undefined);}
    assert.equal(sent.body.duration,action==='edit'?undefined:6);
  }
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

test('Grok responses payload pins the fixed-reasoning console model to summary only', () => {
  const route = { id: 'grok-4.20-0309-reasoning', provider: 'console', upstream_model: 'grok-4.20-0309-reasoning' };
  const result = buildPayloadContext(
    { model: route.id, routes: [route] },
    { reasoningEffort: 'high', messages: [{ role: 'user', content: 'hello' }] },
  );
  // The model owns its reasoning level, so no effort is sent; the summary is.
  assert.equal(result.reasoning.effort, undefined);
  assert.equal(result.reasoning.summary, 'auto');
});

test('Grok responses payload never asks for a summary when reasoning is disabled', () => {
  const result = buildPayloadContext(
    { model: 'grok-4.5' },
    { reasoningEffort: 'none', messages: [{ role: 'user', content: 'hello' }] },
  );
  assert.deepEqual(JSON.parse(JSON.stringify(result.reasoning)), { effort: 'none' });
});

test('Grok responses payload only sends sampling where the backend accepts it', () => {
  const route = { id: 'console/grok-4.5', provider: 'console', upstream_model: 'grok-4.5' };
  const consoleResult = buildPayloadContext(
    { model: route.id, routes: [route] },
    { messages: [{ role: 'user', content: 'hi' }] },
  );
  assert.equal(consoleResult.temperature, 0.8);
  assert.equal(consoleResult.top_p, 0.95);
  // Build forwards unknown fields upstream verbatim, so sampling stays off.
  const buildResult = buildPayloadContext({ model: 'grok-4.6' }, { messages: [{ role: 'user', content: 'hi' }] });
  assert.equal(buildResult.temperature, undefined);
  assert.equal(buildResult.top_p, undefined);
});

test('JSZip is loaded only when the image batch download is used', () => {
  const template = fs.readFileSync(path.join(__dirname, 'templates/pages/grok-tools.html'), 'utf8');
  const imagine = fs.readFileSync(path.join(__dirname, 'static/js/grok-imagine.js'), 'utf8');
  assert.doesNotMatch(template, /<script[^>]+jszip/i, 'JSZip must not block the initial page load');
  assert.match(imagine, /function loadJSZip\(\)/, 'the image downloader has no lazy JSZip loader');
  assert.match(imagine, /await loadJSZip\(\)/, 'batch download does not await the lazy JSZip loader');
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

test('Grok model UI retains Console fixed reasoning behavior', () => {
  const sync = source.slice(source.indexOf('  function syncChatModelUI()'), source.indexOf('  function renderChatModelDropdown()'));
  const session = { reasoningEffort: 'high' };
  const effort = element(); effort.value = ''; effort.replaceChildren = function(...children) { this.children = children; };
  const document = { getElementById: id => id === 'grokReasoningEffort' ? effort : null, createElement: () => ({}) };
  const route = { id: 'grok-4.20-0309-reasoning', provider: 'console', upstream_model: 'grok-4.20-0309-reasoning' };
  const context = vm.createContext({ document, chatState: { model: route.id, routes: [route] }, activeChatSession: () => session });
  vm.runInContext(`${sync}\nsyncChatModelUI();`, context);
  assert.equal(effort.disabled, true);
  assert.equal(effort.value, '');
  assert.equal(session.reasoningEffort, '');
});

test('Grok payload omits Web search when route explicitly rejects backend search', () => {
  const route = { id: 'grok-4.6', supports_backend_search: false };
  const result = buildPayloadContext({ model: route.id, routes: [route] }, { webSearch: true, xSearch: true, messages: [{ role: 'user', content: 'hi' }] });
  assert.deepEqual(JSON.parse(JSON.stringify(result.tools)), [{ type: 'x_search' }]);
});

test('client key auth helpers scope history without persisting the secret', () => {
  const helper = source.slice(source.indexOf('  const toolAuthState'), source.indexOf('  const cacheOnlineState'));
  const context = vm.createContext({});
  vm.runInContext(helper, context);
  vm.runInContext('toolAuthState.mode="client"; toolAuthState.apiKey="sk-secret";', context);
  assert.equal(context.toolHistoryScope().startsWith('key-'), true);
  assert.equal(context.toolHistoryScope().includes('sk-secret'), false);
  assert.equal(context.toolAuthHeaders({Accept:'x'}).Authorization, 'Bearer sk-secret');
  assert.equal(context.toolInferencePrefix(), '/v1');
});
