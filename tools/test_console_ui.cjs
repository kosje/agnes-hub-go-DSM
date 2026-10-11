// Exercise the real embedded UI with a small DOM/API adapter (no dependencies).
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert/strict');
const root = path.resolve(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'internal/web/static/console.html'), 'utf8');
const script = [...html.matchAll(/<script[^>]*>([\s\S]*?)<\/script>/g)].map(m => m[1]).join('\n');
const nodes = new Map();
const requests = [], opened = [];
let store = { public_base_url: 'https://hub.example.com:52325', client_media_url: '',
  video_cache_max_mb: 4096, auto_intent: {video_stream_wait_sec: 900}, chat_password_set: true };
function decode(s) { return s.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&'); }
function node(id) {
  if (nodes.has(id)) return nodes.get(id);
  let markup = '';
  const item = {value:'', checked:false, disabled:false, textContent:'', onclick:null,
    classList:{add(){},remove(){},toggle(){}}, focus(){},scrollIntoView(){},querySelectorAll(){return []}};
  Object.defineProperty(item,'innerHTML',{get:()=>markup,set:s=>{markup=s; mount(s);}});
  nodes.set(id,item); return item;
}
function mount(markup) {
  for (const m of markup.matchAll(/<[^>]+\bid="([^"]+)"[^>]*>/g)) {
    const item=node(m[1]); const value=/\bvalue="([^"]*)"/.exec(m[0]);
    if(value) item.value=decode(value[1]);
    item.checked=/\bchecked\b/.test(m[0]);
  }
  for(const m of markup.matchAll(/<select[^>]+id="([^"]+)"[^>]*>([\s\S]*?)<\/select>/g)) {
    const opts=[...m[2].matchAll(/<option\b([^>]*)>/g)]; const selected=opts.find(o=>/\bselected\b/.test(o[1]))||opts[0];
    if(selected) node(m[1]).value=decode((/\bvalue="([^"]*)"/.exec(selected[1])||['',''])[1]);
  }
}
const sandbox = {console, setTimeout,clearTimeout,URL,Date,navigator:{},location:{reload(){}},
  document:{getElementById:id=>nodes.get(id)||null,querySelectorAll:()=>[]},
  window:{addEventListener(){},open:url=>opened.push(url)},confirm:()=>true,prompt:()=>null,
  fetch:async (url, opts={})=>{
    const body=opts.body?JSON.parse(opts.body):null; requests.push({url,body});
    let result={ok:true};
    if(url==='/api/settings' && body) store={...store,...body};
    if(url==='/api/settings' && !body) result=store;
    if(url==='/api/media-base') result={base_url:store.public_base_url,source:'configured',client_media_mode:'auto',url_for_client:store.public_base_url+'/api/chat/images/example.png',sample_image_url:store.public_base_url+'/api/chat/images/example.png'};
    if(url==='/api/update/status') result={current_version:'1.0.31',checkable:true,enabled:false,suite_managed:true,repo:'kosje/agnes-hub-go-DSM',update_hint:'package center'};
    if(url==='/api/update/check') result={latest_version:'1.0.32',is_update_available:true};
    return {ok:true,text:async()=>JSON.stringify(result)};
  }};
node('app'); node('view');
const ctx=vm.createContext(sandbox);vm.runInContext(script,ctx);
vm.runInContext('toast=()=>{}; refresh=async()=>{};',ctx);
function settings(){sandbox.fixture=store;vm.runInContext('state.settings=fixture; document.getElementById("view").innerHTML=viewSettings(); bindSettingsEvents();',ctx);}
(async()=>{
  settings();
  assert.equal(node('s_baseurl').value,'https://hub.example.com:52325');
  assert.equal(node('s_cmedia').value,'');
  assert.equal(String(node('s_vswait').value),'900');
  assert.equal(String(node('s_vid_mb').value),'4096');
  // Saving another preference must include existing address/media values.
  await node('btnSaveSettings').onclick();
  const saved=requests.filter(r=>r.url==='/api/settings' && r.body).at(-1).body;
  assert.equal(saved.public_base_url, 'https://hub.example.com:52325');
  assert.equal(saved.client_media_url,'');
  assert.equal(saved.auto_intent.video_stream_wait_sec,900);
  assert(!Object.hasOwn(saved,'chat_password_clear'));
  node('s_baseurl').value='https://nas.example.com:5443';node('s_cmedia').value='local';
  await node('btnSaveSettings').onclick();settings();
  assert.equal(node('s_baseurl').value,'https://nas.example.com:5443');
  assert.equal(node('s_cmedia').value,'local');
  await node('btnMediaBase').onclick();assert(node('mediaBaseBox').innerHTML.includes('https://nas.example.com:5443'));
  // Both export buttons must honor their label, especially redaction.
  await node('btnExport').onclick();assert.equal(opened.at(-1),'/api/export?redact=1');
  await node('btnExportFull').onclick();assert.equal(opened.at(-1),'/api/export');
  node('s_chat_pw').value=node('s_chat_pw_confirm').value='chat-test-password';
  await node('btnChatPw').onclick();assert.equal(requests.at(-1).body.chat_password,'chat-test-password');
  await node('btnChatPwClear').onclick();assert.equal(requests.at(-1).body.chat_password_clear,true);
  vm.runInContext('state.session={logged_in:true,must_change_password:true};',ctx);await ctx.boot();
  assert(nodes.has('btnFc'));assert(nodes.has('fcOld'));
  assert(!html.includes('id="btnApply"'));
  node('view').innerHTML=ctx.viewAbout();ctx.bindAboutEvents();
  await new Promise(resolve=>setImmediate(resolve));
  assert(node('updKpi').innerHTML.includes('1.0.32'));
  await node('btnChk').onclick({currentTarget:node('btnChk')});assert(node('updKpi').innerHTML.includes('1.0.32'));
  assert(!requests.some(r=>r.url==='/api/update/apply'));
  node('view').innerHTML=ctx.viewAccounts();ctx.bindUpstreamModels();
  await node('btnUpModels').onclick();assert.equal(requests.at(-1).url,'/api/models/upstream');
  sandbox.jobFixture=[{job_id:'job-1',status:'completed',url:'https://nas.example.com:5443/api/chat/videos/'+ 'a'.repeat(64)+'.mp4'}];
  vm.runInContext('state.jobs=jobFixture; document.getElementById("view").innerHTML=viewJobs();',ctx);
  const play={dataset:{play:sandbox.jobFixture[0].url},onclick:null};
  sandbox.document.querySelectorAll=selector=>selector==='button[data-play]'?[play]:[];
  ctx.bindJobEvents();play.onclick();assert(node('jobPlayer').innerHTML.includes(sandbox.jobFixture[0].url));
  const fnNames=[...script.matchAll(/^(?:async )?function (\w+)\(/gm)].map(m=>m[1]);
  assert.equal(new Set(fnNames).size,fnNames.length,'duplicate top-level functions');
  console.log('Console UI: saved address/media values, round trip, diagnostics, export, chat password, initial-password screen and version checks PASS');
})().catch(err=>{console.error(err);process.exitCode=1;});
