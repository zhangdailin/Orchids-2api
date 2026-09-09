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
    const deltas = [{reasoning_content:'first'},{content:'answer'},{reasoning_content:'second'},{content:'end'}, {x_grok_search:{id:'s',type:'x_search_call'},x_grok_search_done:true}];
    let body = deltas.map(delta => 'data: '+JSON.stringify({choices:[{delta}]})+'\n\n').join('');
    body += failed ? 'data: {"error":{"message":"interrupted"}}\n\n' : 'data: [DONE]\n\n';
    const session = {messages:[]};
    const ctx = vm.createContext({ session, AbortController,TextDecoder,fetch:async()=>new Response(body), chatState:{},
      setChatSendButtonState(){},updateChatStatus(){},buildChatPayload:()=>({}),handleUnauthorized:()=>false,
      requestAnimationFrame:()=>1,cancelAnimationFrame(){},trimChatSessionMessages:()=>0,saveChatSessions(){},renderChatSessions(){},
    });
    vm.runInContext(request,ctx); await ctx.requestChatCompletion(session,null);
    assert.equal(session.messages.length,1);
    assert.equal(session.messages[0].content,'answerend');
    assert.equal(session.messages[0].reasoning,'firstsecond');
    assert.equal(session.messages[0].tools[0].status,'completed');
  }
});

function streamContext(session, body, statuses) {
  const request = source.slice(source.indexOf('  async function requestChatCompletion('), source.indexOf('  async function retryAssistantMessage('));
  const ctx = vm.createContext({ session, AbortController, TextDecoder, fetch:async()=>new Response(body), chatState:{},
    setChatSendButtonState(){}, updateChatStatus:(text,type)=>statuses.push([text,type]), buildChatPayload:()=>({}), handleUnauthorized:()=>false,
    requestAnimationFrame:()=>1, cancelAnimationFrame(){}, trimChatSessionMessages:()=>0, saveChatSessions(){}, renderChatSessions(){} });
  vm.runInContext(request, ctx);
  return ctx;
}

test('failed search keeps its failure status after the finish sweep', async () => {
  const statuses = [];
  const frames = [
    {choices:[{delta:{x_grok_search:{id:'s1',type:'web_search_call',status:'in_progress',action:{query:'q'}}}}]},
    {choices:[{delta:{x_grok_search:{id:'s1',type:'web_search_call',status:'failed',action:{query:'q'}}},x_grok_search_done:true}]},
    {choices:[{delta:{},finish_reason:'stop'}]},
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
    {choices:[{delta:{x_grok_search:{id:'a',type:'x_search_call',action:{query:'q1'}}}}]},
    {choices:[{delta:{x_grok_search:{id:'b',type:'x_search_call',status:'completed',action:{query:'q2'}},x_grok_search_done:true}}]},
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
    {choices:[{delta:{content:'partial'}}]},
    {choices:[{delta:{},finish_reason:'length'}]},
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
  const deltas = [{reasoning_content:'first'},{content:'answer'},{reasoning_content:'second'},{content:'end'}];
  const body = deltas.map(d=>'data: '+JSON.stringify({choices:[{delta:d}]})+'\r\n\r\n').join('')+'data: [DONE]\r\n\r\n';
  const session = {messages:[]};
  await streamContext(session, body, statuses).requestChatCompletion(session, null);
  assert.equal(session.messages.length,1);
  assert.equal(session.messages[0].content,'answerend');
  assert.equal(session.messages[0].reasoning,'firstsecond');
  assert.equal(statuses[statuses.length-1][0],'完成');
});

test('trailing DONE without a blank line still completes', async () => {
  const statuses = [];
  const body = 'data: '+JSON.stringify({choices:[{delta:{content:'hi'}}]})+'\n\ndata: [DONE]\n';
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

test('Grok chat payload carries per-session reasoning, search, and cache settings', () => {
  const start = source.indexOf('  function buildChatPayload()');
  const end = source.indexOf('  function syncChatSessionSettings(', start);
  const buildPayload = source.slice(start, end);
  const session = {
    promptCacheKey: 'grok-tools-test', reasoningEffort: 'low', webSearch: true, xSearch: true,
    messages: [{ role: 'user', content: 'hello' }],
  };
  const context = vm.createContext({
    chatState: { model: 'grok-4.6' },
    activeChatSession: () => session,
    document: { getElementById(id) {
      return { value: id === 'grokTempRange' ? '0.8' : id === 'grokTopPRange' ? '0.95' : '' };
    } },
  });
  vm.runInContext(`${buildPayload}\nresult = buildChatPayload();`, context);
  assert.equal(context.result.reasoning_effort, 'low');
  assert.equal(context.result.prompt_cache_key, 'grok-tools-test');
  assert.deepEqual(JSON.parse(JSON.stringify(context.result.x_responses_tools)), [{ type: 'web_search' }, { type: 'x_search' }]);
});
