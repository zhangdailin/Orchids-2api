const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'static/js/grok-tools.js'), 'utf8');
const render = source.slice(source.indexOf('  function renderChatSessions()'), source.indexOf('  function syncChatModelUI()'));

test('legacy reasoning migrates all blocks without dropping answers or history', () => {
  const helpers = source.slice(source.indexOf('  function trimChatSessionMessages('), source.indexOf('  function saveChatSessions('));
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
  const session = {messages:Array.from({length:100}, (_,i)=>({role:i%2?'assistant':'user',content:String(i)}))};
  ctx.trimChatSessionMessages(session); assert.equal(session.messages.length,100);
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
    const ctx = vm.createContext({ session, AbortController,TextDecoder,fetch:async()=>new Response(body), chatState:{},
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
  const ctx = vm.createContext({ session, AbortController, TextDecoder, fetch:async()=>new Response(body), chatState:{},
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
