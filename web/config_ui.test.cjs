const {test}=require('node:test');
const assert=require('node:assert/strict');
const same=(a,b)=>assert.equal(JSON.stringify(a),JSON.stringify(b));
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

// Load config.js in a DOM stand-in. Only the pure helpers matter here: the
// anonymous-allowlist parser and the USD <-> tick conversion decide what the
// admin plane sends to the server.
function loadConfig(){
 const nodes=new Map();
 const node=id=>{if(!nodes.has(id))nodes.set(id,{value:'',checked:false});return nodes.get(id)};
 const context=vm.createContext({
  console,Date,Math,Number,String,Array,Object,JSON,isNaN,parseInt,parseFloat,
  setTimeout(){},setInterval(){},fetch(){return Promise.resolve({ok:true,json:()=>({})})},
  window:{matchMedia:()=>({matches:false}),addEventListener(){}},
  document:{readyState:'loading',addEventListener(){},getElementById:node,createElement:()=>({}),querySelectorAll:()=>[]},
  encodeData:v=>String(v),
 });
 // config.js declares its helpers at top level, so the harness only appends an
 // export for the pure functions it wants to assert on.
 let src=fs.readFileSync(path.join(__dirname,'static/js/config.js'),'utf8');
 src+='\nglobalThis.probe={parseAnonymousAllowIPs,ticksToUSD,usdToTicks,formatUSD,periodSuffix};\n';
 vm.runInContext(src,context);
 return {api:context.probe,node};
}

// Arrays built inside the vm belong to that realm, so a strict deep comparison
// would fail on the prototype rather than on the value.
const plain=v=>Array.from(v);

test('anonymous_allow_ips is parsed into a trimmed list, and an empty box means nobody',()=>{
 const {api,node}=loadConfig();
 node('cfg_anonymous_allow_ips').value=' 161.118.140.32 \n\n203.77.252.0/24\n';
 assert.deepStrictEqual(plain(api.parseAnonymousAllowIPs()),['161.118.140.32','203.77.252.0/24']);
 node('cfg_anonymous_allow_ips').value='   \n ';
 assert.deepStrictEqual(plain(api.parseAnonymousAllowIPs()),[]);
 node('cfg_anonymous_allow_ips').value='';
 assert.deepStrictEqual(plain(api.parseAnonymousAllowIPs()),[]);
});

test('USD and ticks convert both ways without losing the integer ledger',()=>{
 const {api}=loadConfig();
 assert.equal(api.usdToTicks(0.5),5_000_000_000);
 assert.equal(api.usdToTicks(0),0);
 assert.equal(api.usdToTicks(''),0);
 assert.equal(api.usdToTicks(-1),0);
 assert.equal(api.ticksToUSD(5_000_000_000),0.5);
 assert.equal(api.ticksToUSD(0),0);
 assert.equal(api.formatUSD(0.5),'$0.50');
 // A round trip through the ledger stays an integer.
 assert.equal(Number.isInteger(api.usdToTicks(api.ticksToUSD(9_000_000_000))),true);
});

test('a billing policy line reads in dollars and names the period',()=>{
 const {api}=loadConfig();
 assert.equal(api.periodSuffix({billing_period_days:30}),' · 30 天账期');
 assert.equal(api.periodSuffix({billing_period_days:0}),'');
 assert.equal(api.periodSuffix({}),'');
});
