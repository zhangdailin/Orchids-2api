const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');
function element(tag='div'){
 return {tag,children:[],textContent:'',innerHTML:'',style:{},dataset:{},value:'',hidden:false,
 classList:{add(){},remove(){},toggle(){}},setAttribute(){},addEventListener(){},
 appendChild(node){this.children.push(node);return node},replaceChildren(...nodes){this.children=nodes;this.textContent=''},
 querySelector(selector){if(selector==='.ops-kpi-head')return this.children[0];return null},querySelectorAll(){return []}};
}
function loadOps(){
 const nodes=new Map();const node=id=>{if(!nodes.has(id))nodes.set(id,element());return nodes.get(id)};
 const context=vm.createContext({console,Date,URLSearchParams,setInterval(){},window:{},document:{readyState:'loading',addEventListener(){},getElementById:node,createElement:element,createElementNS:(_,tag)=>element(tag),querySelectorAll:()=>[]}});
 let src=fs.readFileSync(path.join(__dirname,'static/js/ops.js'),'utf8');
 src=src.replace(/\}\)\(\);\s*$/,'globalThis.review={state,renderHero,renderKpis,renderResources,liveBuckets};})();');vm.runInContext(src,context);
 return {api:context.review,node};
}
const text=node=>[node.textContent,...node.children.map(text)].join(' ');
function payload(series){return {available:true,until:'2026-09-13T08:30:30Z',window_minutes:180,totals:{requests:60,success:60},series};}
const current={minute:'2026-09-13T08:30:00Z',requests:60,input_tokens:600,output_tokens:300,usage_samples:60};
test('1min renders one data point, TPS and server-aligned time without insufficient-sample state',()=>{
 const {api,node}=loadOps();api.renderHero(payload([current]));
 assert.equal(node('opsLiveQpsNow').textContent,'2.00');assert.equal(node('opsLiveTpsNow').textContent,'30.0');
 assert.equal(node('opsSpark').children[0].tag,'svg');assert.doesNotMatch(text(node('opsSpark')),/样本不足/);
});
test('5min fills idle minutes and includes them in average, without treating stale traffic as current',()=>{
 const {api,node}=loadOps();api.state.liveWindow=5;api.renderHero(payload([{...current,minute:'2026-09-13T08:28:00Z'}]));
 assert.equal(api.liveBuckets(5).length,5);assert.equal(node('opsLiveQpsNow').textContent,'0.00');
 assert.equal(node('opsLiveQpsAvg').textContent,(60/270).toFixed(2));assert.equal(node('opsSpark').children[0].tag,'svg');
});
test('missing token measurements stay unknown, and disabled aggregation never shows fake live zeroes',()=>{
 const {api,node}=loadOps();api.renderHero(payload([{minute:current.minute,requests:60}]));assert.equal(node('opsLiveTpsNow').textContent,'未采集');
 api.renderHero({...payload([]),available:false});assert.equal(node('opsLiveQpsNow').textContent,'未采集');
});
test('P95-only historical data is never relabelled P99 or used for a failed-request cohort',()=>{
 const {api,node}=loadOps();const p=payload([]);p.totals={requests:1,success:1,duration_p95_ms:950,samples:1};
 api.renderKpis(p);assert.match(text(node('opsKpis')),/P95/);assert.doesNotMatch(text(node('opsKpis')),/P99/);
 api.state.outcome='failed';api.renderKpis(p);assert.doesNotMatch(text(node('opsKpis')),/950/);
});
test('resource cards retain host CPU, memory and RSS alongside Go metrics',()=>{
 const {api,node}=loadOps();api.renderResources({available:true,metrics:['主机 CPU','主机内存','进程内存 RSS'].map(label=>({label,value:'12%',available:true}))});
 assert.equal(node('opsResources').children.length,3);assert.match(text(node('opsResources')),/主机 CPU/);assert.match(text(node('opsResources')),/主机内存/);
});
function loadModels(){
 const list=element();const calls=[];
 const ctx=vm.createContext({console,document:{addEventListener(){},getElementById:id=>id==='modelsList'?list:null,querySelectorAll:()=>[]},window:{matchMedia:()=>({matches:false})},encodeData:encodeURIComponent,decodeData:decodeURIComponent,escapeHtml:String,showToast(){},confirm:()=>true,fetch:async(url,options)=>{calls.push({url,options});return {ok:true}}});
 vm.runInContext(fs.readFileSync(path.join(__dirname,'static/js/models.js'),'utf8'),ctx);
 vm.runInContext(`updateModelSummary=()=>{};renderModelRefreshSummary=()=>{};renderPagination=()=>{};updateRefreshButton=()=>{};loadModels=async()=>{};
 models=[{id:'warp-a',channel:'Warp',model_id:'one',status:'available'},{id:'grok-b',channel:'Grok',model_id:'two',status:'available'}];currentModelChannel='Warp';`,ctx);
 return {ctx,list,calls};
}
test('selected model remains checked after redraw and changing provider clears the selection',async()=>{
 const {ctx,list,calls}=loadModels();vm.runInContext(`modelsSelectedIds.add('warp-a');renderModels()`,ctx);
 assert.match(list.innerHTML,/data-action="row-select"[^>]+checked/);
 vm.runInContext(`filterModelsByChannel('Grok')`,ctx);assert.equal(vm.runInContext('modelsSelectedIds.size',ctx),0);
 await vm.runInContext(`runModelBatch('disable')`,ctx);assert.equal(calls.length,0);
});
test('batch action rejects stale cross-channel IDs even if selection state is corrupted',async()=>{
 const {ctx,calls}=loadModels();vm.runInContext(`currentModelChannel='Grok';modelsSelectedIds.add('warp-a');modelsSelectedIds.add('grok-b')`,ctx);
 await vm.runInContext(`runModelBatch('disable')`,ctx);assert.deepEqual(calls.map(c=>c.url),['/api/models/grok-b']);
});
