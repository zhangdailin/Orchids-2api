const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');
function element(tag='div'){
 return {tag,children:[],textContent:'',classList:{add(){},remove(){},toggle(){}},appendChild(n){this.children.push(n);return n},replaceChildren(){this.children=[]},addEventListener(){},setAttribute(){}};
}
function load(){
 const context=vm.createContext({console,URLSearchParams,document:{readyState:'loading',addEventListener(){}},window:{}});
 context.document.createElement=element;
 let src=fs.readFileSync(path.join(__dirname,'static/js/logs.js'),'utf8');
 src=src.replace(/\}\)\(\);\s*$/,'globalThis.review={sectionLabel,renderBundle};})();');
 vm.runInContext(src,context);return context.review;
}
const allText=n=>[n.textContent,...n.children.map(allText)].join(' ');
test('diagnostic attempt labels distinguish request, response, status and read failure',()=>{
 const api=load();
 for(const [name,label] of [['request.json','请求'],['response.txt','响应内容'],['result.json','HTTP 状态'],['read_error.json','响应读取错误']]){
  assert.equal(api.sectionLabel({name:'upstream_002_'+name}),'上游尝试 2 · '+label);
 }
 assert.match(api.sectionLabel({name:'4_upstream_sse.jsonl'}),/Protobuf 解码/);
});
test('bundle renders both attempts without hiding truncation or interpreting response markup',()=>{
 const api=load(),container=element();
 api.renderBundle(container,{bytes:200,duration_ms:31,truncated:true,sections:[
  {name:'upstream_001_request.json',payload:'first',bytes:5},
  {name:'upstream_001_read_error.json',payload:'read failed',bytes:11},
  {name:'upstream_002_response.txt',payload:'<script>example</script>',bytes:24,truncated:true},
 ]},'24 小时');
 assert.match(allText(container),/上游尝试 1/);assert.match(allText(container),/上游尝试 2/);
 assert.match(allText(container),/已截断/);assert.match(allText(container),/请求耗时 31 ms/);
 const details=container.children.filter(n=>n.tag==='details');assert.equal(details[1].open,true);
 assert.equal(details[2].children[1].textContent,'<script>example</script>');
});
