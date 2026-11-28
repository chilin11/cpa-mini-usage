'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');

// Execute the actual embedded dashboard script, not a copy of its formatter.
const html = fs.readFileSync(path.join(__dirname, '../web/index.html'), 'utf8');
const source = html.match(/<script>([\s\S]*?)<\/script>/)[1];
class Element {
  constructor(tag) { this.tag = tag; this.children = []; this._text = ''; this.attributes = {}; this.dataset = {}; this.style = {setProperty(){}}; this.classes = new Set(); this.classList = {add:(c)=>this.classes.add(c), remove:(c)=>this.classes.delete(c), toggle:(c,on)=>{if(on??!this.classes.has(c))this.classes.add(c);else this.classes.delete(c);}}; }
  get textContent() { return this._text; }
  set textContent(value) { this._text = String(value); this.children = []; }
  append(child) { if(child.parentNode)child.parentNode.children=child.parentNode.children.filter(n=>n!==child);child.parentNode=this;this.children.push(child); }
  replaceChildren(...children) { this.children.forEach(n=>n.parentNode=null);this.children=[];this._text='';children.forEach(n=>this.append(n)); }
  replaceWith(node) { const parent=this.parentNode,index=parent.children.indexOf(this);parent.children[index]=node;node.parentNode=parent;this.parentNode=null; }
  setAttribute(name,value) { this.attributes[name] = String(value);if(name==='id')this.id=value;if(name.startsWith('data-'))this.dataset[name.slice(5).replace(/-([a-z])/g,(_,c)=>c.toUpperCase())]=value; }
  getAttribute(name) { return this.attributes[name] ?? null; }
  addEventListener() {}
  focus() {}
}
function dashboard(stored = {}, fetcher = async () => ({ok:true,json:async()=>({})}), options={}) {
  const elements = new Map(), writes=[], intervals=[], events={};
  // Parse the real static markup so translations and accessible labels are tested too.
  const root=new Element('document'),stack=[root];
  for(const match of html.split('<script>')[0].matchAll(/<\/?[a-zA-Z][^>]*>|[^<]+/g)){
    const token=match[0];
    if(token.startsWith('</')){if(stack.length>1)stack.pop();continue;}
    if(token.startsWith('<')){
      const tag=token.match(/^<(\w+)/)?.[1];if(!tag)continue;const node=new Element(tag);
      for(const a of token.matchAll(/([\w-]+)="([^"]*)"/g))node.setAttribute(a[1],a[2]);
      if(node.id)elements.set(node.id,node);stack.at(-1).append(node);
      if(!['meta','link','path','input','br','hr'].includes(tag)&&!token.endsWith('/>'))stack.push(node);
    }else if(token.trim())stack.at(-1)._text+=token.trim();
  }
  const storage = {getItem(name){ return stored[name] ?? null; },setItem(name,value){writes.push([name,value]);stored[name]=value;}};
  let fakeNow=options.now??Date.now();const FakeDate=class extends Date {constructor(...args){super(...(args.length?args:[fakeNow]));}static now(){return fakeNow;}};
  const document={
    documentElement:descendants(root).find(n=>n.tag==='html'),hidden:false,
    getElementById(id) { const found=descendants(root).find(n=>n.id===id);if(found)return found;if(!elements.has(id))elements.set(id,new Element('div'));return elements.get(id); },
    querySelectorAll(selector) { return descendants(root).filter(n=>selector==='[data-reset-at]'?n.dataset.resetAt:selector==='[data-i18n]'?n.dataset.i18n:selector==='[aria-label]'?n.getAttribute('aria-label')!==null:false); },
    createElement(tag) { const node=new Element(tag);node.focus=()=>{document.activeElement=node;};return node; },
  };
  for(const node of descendants(root))node.focus=()=>{document.activeElement=node;};
  let alertTimeout;
  const context = vm.createContext({
    Intl, URL, URLSearchParams, AbortController, TextEncoder, TextDecoder, atob, Date:options.fakeClock?FakeDate:Date,
    location:{protocol:'https:',host:'cpa.test',origin:'https://cpa.test'}, navigator:{userAgent:'test-agent',language:options.language||'zh-CN',languages:options.languages},
    document, localStorage:storage, sessionStorage:{getItem(){return null;}}, fetch:fetcher, performance:{now:()=>fakeNow},
    setInterval(callback,ms){intervals.push({callback,ms});},
    setTimeout(callback,ms){
      if(callback.name==='renderAlerts')return alertTimeout={callback,at:fakeNow+ms};
      if(ms===20000)return 1;if(options.fakeClock)fakeNow+=ms;callback();
    },
    clearTimeout(timer){if(timer===alertTimeout)alertTimeout=undefined;}, addEventListener(name,callback){events[name]=callback;},
  });
  context.window=context;
  const instrumented = source.replace(/^load\(\);$/m, '').replace(/\}\)\(\);\s*$/, 'globalThis.dashboardTest = {tokens, fmt, remainingPercent, validEstimate, totalInput, requestMoney, loadHistory, historyParams, localMidnight, localDateValue, resetCountdown, updateCountdowns, acceptSnapshot, guessKey, load, refreshAccount, openReset, confirmReset, clearCooldown, confirmDisable, toggleAccount, saveWakeupConfig, runWakeupAll, runAccountWakeup, toggleAccountWakeupExclude, parseCronJS, nextCronRunsJS, buildCadenceCron, savePersistence, firstTokenTime, requestTPS, setLanguage, setAlert, setStatus, setTab, claudeResetDiagnostics, renderSample(sample, provider="") {data=sample; filter=provider; expanded="account"; render();}}; })();');
  vm.runInContext(instrumented, context);
  return {api: context.dashboardTest, elements, writes, document, intervals, events, advanceTime(ms){
    fakeNow+=ms;
    if(alertTimeout?.at<=fakeNow){const {callback}=alertTimeout;alertTimeout=undefined;callback();}
  }};
}
const cases = [
  [0, '0'], [12, '12'], [999, '999'], [1000, '1k'], [1234, '1.23k'],
  [12500, '12.5k'], [999994, '999.99k'], [999995, '1m'],
  [1000000, '1m'], [1250000, '1.25m'], [123456789, '123.46m'],
  [999994999, '999.99m'], [999999999, '1b'], [1000000000, '1b'],
  [2400000000, '2.4b'], [1234567890123, '1,234.57b'],
  ['2500000', '2.5m'], [null, '—'], [undefined, '—'], [NaN, '—'],
  [Infinity, '—'], [-1, '—'], ['', '—'], ['   ', '—'], ['invalid', '—'], [true, '—'],
];
for (const [input, expected] of cases) {
  test(`token format ${String(input)} => ${expected}`, () => {
    assert.equal(dashboard().api.tokens(input), expected);
  });
}
function descendants(element) { return [element, ...element.children.flatMap(descendants)]; }
test('all token displays use compact units, retaining full values in tooltips', () => {
  const {api, elements} = dashboard();
  api.renderSample({
    quotas: [{provider:'codex', name:'alice@example.com', auth_id:'account', five_hour:{used_percent:25, estimated_total_tokens:2400000000, estimated_total_cost:4}, seven_day:{used_percent:0}}],
    credentials:[{provider:'codex', name:'Account', auth_id:'account', requests:1234, failures:0, input:1250000, output:2400000000, cached:1234, total:2401250000, cost:3.5, models:[{model:'gpt', requests:1234, tokens:2401250000, cost:3.5}]}],
    recent:[{auth_id:'account', at:'2026-10-09T00:00:00Z', model:'gpt', total:1250000}],
  });
  const quotaNodes = descendants(elements.get('quota-cards'));
  assert(quotaNodes.some(n => n.textContent === '2.4b token' && n.title.startsWith('2,400,000,000 token\n')));
  assert(!quotaNodes.some(n => n.textContent === '1.8b token'));
  assert(quotaNodes.some(n => n.textContent === '—'));
  const requestNodes = descendants(elements.get('credential-table'));
  const cells = requestNodes.filter(n => n.tag === 'td');
  assert.deepEqual(cells.slice(2,8).map(n => n.textContent), ['1,234', '1.25m', '2.4b', '', '2.4b', '$3.50']);
  assert.equal(cells[6].title, '2,401,250,000 token');
  assert(requestNodes.some(n => n.textContent === '1,234 次请求 · 2.4b token · $3.50' && n.title === '2,401,250,000 token'));
  assert(descendants(elements.get('recent-table')).some(n => n.textContent === 'gpt'));
});

test('remaining percentage clamps safely and unknown data stays unknown', () => {
  const {api}=dashboard();
  for(const [used,want] of [[0,100],[25,75],[100,0],[110,0],[-5,100],[null,null],[undefined,null],['',null],[Infinity,null]])assert.equal(api.remainingPercent({used_percent:used}),want);
  assert.equal(api.validEstimate(4000000),4000000);
  for(const value of [null,undefined,Infinity,NaN,-1,'4000000','',true])assert.equal(api.validEstimate(value),null);
});
test('providers have independent named stacks with vertically ordered cards; absent manual reset stays hidden', () => {
  const {api,elements}=dashboard();
  const quotas=[{provider:'claude',name:'alice@example.com',manual_reset_remaining:2},{provider:'codex',name:'bob@example.com'},{provider:'codex',name:'carol@example.com',manual_reset_remaining:0}];
  api.renderSample({quotas});
  const stacks=elements.get('quota-cards').children,cards=visibleAccounts({elements});
  assert(stacks.every(stack=>stack.tag==='section'&&stack.className==='provider-stack'));
  assert.deepEqual(stacks.map(s=>s.dataset.provider),['claude','codex']);
  assert.deepEqual(stacks.map(s=>s.children[0].children[0].textContent),['Claude','Codex']);
  assert.deepEqual(stacks.map(s=>s.children[1].children.length),[1,2]);
  assert(cards.every(card=>card.tag==='article'));assert.deepEqual(cards.map(s=>s.dataset.provider),['claude','codex','codex']);
  assert.equal(stacks[1].children[0].children[1].getAttribute('aria-label'),'显示 2 / 2 个匹配账户');
  const resets=descendants(elements.get('quota-cards')).filter(n=>n.className==='manual-reset');
  assert.deepEqual(resets.map(n=>n.children.map(c=>c.textContent)),[['可重置','2','次'],['可重置','0','次']]);
  api.renderSample({quotas},'claude');assert.equal(elements.get('quota-cards').children.length,1);
  assert.equal(elements.get('quota-cards').children[0].dataset.provider,'claude');
});
for(const language of ['zh','en'])test(`${language}: Antigravity renders one 5h/7d pair per quota group with translated labels`,()=>{
  const {api,elements}=dashboard({},undefined,{language});
  const reset=new Date(Date.now()+3600000).toISOString();
  const quotas=[{provider:'antigravity',auth_id:'ag',name:'g@example.com',groups:[
    {key:'gemini',label:'Gemini Models',five_hour:{used_percent:30,reset_at:reset,observed_tokens:1100000,observed_cost:1.125,estimated_total_tokens:4400000,estimated_total_cost:4.5,estimated_basis:'quota'},seven_day:{used_percent:55,reset_at:reset}},
    {key:'claude-gpt',label:'Claude and GPT Models',seven_day:{used_percent:100,reset_at:reset}},
    {key:'other-imagen',label:'Imagen Models'}]},{provider:'codex',auth_id:'c',name:'c@example.com',five_hour:{used_percent:10}}];
  api.renderSample({quotas});
  const sections=elements.get('quota-cards').children;assert.deepEqual(sections.map(s=>s.dataset.provider),['antigravity','codex']);
  const card=descendants(sections[0]);
  assert.deepEqual(card.filter(n=>n.className==='window-group').map(n=>n.children[0].textContent),language==='en'?['Gemini models','Claude / GPT models','Imagen Models']:['Gemini 模型','Claude / GPT 模型','Imagen Models']);
  assert.equal(card.filter(n=>n.className==='windows').length,3);
  assert.equal(sections[0].children[0].children[0].textContent,'Antigravity'); // Identity is shared by the provider stack, not repeated in every card.
  assert.deepEqual(card.filter(n=>n.className==='percent').map(n=>n.children.length?n._text:n.textContent),['70.0','45.0','—','0.0','—','—']);
  assert(card.some(n=>n.textContent==='$4.50')&&card.some(n=>n.textContent.startsWith('4.4m ')&&n.title.startsWith('4,400,000 ')));
  api.renderSample({quotas},'antigravity');assert.deepEqual(elements.get('quota-cards').children.map(s=>s.dataset.provider),['antigravity']);
  assert(descendants(elements.get('provider')).some(n=>n.attributes.value==='antigravity'));
});
function encoded(value,version=1){
  const mask=Buffer.from('cli-proxy-api-webui::secure-storage|cpa.test'+(version===1?'|test-agent':''));
  return `enc::v${version}::`+Buffer.from(Buffer.from(JSON.stringify(value)).map((b,i)=>b^mask[i%mask.length])).toString('base64');
}
for(const version of [1,2]) test(`reuse CPA v${version} encoded login without inputs, persistence or query credentials`, async () => {
  const requests=[];
  const stored={'cli-proxy-auth':encoded({state:{apiBase:'https://cpa.test',managementKey:'test-only-key'}},version)};
  const {api,writes}=dashboard(stored,async(url,options)=>{requests.push({url,options});return {ok:true,json:async()=>({credentials:[],quotas:[]})};});
  assert.equal(api.guessKey(),'test-only-key');await api.load();
  assert.equal(requests[0].url,'/v0/management/plugins/cpa-mini-usage/data');
  assert.equal(requests[0].options.headers.Authorization,'Bearer test-only-key');
  assert.equal(requests[0].options.redirect,'error');
  const outsideWakeup=html.replace(/<section id="page-wakeup"[\s\S]*?<footer class="foot"/, '<footer class="foot"');
  assert.equal((outsideWakeup.match(/<input\b/g)||[]).length,4);assert.match(html,/<input id="account-search" type="search"/);assert.equal((html.match(/type="date"/g)||[]).length,2);assert(!/type="password"/.test(html));assert(!/type="(?:password|text)"/.test(outsideWakeup));assert.match(html,/<input id="persistence-enabled" type="checkbox"/);assert.deepEqual(writes,[]);api.setLanguage('en');assert.deepEqual(writes,[['cpa-mini-usage-language','en']]);assert(!/sessionStorage\.setItem/.test(source));
});
test('cross-origin or corrupt CPA credentials are not reused', () => {
  for(const version of [1,2]){
    assert.equal(dashboard({'cli-proxy-auth':encoded({state:{apiBase:'https://other.test',managementKey:'wrong-key'}},version)}).api.guessKey(),'');
    assert.equal(dashboard({'cli-proxy-auth':`enc::v${version}::invalid`}).api.guessKey(),'');
  }
  assert.equal(dashboard({'cli-proxy-auth':'enc::v3::unknown'}).api.guessKey(),'');
  assert.equal(dashboard({managementKey:JSON.stringify('legacy-key')}).api.guessKey(),'legacy-key');
});
test('manual refresh polls until provider fetch finishes and keeps model visible', async () => {
  let reads=0;
  const {api,elements}=dashboard({'cli-proxy-auth':encoded({state:{managementKey:'test-key'}})},async(url)=>({ok:true,json:async()=>url.endsWith('refresh')?{status:'refreshing'}:{quota_refreshing:++reads<3,quota_refresh_finished_at:reads<3?'old':'new',recent:[{model:'claude-test',total:1200}],credentials:[],quotas:[]}}));
  await api.load(true);assert.equal(reads,3);
  assert(descendants(elements.get('recent-table')).some(n=>n.textContent==='claude-test'));
  assert.equal(elements.get('refresh').disabled,false);
});
test('logout clears old private data', async () => {
  const {api,elements}=dashboard();api.renderSample({quotas:[{provider:'claude',name:'private@example.com'}]});
  await api.load();assert(!descendants(elements.get('quota-cards')).some(n=>n.textContent==='private@example.com'));
});

test('late finish snapshot before new refresh is not mistaken for completion', async () => {
  let reads=0;
  const {api}=dashboard({'cli-proxy-auth':encoded({state:{managementKey:'test-key'}})},async(url)=>({ok:true,json:async()=>url.endsWith('refresh')?{quota_refresh_finished_at:'baseline'}:{quota_refreshing:false,quota_refresh_finished_at:++reads===1?'baseline':'completed'}}));
  await api.load(true);assert.equal(reads,2);
});
test('recent request unknown model and provider filtering are explicit', () => {
  const {api,elements}=dashboard();
  api.renderSample({recent:[{provider:'claude',auth_id:'a',model:'claude-model'},{provider:'codex',auth_id:'b',model:''}]},'codex');
  const nodes=descendants(elements.get('recent-table'));
  assert(nodes.some(n=>n.textContent==='未知模型'));assert(!nodes.some(n=>n.textContent==='claude-model'));
});

test('official percentage precedes USD and readable compact tokens', () => {
  const {api,elements}=dashboard();
  api.renderSample({quotas:[{provider:'codex',five_hour:{used_percent:25,observed_cost:30,observed_tokens:1000000,estimated_total_cost:120,estimated_total_tokens:4000000},seven_day:{used_percent:100,observed_cost:120,observed_tokens:4000000,estimated_total_cost:120,estimated_total_tokens:4000000}}]});
  const nodes=descendants(elements.get('quota-cards'));
  const stats=nodes.filter(n=>n.className==='windowstats');
  assert.deepEqual(stats[0].children[0].children.map(n=>n.textContent),['已用（观测）','预估总额']);
  assert.deepEqual(stats[0].children[1].children.map(n=>n.className),['observed-cost','estimate-cost']);
  assert.deepEqual(stats[0].children[1].children.map(n=>n.textContent),['$30.00','$120.00']);
  assert.deepEqual(stats[0].children[2].children.map(n=>n.textContent),['1m token','4m token']);
  assert.deepEqual(stats[1].children[1].children.map(n=>n.textContent),['$120.00','$120.00']);
  const window=nodes.find(n=>n.className==='window');
  assert.equal(window.children[1].className,'quota-value');
  assert.equal(window.children[3].className,'windowstats');
});
test('missing model price does not display a false USD amount', () => {
  const {api,elements}=dashboard();
  api.renderSample({quotas:[{provider:'codex',five_hour:{used_percent:25,estimated_total_tokens:4000000,missing_price:true}}]});
  const nodes=descendants(elements.get('quota-cards'));
  assert.equal(nodes.find(n=>n.className==='estimate-cost').textContent,'—');
  assert(nodes.some(n=>n.textContent==='4m token'));
});

test('reset times show days/hours/minutes and never a negative countdown', () => {
  const {api}=dashboard();const now=Date.parse('2026-10-09T00:00:00Z');
  for(const [offset,want] of [[(2*24+3)*3600000,'还有 2 天 3 小时重置'],[2*3600000+15*60000,'还有 2 小时 15 分钟重置'],[20*60000,'还有 20 分钟重置'],[1,'还有 1 分钟重置'],[0,'等待额度更新'],[-1000,'等待额度更新']])assert.equal(api.resetCountdown(new Date(now+offset).toISOString(),now),want);
  assert.equal(api.resetCountdown(null,now),'重置时间待更新');
  assert.equal(api.resetCountdown('invalid',now),'重置时间待更新');
});
test('reset countdown retains full date in tooltip', () => {
  const {api,elements}=dashboard();const at=new Date(Date.now()+7200000).toISOString();
  api.renderSample({quotas:[{provider:'codex',five_hour:{used_percent:25,reset_at:at}}]});
  const reset=descendants(elements.get('quota-cards')).find(n=>n.className==='reset');
  assert.match(reset.textContent,/还有 2 小时/);assert.notEqual(reset.title,'待更新');
});

function sampleSnapshot(){return {generated_at:'2026-10-09T00:00:00Z',quotas:[{auth_id:'account',provider:'codex',name:'alice@example.com',fetched_at:'old',five_hour:{used_percent:25,reset_at:new Date(Date.now()+7200000).toISOString(),estimated_total_cost:120,estimated_total_tokens:4000000}}],credentials:[],recent:[]};}
test('unchanged and timestamp-only snapshots preserve all content nodes',()=>{
  const {api,elements}=dashboard();const sample=sampleSnapshot();api.renderSample(sample);
  const quota=panel(elements,'account'),requests=elements.get('recent-table').children[0];
  api.acceptSnapshot(JSON.parse(JSON.stringify(sample)));
  api.acceptSnapshot({...sample,generated_at:'new',quota_updated_at:'2026-10-09T01:00:00Z',quotas:sample.quotas.map(q=>({...q,fetched_at:'new'}))});
  assert.equal(panel(elements,'account'),quota);assert.equal(elements.get('recent-table').children[0],requests);
});
test('request-only changes preserve quota nodes; quota-only changes preserve requests',()=>{
  const {api,elements}=dashboard();let sample=sampleSnapshot();api.renderSample(sample);
  const quota=panel(elements,'account');
  sample={...sample,recent:[{auth_id:'account',model:'gpt-test',total:1000}]};api.acceptSnapshot(sample);
  assert.equal(panel(elements,'account'),quota);
  const requests=elements.get('recent-table').children[0];
  api.acceptSnapshot({...sample,quotas:sample.quotas.map(q=>({...q,manual_reset_remaining:2}))});
  assert.equal(elements.get('recent-table').children[0],requests);
  assert.notEqual(panel(elements,'account'),quota); // Only the changed account card is replaced.
});
test('countdowns update in place without rebuilding cards',()=>{
  const {api,elements}=dashboard();api.renderSample(sampleSnapshot());
  const quota=elements.get('quota-cards').children[0],reset=descendants(quota).find(n=>n.className==='reset');
  reset.dataset.resetAt=new Date(Date.now()+20*60000).toISOString();api.updateCountdowns();
  assert.equal(elements.get('quota-cards').children[0],quota);assert.match(reset.textContent,/还有 20 分钟/);
});
test('quiet automatic refresh keeps visible controls and content stable',async()=>{
  let release, notify, delayed=false;const sample=sampleSnapshot(),started=new Promise(resolve=>{notify=resolve;});
  const {api,elements}=dashboard({managementKey:JSON.stringify('key')},async()=>({ok:true,json:async()=>{if(delayed)return new Promise(resolve=>{release=()=>resolve(sample);notify();});return sample;}}));
  await api.load();const quota=elements.get('quota-cards').children[0];delayed=true;
  const pending=api.load();await started;
  assert.equal(elements.get('refresh').disabled,false);assert.equal(elements.get('refresh-label').textContent,'刷新全部');
  release();await pending;assert.equal(elements.get('quota-cards').children[0],quota);
});
test('temporary automatic-refresh errors retain data; authentication errors clear it',async()=>{
  let status=200;const sample=sampleSnapshot();
  const {api,elements}=dashboard({managementKey:JSON.stringify('key')},async()=>({ok:status===200,status,json:async()=>sample}));
  await api.load();const quota=elements.get('quota-cards').children[0];
  status=503;await api.load();assert.equal(elements.get('quota-cards').children[0],quota);
  status=401;await api.load();assert(!descendants(elements.get('quota-cards')).some(n=>n.textContent==='alice@example.com'));
});
test('refresh cadence is low and entry animations are absent',()=>{
  assert.match(source,/setInterval\([\s\S]*?,60000\)/);assert.match(source,/setInterval\([\s\S]*?,30000\)/);
  const css=html.match(/<style>([\s\S]*?)<\/style>/)[1];assert(!/animation:|@keyframes|transition:/.test(css));
  assert.match(css,/\.manual-reset strong\{font-size:18px/);
});
test('logged-out view has a single visible login section',async()=>{
  const {api,elements}=dashboard();await api.load();
  assert(elements.get('dashboard').classes.has('needs-login'));
  assert.match(html,/\.needs-login \.tabs,\.needs-login \[role=tabpanel\],\.needs-login \.foot,\.needs-login \.filter-control/);assert(!/\.needs-login \.toolbar\s*[,\{]/.test(html));
});

const reply=(payload,status=200)=>({ok:status>=200&&status<300,status,json:async()=>payload});
const login=()=>({managementKey:JSON.stringify('key')});
function twoAccounts(){const sample=sampleSnapshot();return {...sample,quotas:[...sample.quotas,{auth_id:'other',provider:'codex',name:'bob@example.com',manual_reset_remaining:2,five_hour:{used_percent:20}}]};}
const panel=(elements,id)=>descendants(elements.get('quota-cards')).find(n=>n.tag==='article'&&n.dataset.authId===id);
const refreshButton=card=>descendants(card).find(n=>n.className==='button account-refresh');
const accountError=card=>descendants(card).find(n=>n.className==='account-error');

test('language follows explicit plugin choice, CPA setting, then browser; manual changes persist',()=>{
  for(const [browser,stored,want] of [
    ['en-GB',{},'en'],['zh-TW',{},'zh'],['fr-FR',{},'en'],
    ['en-US',{'cpa-mini-usage-language':'zh'},'zh'],['zh-CN',{'cpa-mini-usage-language':'en'},'en'],
    ['zh-CN',{'cli-proxy-language':'{"state":{"language":"en"},"version":0}'},'en'],
    ['en-US',{'cli-proxy-language':'{"state":{"language":"zh-TW"},"version":0}'},'zh'],
    ['zh-CN',{'cli-proxy-language':'{"language":"en"}'},'en'],['zh-CN',{'cli-proxy-language':'"en"'},'en'],
    ['en-US',{'cli-proxy-language':'zh-CN'},'zh'],['zh-CN',{'cli-proxy-language':'ru'},'en'],
    ['en-US',{'cli-proxy-language':'bad-json{'},'en'],
    ['zh-CN',{'cpa-mini-usage-language':'zh','cli-proxy-language':'en'},'zh'],
    ['en-US',{'cpa-mini-usage-language':'invalid','cli-proxy-language':'zh-CN'},'zh']
  ]){
    const d=dashboard(stored,undefined,{language:browser});assert.equal(d.elements.get('language').value,want);assert.equal(d.document.documentElement.lang,want==='en'?'en':'zh-CN');assert.deepEqual(d.writes,[]);
    d.api.setLanguage(want==='en'?'zh':'en');assert.deepEqual(d.writes,[['cpa-mini-usage-language',want==='en'?'zh':'en']]);
  }
  assert.equal(dashboard({},undefined,{languages:['en-AU','zh-CN']}).elements.get('language').value,'en');
});
test('CPA language changes update the plugin until the user selects an override',()=>{
  const stored={'cli-proxy-language':'en'},d=dashboard(stored,undefined,{language:'zh-CN'});
  assert.equal(d.elements.get('language').value,'en');
  stored['cli-proxy-language']='{"state":{"language":"zh-CN"}}';d.events.storage({key:'cli-proxy-language'});
  assert.equal(d.elements.get('language').value,'zh');assert.deepEqual(d.writes,[]);
  d.api.setLanguage('en');stored['cli-proxy-language']='zh-CN';d.events.storage({key:'cli-proxy-language'});
  assert.equal(d.elements.get('language').value,'en');assert.deepEqual(d.writes,[['cpa-mini-usage-language','en']]);
});

test('persistent dashboard alerts expire after one minute without reappearing on poll',async()=>{
  const failures={price_error:'Price catalog fetch failed',quota_error:'All OAuth quota reads failed'};
  let issues=failures;
  const d=dashboard(login(),async()=>reply({...twoAccounts(),...issues}),{fakeClock:true});
  const alert=d.elements.get('alert');
  await d.api.load();assert.equal(alert.textContent,'价格读取失败。 · 额度接口不可用。');
  d.advanceTime(30000);await d.api.load();
  d.advanceTime(29999);assert.equal(alert.classes.has('hide'),false);
  d.advanceTime(1);assert.equal(alert.classes.has('hide'),true);
  await d.api.load();assert.equal(alert.textContent,'');
  issues={};await d.api.load();
  issues=failures;await d.api.load();assert.equal(alert.classes.has('hide'),false);
});

for(const language of ['zh','en']){
  test(`${language}: complete static/dynamic translations, alerts, labels, titles and countdowns`,()=>{
    const d=dashboard({},undefined,{language}),{api,elements,document}=d;const sample=twoAccounts();
    sample.credentials=[{auth_id:'account',name:'alice@example.com',provider:'codex',requests:1,failures:1,total:1250,models:[{model:'gpt-test',tokens:1250,requests:1}]}];sample.recent=[{auth_id:'account',model:'gpt-test',failed:true,total:1250}];
    api.renderSample(sample);api.setStatus('部分数据异常');api.setAlert('账户额度刷新超时，请稍后重试。');
    assert.equal(elements.get('refresh-label').textContent,language==='en'?'Refresh all':'刷新全部');
    const reset=descendants(panel(elements,'other')).find(n=>n.className==='manual-reset');assert.deepEqual(reset.children.map(n=>n.textContent),language==='en'?['Resets remaining','2']:['可重置','2','次']);
    assert.equal(refreshButton(panel(elements,'account')).getAttribute('aria-label'),language==='en'?'Refresh quota for alice@example.com':'刷新 alice@example.com 的额度');
    assert.equal(elements.get('provider').getAttribute('aria-label'),language==='en'?'Filter by provider':'筛选服务商');
    assert.equal(elements.get('account-search').placeholder,language==='en'?'Search accounts (name, email or ID)':'搜索账户（名称、邮箱或 ID）');
    const refresh=refreshButton(panel(elements,'account'));assert.equal(refresh.title,refresh.getAttribute('aria-label'));assert.equal(refresh.children[0].getAttribute('aria-hidden'),'true');assert.equal(refresh.children[1].textContent,language==='en'?'Refresh':'刷新');
    assert.match(elements.get('footer').textContent,/15/);assert.equal(document.title,language==='en'?'Usage & quota · CPA Mini Usage':'用量与额度 · CPA Mini Usage');
    assert.equal(api.resetCountdown(new Date(Date.now()+20*60000).toISOString()),language==='en'?'Resets in 20m':'还有 20 分钟重置');
    const nodes=[...elements.values()].flatMap(descendants);assert(nodes.some(n=>n.textContent===(language==='en'?'Total tokens':'总 token')));assert(nodes.some(n=>n.textContent==='gpt-test'));
    if(language==='en'){for(const node of nodes){assert(!/[\u3400-\u9fff]/.test(node.textContent),node.textContent);assert(!/[\u3400-\u9fff]/.test(node.title||''),node.title);for(const [attr,value] of Object.entries(node.attributes))if(['aria-label','aria-valuetext'].includes(attr))assert(!/[\u3400-\u9fff]/.test(value),value);}}
    api.setLanguage(language==='en'?'zh':'en');assert.equal(elements.get('alert').textContent,language==='en'?'账户额度刷新超时，请稍后重试。':'Account quota refresh timed out. Please retry later.');assert.equal(elements.get('status').textContent,language==='en'?'部分数据异常':'Some data unavailable');
  });

  test(`${language}: individual refresh posts JSON, only clicked button busy, accepted baseline gates completion and sibling identity survives`,async()=>{
    const sample=twoAccounts(),requests=[];let release,reads=0;
    const d=dashboard(login(),async(url,options)=>{
      requests.push({url,options});if(url.endsWith('refresh-account'))return new Promise(resolve=>{release=()=>resolve(reply({status:'refreshing',auth_id:'account',refresh_finished_at:'baseline'},202));});
      if(!release)return reply(sample);
      reads++;return reply({...sample,quotas:reads===3?sample.quotas.map(q=>q.auth_id==='account'?{...q,five_hour:{used_percent:10}}:q):sample.quotas,account_refresh:{account:{refreshing:reads===2,finished_at:reads<3?'baseline':'done'}}});
    },{language});
    await d.api.load();const other=panel(d.elements,'other'),first=panel(d.elements,'account'),table=d.elements.get('recent-table').children[0],status=d.elements.get('status').textContent;
    const pending=refreshButton(first).onclick();assert.equal(refreshButton(first).disabled,true);assert.equal(refreshButton(other).disabled,false);assert.equal(d.elements.get('refresh').disabled,true);assert.equal(d.elements.get('status').textContent,status);assert.equal(d.elements.get('page-quota').getAttribute('aria-busy'),'false');
    assert.equal(refreshButton(first).children[0].textContent,'…');assert.equal(refreshButton(first).children[1].textContent,language==='en'?'Refreshing…':'正在刷新…');assert.match(refreshButton(first).getAttribute('aria-label'),language==='en'?/Refreshing…/:/正在刷新…/);
    assert.equal(requests.at(-1).options.method,'POST');assert.equal(requests.at(-1).options.headers['Content-Type'],'application/json');assert.deepEqual(JSON.parse(requests.at(-1).options.body),{auth_id:'account'});
    release();await pending;assert.equal(reads,3);assert.equal(panel(d.elements,'other'),other);assert.notEqual(panel(d.elements,'account'),first);assert.equal(d.elements.get('recent-table').children[0],table);assert.equal(refreshButton(panel(d.elements,'account')).disabled,false);assert.equal(d.elements.get('refresh').disabled,false);
  });

  for(const [status,error,zh,en] of [[400,'invalid_auth_id','无效的账户标识。','Invalid account ID.'],[404,'account_not_found','未找到账户。','Account not found.'],[409,'refresh_busy','已有额度刷新正在进行，请稍后重试。','A quota refresh is already running. Please retry later.']]){
    test(`${language}: account HTTP ${status} is translated and retained after language switching`,async()=>{
      const d=dashboard(login(),async url=>url.endsWith('refresh-account')?reply({error},status):reply(twoAccounts()),{language});await d.api.load();const other=panel(d.elements,'other');await d.api.refreshAccount('account');assert.equal(accountError(panel(d.elements,'account')).textContent,language==='zh'?zh:en);assert.equal(panel(d.elements,'other'),other);d.api.setLanguage(language==='en'?'zh':'en');assert.equal(accountError(panel(d.elements,'account')).textContent,language==='en'?zh:en);
    });
  }
  test(`${language}: account completion failure and timeout preserve quota data and translate`,async()=>{
    for(const timeout of [false,true]){
      let accepted=false;const sample=twoAccounts();const d=dashboard(login(),async url=>{if(url.endsWith('refresh-account')){accepted=true;return reply({refresh_finished_at:'baseline'},202);}return reply(accepted?{...sample,account_refresh:{account:{refreshing:timeout,finished_at:timeout?'baseline':'done',error:timeout?'':'quota HTTP 429'}}}:sample);},{language,fakeClock:true});
      await d.api.load();const other=panel(d.elements,'other');await d.api.refreshAccount('account');assert.equal(panel(d.elements,'other'),other);assert.equal(accountError(panel(d.elements,'account')).textContent,timeout?(language==='en'?'Account quota refresh timed out. Please retry later.':'账户额度刷新超时，请稍后重试。'):(language==='en'?'Quota endpoint returned HTTP 429.':'额度接口返回 HTTP 429。'));
    }
  });
  test(`${language}: account network/auth failures do not leak backend OAuth text`,async()=>{
    for(const status of [503,401,403]){
      const d=dashboard(login(),async url=>url.endsWith('refresh-account')?reply({error:'access_token=secret refresh_token=private'},status):reply(twoAccounts()),{language});await d.api.load();await d.api.refreshAccount('account');
      if(status===503){assert(panel(d.elements,'account'));assert(!/secret|private/.test(accountError(panel(d.elements,'account')).textContent));}else{assert(!panel(d.elements,'account'));assert(d.elements.get('dashboard').classes.has('needs-login'));assert.match(d.elements.get('alert').textContent,language==='en'?/expired/:/失效/);}
    }
  });
}

test('backend refresh from another window blocks only the same account, not a different local account',async()=>{
  let posts=0;const sample=twoAccounts(),d=dashboard(login(),async url=>{if(url.endsWith('refresh-account')){posts++;return reply({refresh_finished_at:'old'},202);}return reply({...sample,account_refresh:{account:{refreshing:true},other:{refreshing:false,finished_at:'done'}}});});await d.api.load();const first=panel(d.elements,'account'),other=panel(d.elements,'other'),table=d.elements.get('recent-table').children[0];
  d.api.acceptSnapshot({...sample,generated_at:'new',account_refresh:{account:{refreshing:true}}});assert.equal(panel(d.elements,'account'),first);assert.equal(panel(d.elements,'other'),other);assert.equal(refreshButton(first).disabled,true);assert.equal(refreshButton(other).disabled,false);await d.api.refreshAccount('other');assert.equal(posts,1);assert.equal(refreshButton(first).disabled,true);assert.equal(refreshButton(other).disabled,false);assert.equal(accountError(other).textContent,'');assert.equal(d.elements.get('recent-table').children[0],table);
  d.api.acceptSnapshot({...sample,account_refresh:{account:{refreshing:false,finished_at:'done',error:'quota response invalid'}}});assert.equal(refreshButton(first).disabled,false);assert.equal(accountError(first).textContent,'额度响应格式异常。');d.api.acceptSnapshot({...sample,account_refresh:{account:{refreshing:true}}});await d.api.refreshAccount('account');assert.equal(posts,1);assert.match(accountError(first).textContent,/正在进行/);
});

for(const step of ['accepted','snapshot'])for(const change of ['logout','key-change']){
  test(`account ${step} response after ${change} is discarded and privacy cleared`,async()=>{
    const stored=login(),sample=twoAccounts();let post=false,release,notify;const started=new Promise(resolve=>notify=resolve);
    const d=dashboard(stored,async(url,options)=>{
      if(options.headers.Authorization==='Bearer new-key')return reply({quotas:[],credentials:[],recent:[]});
      if(url.endsWith('refresh-account')){post=true;if(step==='accepted')return new Promise(resolve=>{release=()=>resolve(reply({refresh_finished_at:'baseline'},202));notify();});return reply({refresh_finished_at:'baseline'},202);}
      if(post)return new Promise(resolve=>{release=()=>resolve(reply({...sample,account_refresh:{account:{refreshing:false,finished_at:'done'}}}));notify();});return reply(sample);
    });await d.api.load();const pending=d.api.refreshAccount('account');await started;
    if(change==='logout')delete stored.managementKey;else stored.managementKey=JSON.stringify('new-key');release();await pending;assert(!panel(d.elements,'account'));assert(!descendants(d.elements.get('quota-cards')).some(n=>n.textContent==='alice@example.com'));assert.equal(d.elements.get('refresh').disabled,false);
  });
}

test('manual global refresh keeps bodyless POST contract and temporary failure retains cards',async()=>{
  const sample=twoAccounts(),requests=[];let failing=false;
  const d=dashboard(login(),async(url,options)=>{requests.push({url,options});if(url.endsWith('refresh'))return failing?reply({},503):reply({quota_refresh_finished_at:'old'},202);return reply({...sample,quota_refresh_finished_at:'done'});});
  await d.api.load();await d.api.load(true);const post=requests.find(r=>r.url.endsWith('refresh'));assert.equal(post.options.method,'POST');assert.equal(post.options.body,undefined);assert.equal(post.options.headers['Content-Type'],undefined);
  const other=panel(d.elements,'other');failing=true;await d.api.load(true);assert.equal(panel(d.elements,'other'),other);
});

async function waitForTest(predicate){for(let i=0;i<100;i++){if(predicate())return;await new Promise(resolve=>setImmediate(resolve));}assert.fail('async test condition did not settle');}
for(const language of ['zh','en'])test(`${language}: two accounts refresh concurrently with independent completion/errors and stale polls ignored`,async()=>{
  const sample=twoAccounts(),posts=new Map(),polls=[],calls=[];
  const d=dashboard(login(),async(url,options)=>{calls.push({url,options});if(url.endsWith('refresh-account')){const id=JSON.parse(options.body).auth_id;return new Promise(resolve=>posts.set(id,resolve));}if(posts.size)return new Promise(resolve=>polls.push(resolve));return reply(sample);},{language});
  await d.api.load();const first=panel(d.elements,'account'),other=panel(d.elements,'other'),table=d.elements.get('recent-table').children[0];
  const a=d.api.refreshAccount('account'),b=d.api.refreshAccount('other');assert.equal(posts.size,2);assert.equal(refreshButton(first).disabled,true);assert.equal(refreshButton(other).disabled,true);await d.api.refreshAccount('account');assert.equal(calls.filter(c=>c.options.method==='POST').length,2);
  posts.get('account')(reply({refresh_finished_at:'old-a'},202));await waitForTest(()=>polls.length===1);
  posts.get('other')(reply({refresh_finished_at:'old-b'},202));await waitForTest(()=>polls.length===2);
  polls[1](reply({...sample,account_refresh:{account:{refreshing:true,finished_at:'old-a'},other:{refreshing:false,finished_at:'done-b'}}}));await b;
  assert.equal(refreshButton(first).disabled,true);assert.equal(refreshButton(other).disabled,false);assert.equal(accountError(other).textContent,'');assert.equal(d.elements.get('refresh').disabled,true);
  polls[0](reply({...sample,account_refresh:{account:{refreshing:true,finished_at:'old-a'},other:{refreshing:true,finished_at:'old-b'}}}));await waitForTest(()=>polls.length===3);assert.equal(refreshButton(other).disabled,false);
  polls[2](reply({...sample,account_refresh:{account:{refreshing:false,finished_at:'done-a',error:'quota HTTP 429'},other:{refreshing:false,finished_at:'done-b'}}}));await a;
  assert.equal(refreshButton(first).disabled,false);assert.equal(refreshButton(other).disabled,false);assert.equal(d.elements.get('refresh').disabled,false);assert.match(accountError(first).textContent,language==='en'?/HTTP 429/:/HTTP 429/);assert.equal(accountError(other).textContent,'');assert.equal(panel(d.elements,'account'),first);assert.equal(panel(d.elements,'other'),other);assert.equal(d.elements.get('recent-table').children[0],table);assert(calls.filter(c=>c.options.method==='POST').every(c=>c.url.endsWith('refresh-account')));
});
for(const change of ['logout','key-change'])test(`two concurrent account jobs discard all old responses after ${change}`,async()=>{
  const stored=login(),pendingPosts=new Map(),sample=twoAccounts();
  const d=dashboard(stored,async(url,options)=>{if(url.endsWith('refresh-account'))return new Promise(resolve=>pendingPosts.set(JSON.parse(options.body).auth_id,resolve));return reply(options.headers.Authorization==='Bearer new-key'?{quotas:[],credentials:[],recent:[]}:sample);});
  await d.api.load();const a=d.api.refreshAccount('account'),b=d.api.refreshAccount('other');assert.equal(pendingPosts.size,2);
  if(change==='logout')delete stored.managementKey;else stored.managementKey=JSON.stringify('new-key');pendingPosts.get('account')(reply({refresh_finished_at:'old'},202));await a;pendingPosts.get('other')(reply({refresh_finished_at:'old'},202));await b;assert(!panel(d.elements,'account'));assert(!panel(d.elements,'other'));assert.equal(d.elements.get('refresh').disabled,false);
});

test('poll errors retain private snapshot unless authentication expires',async()=>{
  for(const status of [503,401]){
    let accepted=false;const d=dashboard(login(),async url=>{if(url.endsWith('refresh-account')){accepted=true;return reply({refresh_finished_at:'old'},202);}return accepted?reply({},status):reply(twoAccounts());},{language:'en'});
    await d.api.load();const other=panel(d.elements,'other');await d.api.refreshAccount('account');if(status===503){assert.equal(panel(d.elements,'other'),other);assert.match(accountError(panel(d.elements,'account')).textContent,/HTTP 503/);}else assert(!panel(d.elements,'account'));
  }
});

test('language switching translates empty states, unknown model, missing prices, and token tooltips',()=>{
  const d=dashboard(),sample=twoAccounts();sample.quotas[0].five_hour.missing_price=true;sample.credentials=[{auth_id:'account',total:1000,missing_price:true}];sample.recent=[{auth_id:'account'}];d.api.renderSample(sample);d.api.setLanguage('en');
  const nodes=[...d.elements.values()].flatMap(descendants);assert(nodes.some(n=>n.textContent==='Unknown model'));assert(nodes.some(n=>n.title==='Model price missing; amount unavailable'));assert(nodes.some(n=>n.title==='1,000 tokens'));assert(nodes.some(n=>n.textContent==='Price missing'));
  assert(!nodes.some(n=>/[\u3400-\u9fff]/.test(n.textContent)||/[\u3400-\u9fff]/.test(n.title||'')));
  d.api.renderSample({quotas:[],credentials:[],recent:[]});assert(descendants(d.elements.get('quota-cards')).some(n=>n.textContent==='No OAuth quota available yet'));d.api.setLanguage('zh');assert(descendants(d.elements.get('quota-cards')).some(n=>n.textContent==='还没有可用的 OAuth 额度'));
});

test('names resembling translations or placeholders remain literal; no OAuth fields are rendered',()=>{
  const d=dashboard({},undefined,{language:'en'});d.api.renderSample({quotas:[{auth_id:'account',provider:'codex',name:'alice+{name}@example.com',access_token:'secret',refresh_token:'private',five_hour:{used_percent:0}}],credentials:[{auth_id:'account',name:'剩余额度',models:[{model:'输入'}]}],recent:[{auth_id:'account',model:'输出'}]});
  const nodes=[...d.elements.values()].flatMap(descendants);for(const name of ['alice+{name}@example.com','剩余额度','输入','输出'])assert(nodes.some(n=>n.textContent===name));assert(!nodes.some(n=>/secret|private/.test(n.textContent)));assert.equal(refreshButton(panel(d.elements,'account')).getAttribute('aria-label'),'Refresh quota for alice+{name}@example.com');
});

test('cadence keeps minute snapshot and 30-second countdown timers without browser quota POST scheduling',()=>{
  const d=dashboard();assert.deepEqual(d.intervals.map(i=>i.ms),[60000,30000]);assert.match(d.elements.get('footer').textContent,/每 15 分钟/);
  const css=html.match(/<style>([\s\S]*?)<\/style>/)[1];assert.match(css,/\.percent\{font-size:22px/);assert.match(css,/\.estimate-cost\{[^}]*font-size:15px/);assert.match(css,/\.estimate-value\{font-size:12px/);assert.match(css,/\.manual-reset strong\{font-size:18px/);assert.match(css,/\.masthead\{[^}]*flex-wrap:wrap/);
});

test('successful later refresh clears a prior local account error without replacing siblings',async()=>{
  const sample=twoAccounts(),d=dashboard(login(),async url=>url.endsWith('refresh-account')?reply({error:'refresh_busy'},409):reply(sample));
  await d.api.load();const other=panel(d.elements,'other');await d.api.refreshAccount('account');assert.match(accountError(panel(d.elements,'account')).textContent,/正在进行/);
  d.api.acceptSnapshot({...sample,quotas:sample.quotas.map(q=>q.auth_id==='account'?{...q,fetched_at:'successful-later'}:q),account_refresh:{account:{refreshing:false,finished_at:'completed'}}});
  assert.equal(accountError(panel(d.elements,'account')).textContent,'');assert.equal(panel(d.elements,'other'),other);
});

test('request-average TPS follows Manager Plus total latency, never TTFT or input tokens',()=>{
  const {api}=dashboard(),event={output:100,latency_ms:2500,ttft_ms:500};assert.equal(api.requestTPS(event),40);assert.equal(api.requestTPS({output:20,latency_ms:1500}),20/1.5);
  for(const value of [null,undefined,NaN,Infinity,-1,'500','',true]){
    assert.equal(api.requestTPS({...event,ttft_ms:value}),40);assert.equal(api.requestTPS({...event,latency_ms:value}),null);
    assert.equal(api.firstTokenTime(value),'—');
  }
  for(const patch of [{latency_ms:0},{output:0},{output:-1},{output:NaN},{output:Infinity},{output:'100'},{output:null},{failed:true},{latency_ms:undefined}])assert.equal(api.requestTPS({...event,...patch}),null,JSON.stringify(patch));
  for(const ttft_ms of [0,500,2500,5000])assert.equal(api.requestTPS({...event,ttft_ms}),40);assert.equal(api.requestTPS({...event,stream:false}),40);assert.equal(api.requestTPS({...event,output:1}),.4);assert.equal(api.requestTPS({...event,latency_ms:500}),200);assert.equal(api.firstTokenTime(0),'0.00 s');assert.equal(api.firstTokenTime(450),'0.45 s');assert.equal(api.firstTokenTime(1250),'1.25 s');
});

for(const language of ['zh','en']){
  test(`${language}: timing column shows independent first-token and total seconds with request-average TPS`,()=>{
    const d=dashboard({},undefined,{language});d.api.renderSample({recent:[{output:100,ttft_ms:500,latency_ms:2500,model:'gpt-test'},{output:100,ttft_ms:1250,latency_ms:3250},{output:100},{output:100,ttft_ms:500,latency_ms:2500,failed:true}]});
    const nodes=descendants(d.elements.get('recent-table')),timing=nodes.filter(n=>n.className==='num request-timing'),tps=nodes.filter(n=>n.className==='num tps');
    assert(nodes.some(n=>n.tag==='th'&&n.textContent===(language==='en'?'Time':'时间')));assert(nodes.some(n=>n.tag==='th'&&n.textContent===(language==='en'?'Requested at':'请求时间')));assert(!nodes.some(n=>n.tag==='th'&&n.textContent===(language==='en'?'First token':'首字时间')));assert(nodes.some(n=>n.tag==='th'&&n.textContent==='TPS'));
    assert.deepEqual(timing.map(n=>descendants(n).filter(c=>c.className==='timing-value').map(c=>c.textContent)),[['0.50 s','2.50 s'],['1.25 s','3.25 s'],['—','—'],['0.50 s','2.50 s']]);assert.deepEqual(tps.map(n=>n.textContent),['40','30.77','—','—']);
    assert.deepEqual(descendants(timing[0]).filter(n=>n.className==='timing-label').map(n=>n.textContent),language==='en'?['First token','Total']:['首字','总时间']);
    assert.equal(tps[0].children[0].textContent,language==='en'?' tokens/s':' token/s');assert.match(tps[0].title,/2\.50 s/);assert.match(timing[0].title.split('\n')[0],/0\.50 s$/);assert.match(tps[0].title,/100 token/);assert(!tps[0].title.includes('−'));assert.match(tps[0].title,language==='en'?/request average/:/请求平均速度/);
    d.api.setLanguage(language==='en'?'zh':'en');const translated=descendants(d.elements.get('recent-table'));assert.match(translated.find(n=>n.className==='num tps').title,language==='en'?/请求平均速度/:/request average/);assert.deepEqual(translated.filter(n=>n.className==='timing-label').slice(0,2).map(n=>n.textContent),language==='en'?['首字','总时间']:['First token','Total']);
  });

  test(`${language}: first-token and total timing remain independently visible with partial or invalid measurements`,()=>{
    const d=dashboard({},undefined,{language});d.api.renderSample({recent:[{latency_ms:2500},{ttft_ms:500},{ttft_ms:0,latency_ms:0},{ttft_ms:-1,latency_ms:Infinity},{ttft_ms:'500',latency_ms:null}]});
    const nodes=descendants(d.elements.get('recent-table')),timing=nodes.filter(n=>n.className==='num request-timing');
    assert.deepEqual(timing.map(n=>descendants(n).filter(c=>c.className==='timing-value').map(c=>c.textContent)),[['—','2.50 s'],['0.50 s','—'],['0.00 s','0.00 s'],['—','—'],['—','—']]);assert(nodes.filter(n=>n.className==='num tps').every(n=>n.textContent==='—'));
    const css=html.match(/<style>([\s\S]*?)<\/style>/)[1];assert.match(css,/\.request-timing \.timing-line\{display:block/);
  });

  test(`${language}: persistence toggle posts authenticated settings, shows busy, uses returned state, and preserves cards`,async()=>{
    const sample={...twoAccounts(),persistence:{enabled:true,directory:'/data/cpa/usage'}},requests=[];let release;
    const d=dashboard(login(),async(url,options)=>{requests.push({url,options});return url.endsWith('settings')?new Promise(resolve=>release=payload=>resolve(reply({persistence:payload}))):reply(sample);},{language});
    await d.api.load();const input=d.elements.get('persistence-enabled'),first=panel(d.elements,'account'),other=panel(d.elements,'other'),table=d.elements.get('recent-table').children[0];
    assert.equal(input.checked,true);assert.equal(input.disabled,false);assert.equal(input.getAttribute('aria-label'),language==='en'?'Save data':'保存数据');assert.equal(d.elements.get('persistence-status').textContent,language==='en'?'Enabled':'开启');
    assert.match(input.title,/\/data\/cpa\/usage/);assert.match(input.title,language==='en'?/never deletes saved history/:/不会删除已保存的历史/);assert.match(input.title,language==='en'?/independent folder/:/独立文件夹/);
    input.checked=false;const pending=input.onchange({target:input});assert.equal(input.checked,true);assert.equal(input.disabled,true);assert.equal(input.getAttribute('aria-busy'),'true');assert.equal(d.elements.get('persistence-status').textContent,language==='en'?'Saving…':'保存中…');
    const request=requests.at(-1);assert.equal(request.url,'/v0/management/plugins/cpa-mini-usage/settings');assert.equal(request.options.method,'POST');assert.equal(request.options.headers.Authorization,'Bearer key');assert.equal(request.options.headers['Content-Type'],'application/json');assert.deepEqual(JSON.parse(request.options.body),{persistence_enabled:false});
    release({enabled:false,directory:'/data/{literal}/usage'});await pending;assert.equal(input.checked,false);assert.equal(input.disabled,false);assert.equal(d.elements.get('persistence-status').textContent,language==='en'?'Memory only':'未保存');assert.match(input.title,/\{literal\}/);assert.equal(panel(d.elements,'account'),first);assert.equal(panel(d.elements,'other'),other);assert.equal(d.elements.get('recent-table').children[0],table);assert.deepEqual(d.writes,[]);
    const enable=d.api.savePersistence(true);release({enabled:false,directory:'/returned/actual'});await enable;assert.equal(input.checked,false);assert.match(input.title,/\/returned\/actual/);assert.deepEqual(JSON.parse(requests.at(-1).options.body),{persistence_enabled:true});
    d.api.setLanguage(language==='en'?'zh':'en');assert.equal(d.elements.get('persistence-status').textContent,language==='en'?'未保存':'Memory only');assert.equal(d.writes.length,1);assert.equal(d.writes[0][0],'cpa-mini-usage-language');
  });

  test(`${language}: storage write failures retain previous persistence preference and translate existing errors`,async()=>{
    const sample={...twoAccounts(),persistence:{enabled:false,directory:'/prior/data'}},d=dashboard(login(),async url=>url.endsWith('settings')?reply({error:'storage_write_failed'},500):reply(sample),{language});
    await d.api.load();const other=panel(d.elements,'other');await d.api.savePersistence(true);const input=d.elements.get('persistence-enabled');assert.equal(input.checked,false);assert.equal(input.disabled,false);assert.equal(panel(d.elements,'other'),other);assert.match(input.title,/\/prior\/data/);assert.equal(d.elements.get('persistence-status').textContent,language==='en'?'Cannot save settings. Please retry later.':'无法保存设置，请稍后重试。');
    d.api.setLanguage(language==='en'?'zh':'en');assert.equal(d.elements.get('persistence-status').textContent,language==='en'?'无法保存设置，请稍后重试。':'Cannot save settings. Please retry later.');assert.equal(d.writes.length,1);
  });

  test(`${language}: settings authentication failure clears snapshot and directory tooltip`,async()=>{
    for(const status of [401,403]){
      const d=dashboard(login(),async url=>url.endsWith('settings')?reply({},status):reply({...twoAccounts(),persistence:{enabled:true,directory:'/private/data'}}),{language});await d.api.load();await d.api.savePersistence(false);assert(!panel(d.elements,'account'));assert(d.elements.get('dashboard').classes.has('needs-login'));assert.equal(d.elements.get('persistence-enabled').disabled,true);assert(!d.elements.get('persistence-enabled').title.includes('/private/data'));assert.match(d.elements.get('alert').textContent,language==='en'?/expired/:/失效/);
    }
  });
}

test('old snapshots remain compatible, unknown persistence disabled, and setting-only snapshots preserve every content node',()=>{
  const d=dashboard(),sample={...twoAccounts(),recent:[{output:100}]};d.api.renderSample(sample);assert.equal(d.elements.get('persistence-enabled').disabled,true);assert.equal(d.elements.get('persistence-status').textContent,'—');assert.equal(descendants(d.elements.get('recent-table')).find(n=>n.className==='num tps').textContent,'—');
  const first=panel(d.elements,'account'),table=d.elements.get('recent-table').children[0],summary=d.elements.get('request-summary').children[0];
  for(const persistence of [{enabled:true,directory:'/one'},{enabled:false,directory:'/two'}]){d.api.acceptSnapshot({...sample,persistence});assert.equal(panel(d.elements,'account'),first);assert.equal(d.elements.get('recent-table').children[0],table);assert.equal(d.elements.get('request-summary').children[0],summary);assert.equal(d.elements.get('persistence-enabled').checked,persistence.enabled);assert.match(d.elements.get('persistence-enabled').title,new RegExp(persistence.directory));}
});

for(const change of ['logout','key-change'])for(const storageEvent of [false,true]){
  test(`settings response after ${change} discarded (${storageEvent?'storage event':'response check'})`,async()=>{
    const stored=login(),sample={...twoAccounts(),persistence:{enabled:true,directory:'/private/prior'}},requests=[];let release;
    const d=dashboard(stored,async(url,options)=>{requests.push({url,options});if(url.endsWith('settings'))return new Promise(resolve=>release=()=>resolve(reply({persistence:{enabled:false,directory:'/private/stale'}})));return reply(options.headers.Authorization==='Bearer new-key'?{quotas:[],persistence:{enabled:true,directory:'/new-user'}}:sample);});await d.api.load();const pending=d.api.savePersistence(false);
    if(change==='logout')delete stored.managementKey;else stored.managementKey=JSON.stringify('new-key');if(storageEvent)await d.events.storage({key:'managementKey'});release();await pending;assert(!panel(d.elements,'account'));assert(!/\/private\//.test(d.elements.get('persistence-enabled').title));assert.equal(d.elements.get('persistence-enabled').disabled,change==='logout');assert.equal(d.elements.get('persistence-enabled').checked,true);assert.deepEqual(d.writes,[]);
  });
}

test('settings network or malformed-response errors retain actual preference and do not store it locally',async()=>{
  for(const malformed of [false,true]){
    const sample={...twoAccounts(),persistence:{enabled:true,directory:'/actual'}},d=dashboard(login(),async url=>{if(url.endsWith('settings')){if(malformed)return reply({persistence:{enabled:'false'}});throw Error('access_token=secret');}return reply(sample);});await d.api.load();const before=panel(d.elements,'account');await d.api.savePersistence(false);assert.equal(d.elements.get('persistence-enabled').checked,true);assert.equal(d.elements.get('persistence-status').textContent,'无法保存设置，请稍后重试。');assert.equal(panel(d.elements,'account'),before);assert.deepEqual(d.writes,[]);
  }
});

test('Chinese uses token labels everywhere and recent table scroll is confined internally',()=>{
  assert(!html.includes('\u8bcd\u5143'));const css=html.match(/<style>([\s\S]*?)<\/style>/)[1];assert.match(css,/\.tablewrap\{overflow-x:auto/);assert.match(css,/#recent-table table\{min-width:1220px/);assert.match(css,/\.tablewrap\{max-width:100%;min-width:0\}/);
  const {version}=require('../registry.json').plugins.find(plugin=>plugin.id==='cpa-mini-usage');
  assert.equal(html.match(/<span class="version">v([^<]+)<\/span>/)?.[1],version);
});

test('enabling persistence succeeds using returned setting and directory without redrawing requests',async()=>{
  const sample={...twoAccounts(),persistence:{enabled:false,directory:'/old'}},d=dashboard(login(),async url=>url.endsWith('settings')?reply({persistence:{enabled:true,directory:'/flushed'}}):reply(sample));await d.api.load();const table=d.elements.get('recent-table').children[0];await d.api.savePersistence(true);assert.equal(d.elements.get('persistence-enabled').checked,true);assert.equal(d.elements.get('persistence-status').textContent,'开启');assert.match(d.elements.get('persistence-enabled').title,/\/flushed/);assert.equal(d.elements.get('recent-table').children[0],table);
});

test('persistence updates do not interfere with a running individual account refresh',async()=>{
  const sample={...twoAccounts(),persistence:{enabled:true,directory:'/data'}},posts=[];let release,refreshed=false;
  const d=dashboard(login(),async(url,options)=>{if(url.endsWith('refresh-account')){posts.push(url);return new Promise(resolve=>release=()=>{refreshed=true;resolve(reply({refresh_finished_at:'old'},202));});}if(url.endsWith('settings')){posts.push(url);return reply({persistence:{enabled:false,directory:'/data'}});}return reply(refreshed?{...sample,persistence:{enabled:false,directory:'/data'},account_refresh:{account:{refreshing:false,finished_at:'done'}}}:sample);});await d.api.load();const other=panel(d.elements,'other'),pending=d.api.refreshAccount('account');await d.api.savePersistence(false);assert.equal(refreshButton(panel(d.elements,'account')).disabled,true);assert.equal(refreshButton(other).disabled,false);release();await pending;assert.equal(panel(d.elements,'other'),other);assert.equal(d.elements.get('persistence-enabled').checked,false);assert.equal(posts.length,2);
});

for(const language of ['zh','en']){
  test(`${language}: request top refresh is GET-only, visible, and independent of remote quota busy state`,async()=>{
    const sample={...twoAccounts(),quota_refreshing:true,account_refresh:{other:{refreshing:true}},quota_updated_at:'2020-01-01T00:00:00Z'},calls=[];let release;
    const d=dashboard(login(),async(url,options)=>{calls.push({url,options});return calls.length===1?reply(sample):new Promise(resolve=>release=()=>resolve(reply({...sample,generated_at:'2026-01-01T12:34:56Z',quota_refreshing:false,quotas:sample.quotas.map(q=>({...q,name:'changed-quota'})),recent:[{model:'fresh-request'}]})));},{language});
    await d.api.load();assert.equal(d.elements.get('refresh').disabled,true);d.api.setTab('requests');assert.equal(d.elements.get('refresh-label').textContent,language==='en'?'Refresh requests':'刷新请求');assert.equal(d.elements.get('refresh').disabled,false);assert(d.elements.get('dashboard').classes.has('request-view'));
    const first=panel(d.elements,'account'),other=panel(d.elements,'other'),quotaBusy=d.elements.get('page-quota').getAttribute('aria-busy'),pending=d.elements.get('refresh').onclick();
    assert.equal(d.elements.get('refresh-label').textContent,language==='en'?'Syncing requests…':'正在同步请求…');assert.equal(d.elements.get('status').textContent,language==='en'?'Syncing requests…':'正在同步请求…');assert.equal(d.elements.get('refresh').disabled,true);assert.equal(d.elements.get('page-requests').getAttribute('aria-busy'),'true');assert.equal(d.elements.get('page-quota').getAttribute('aria-busy'),quotaBusy);assert.equal(refreshButton(first).disabled,false);assert.equal(refreshButton(other).disabled,true);
    release();await pending;assert.equal(calls.length,2);assert(calls.every(c=>c.url.endsWith('/data')&&c.options.method==='GET'));assert.equal(panel(d.elements,'account'),first);assert.equal(panel(d.elements,'other'),other);assert.equal(refreshButton(other).disabled,true);assert(descendants(d.elements.get('recent-table')).some(n=>n.textContent==='fresh-request'));assert.equal(d.elements.get('status').textContent,language==='en'?'Requests synced':'请求已同步');assert.match(d.elements.get('updated').textContent,language==='en'?/^Requests synced /:/^请求同步 /);assert.match(d.elements.get('updated').title,/2026/);assert(!/quota|额度/i.test(d.elements.get('updated').title));
    d.api.setTab('quota');assert.equal(d.elements.get('refresh-label').textContent,language==='en'?'Refresh all':'刷新全部');assert.equal(d.elements.get('refresh').disabled,true);assert(!d.elements.get('dashboard').classes.has('request-view'));assert.match(d.elements.get('updated').textContent,language==='en'?/^Quota updated /:/^额度更新 /);
  });

  test(`${language}: switching tabs/language immediately reflects refresh label and generated_at request timestamp`,async()=>{
    const sample={...twoAccounts(),generated_at:'2026-06-15T03:45:00Z',quota_updated_at:'2020-06-15T00:00:00Z',quota_refreshing:true};const d=dashboard(login(),async()=>reply(sample),{language});await d.api.load();d.api.setTab('requests');const opposite=language==='en'?'zh':'en';d.api.setLanguage(opposite);assert.equal(d.elements.get('refresh-label').textContent,opposite==='en'?'Refresh requests':'刷新请求');assert.equal(d.elements.get('refresh').getAttribute('aria-label'),d.elements.get('refresh-label').textContent);assert.equal(d.elements.get('status').textContent,opposite==='en'?'Requests synced':'请求已同步');assert.match(d.elements.get('updated').title,/2026/);assert(!/2020|quota|额度/i.test(d.elements.get('updated').title));d.api.setTab('quota');assert.equal(d.elements.get('refresh-label').textContent,opposite==='en'?'Refresh all':'刷新全部');assert.match(d.elements.get('updated').title,/2020/);
  });

  test(`${language}: request pagination defaults to 20, switches 40/60, resets pages, filters, and clamps shorter snapshots`,()=>{
    const d=dashboard({},undefined,{language}),sample={recent:Array.from({length:75},(_,i)=>({provider:i%2?'claude':'codex',model:'model-'+i}))};d.api.setTab('requests');d.api.renderSample(sample);const rows=()=>descendants(d.elements.get('recent-table')).filter(n=>n.tag==='tbody')[0].children;
    assert.equal(d.elements.get('recent-size').value,'20');assert.equal(rows().length,20);assert.equal(d.elements.get('recent-page').textContent,'1 / 3');assert.equal(d.elements.get('recent-caption').textContent,language==='en'?'Showing 1–20 / 60':'显示 1–20 / 60 条');d.elements.get('recent-next').onclick();assert.equal(d.elements.get('recent-page').textContent,'2 / 3');
    for(const [size,expected,pages] of [['40',40,'1 / 2'],['60',60,'1 / 1']]){d.elements.get('recent-size').onchange({target:{value:size}});assert.equal(rows().length,expected);assert.equal(d.elements.get('recent-page').textContent,pages);assert.equal(d.elements.get('recent-caption').textContent,language==='en'?`Showing 1–${size} / 60`:`显示 1–${size} / 60 条`);}
    d.elements.get('recent-size').onchange({target:{value:'20'}});d.elements.get('recent-next').onclick();d.elements.get('provider').onchange({target:{value:'codex'}});assert.equal(d.elements.get('recent-page').textContent,'1 / 2');assert.equal(d.elements.get('recent-caption').textContent,language==='en'?'Showing 1–20 / 30':'显示 1–20 / 30 条');d.elements.get('recent-next').onclick();assert.equal(rows().length,10);d.api.acceptSnapshot({recent:sample.recent.slice(0,5)});assert.equal(d.elements.get('recent-page').textContent,'1 / 1');assert.equal(rows().length,3);assert.equal(d.elements.get('recent-next').disabled,true);assert.deepEqual(d.writes,[]);
  });
}

test('request snapshot refresh does not cancel or complete the individual account polling job',async()=>{
  const sample=twoAccounts(),calls=[];let releaseAccount,complete=false;
  const d=dashboard(login(),async(url,options)=>{calls.push({url,options});if(url.endsWith('refresh-account'))return new Promise(resolve=>releaseAccount=()=>{complete=true;resolve(reply({refresh_finished_at:'baseline'},202));});if(complete)return reply({...sample,account_refresh:{account:{refreshing:false,finished_at:'done'}},quotas:sample.quotas.map(q=>q.auth_id==='account'?{...q,manual_reset_remaining:3}:q),recent:[{model:'post-account'}]});return reply({...sample,recent:[{model:calls.length>1?'request-during-account':'initial'}],account_refresh:{account:{refreshing:false,finished_at:'fake-completion'}}});});
  await d.api.load();const other=panel(d.elements,'other'),first=panel(d.elements,'account'),account=d.api.refreshAccount('account');d.api.setTab('requests');assert.equal(d.elements.get('refresh').disabled,false);await d.elements.get('refresh').onclick();assert.equal(refreshButton(first).disabled,true);assert.equal(panel(d.elements,'account'),first);assert.equal(panel(d.elements,'other'),other);assert(descendants(d.elements.get('recent-table')).some(n=>n.textContent==='request-during-account'));releaseAccount();await account;assert.notEqual(panel(d.elements,'account'),first);assert.equal(panel(d.elements,'other'),other);assert.equal(refreshButton(panel(d.elements,'account')).disabled,false);assert(calls.filter(c=>c.options.method==='POST').every(c=>c.url.endsWith('refresh-account')));assert.equal(calls.filter(c=>c.url.endsWith('refresh-account')).length,1);
});

test('failed request snapshot retains quota/request nodes and language-switchable error feedback',async()=>{
  let fail=false;const d=dashboard(login(),async()=>reply(twoAccounts(),fail?503:200));await d.api.load();const card=panel(d.elements,'account'),table=d.elements.get('recent-table').children[0];d.api.setTab('requests');fail=true;await d.elements.get('refresh').onclick();assert.equal(panel(d.elements,'account'),card);assert.equal(d.elements.get('recent-table').children[0],table);assert.equal(d.elements.get('status').textContent,'请求同步失败');d.api.setLanguage('en');assert.equal(d.elements.get('status').textContent,'Request sync failed');assert.equal(d.elements.get('refresh-label').textContent,'Refresh requests');assert.match(d.elements.get('alert').textContent,/HTTP 503/);
});

for(const change of ['logout','key-change']){
  test(`request snapshot authentication ${change} discards stale data and clears privacy`,async()=>{
    const stored=login();let release,delayed=false;const d=dashboard(stored,async(url,options)=>{if(options.headers.Authorization==='Bearer new-key')return reply({quotas:[],recent:[]});return delayed?new Promise(resolve=>release=()=>resolve(reply({...twoAccounts(),recent:[{model:'private-stale'}]}))):reply(twoAccounts());});await d.api.load();d.api.setTab('requests');delayed=true;const pending=d.elements.get('refresh').onclick();if(change==='logout')delete stored.managementKey;else stored.managementKey=JSON.stringify('new-key');release();await pending;assert(!panel(d.elements,'account'));assert(!descendants(d.elements.get('recent-table')).some(n=>n.textContent==='private-stale'));assert.equal(d.elements.get('refresh').disabled,false);
  });
}

test('request table CSS retains its independent width and internal scrolling',()=>{
  const css=html.match(/<style>([\s\S]*?)<\/style>/)[1];assert.match(css,/main\{max-width:1600px/);assert.match(css,/main\.request-view\{max-width:1480px\}/);assert.match(css,/\.request-view #page-requests th,\.request-view #page-requests td\{padding:7px 8px\}/);assert.match(css,/\.tablewrap\{overflow-x:auto/);assert(!html.includes('\u8bcd\u5143'));
});

test('pending requests keep explicit feedback across tab/language switches without quota loading changes',async()=>{
  let release,delayed=false;const sample=twoAccounts(),d=dashboard(login(),async()=>delayed?new Promise(resolve=>release=()=>resolve(reply(sample))):reply(sample));await d.api.load();const quota=panel(d.elements,'account');d.api.setTab('requests');delayed=true;const pending=d.elements.get('refresh').onclick();d.api.setLanguage('en');assert.equal(d.elements.get('refresh-label').textContent,'Syncing requests…');assert.equal(d.elements.get('status').textContent,'Syncing requests…');d.api.setTab('quota');assert.equal(d.elements.get('refresh-label').textContent,'Refresh all');assert.equal(d.elements.get('page-quota').getAttribute('aria-busy'),'false');assert.equal(d.elements.get('refresh').disabled,true);d.api.setTab('requests');assert.equal(d.elements.get('refresh-label').textContent,'Syncing requests…');release();await pending;assert.equal(d.elements.get('refresh-label').textContent,'Refresh requests');assert.equal(d.elements.get('status').textContent,'Requests synced');
});

test('quiet automatic request snapshots retain header labels and card identity',async()=>{
  let release,delayed=false;const sample=twoAccounts(),d=dashboard(login(),async()=>reply(delayed?await new Promise(resolve=>release=()=>resolve(sample)):sample));await d.api.load();d.api.setTab('requests');const card=panel(d.elements,'account');delayed=true;const pending=d.api.load();assert.equal(d.elements.get('refresh-label').textContent,'刷新请求');assert.equal(d.elements.get('status').textContent,'请求已同步');release();await pending;assert.equal(d.elements.get('refresh-label').textContent,'刷新请求');assert.equal(d.elements.get('status').textContent,'请求已同步');assert.equal(panel(d.elements,'account'),card);
});

test('quota token tooltip exposes observed counts and total arithmetic in both languages',()=>{
  for(const language of ['zh','en']){
    const d=dashboard({},undefined,{language}),sample=sampleSnapshot();sample.quotas[0].five_hour.observed_tokens=1000000;sample.quotas[0].five_hour.observed_cost=30;
    d.api.renderSample(sample);const metric=descendants(d.elements.get('quota-cards')).find(n=>n.className==='estimate-value');
    assert.match(metric.title,/1,000,000/);assert.match(metric.title,/25%/);assert(!/75%|remaining|剩余/.test(metric.title));
  }
});

for(const language of ['zh','en'])test(`${language}: total input includes cache once without uncached column; TPS uses output only`,()=>{
  const d=dashboard({},undefined,{language});
  const claude={auth_id:'a',provider:'claude',model:'opus',input:2,cache_read:60590,cache_write:10960,output:100,total:71652,ttft_ms:500,latency_ms:2500};
  const codex={...claude,auth_id:'b',provider:'codex',input:1000,cache_read:600,cache_write:100,total:1100};
  d.api.renderSample({recent:[claude,codex],credentials:[{auth_id:'a',provider:'claude',input:2,total_input:71552,uncached_input:2,cached:60590,cache_write:10960,output:100,total:71652,requests:1,cost:1}]});
  const nodes=descendants(d.elements.get('recent-table')),rows=nodes.filter(n=>n.tag==='tr'&&n.children[0]?.tag==='td');
  assert.deepEqual(rows[0].children.slice(4,6).map(n=>n.textContent),['71.55k','100']);
  assert.match(rows[0].children[4].title,/71,552/);assert.match(rows[0].children[4].title,language==='en'?/includes ordinary input/:/包含普通输入/);
  assert.deepEqual(rows[1].children.slice(4,6).map(n=>n.textContent),['1k','100']);assert.match(rows[1].children[4].title,language==='en'?/do not add/:/不能重复相加/);
  assert.equal(d.api.requestTPS(claude),40);assert.equal(d.api.requestTPS({...codex,input:1e9,cache_read:1e9,total:2e9}),40);
  assert(nodes.some(n=>n.tag==='th'&&n.textContent===(language==='en'?'Total input':'总输入')));assert(!nodes.some(n=>n.tag==='th'&&/非缓存|Uncached/.test(n.textContent)));
  const summaryNodes=descendants(d.elements.get('credential-table')),summaryRow=summaryNodes.find(n=>n.tag==='tr'&&n.children[0]?.tag==='td');
  assert.deepEqual(summaryRow.children.slice(3,5).map(n=>n.textContent),['71.55k','100']);assert(!summaryNodes.some(n=>n.tag==='th'&&/非缓存|Uncached/.test(n.textContent)));summaryNodes.find(n=>n.id==='credential-toggle-0').onclick();assert.equal(descendants(d.elements.get('credential-table')).find(n=>n.className==='details-cell').colSpan,8);
});
test('3-percent used quota displays estimated total tokens and cost, not remaining',()=>{
  const d=dashboard();d.api.renderSample({quotas:[{auth_id:'a',provider:'claude',five_hour:{used_percent:3,estimated_total_tokens:4000000,estimated_total_cost:12,observed_tokens:120000,observed_cost:.36}}]});
  const nodes=descendants(d.elements.get('quota-cards'));
  assert(nodes.some(n=>n.className==='estimate-value'&&n.textContent==='4m token'));
  assert(nodes.some(n=>n.className==='estimate-cost'&&n.textContent==='$12.00'));
});

for(const language of ['zh','en']){
  test(`${language}: quota used money and tokens share rows with total estimates for each window`,()=>{
    const d=dashboard({},undefined,{language});d.api.renderSample({quotas:[{auth_id:'a',provider:'claude',five_hour:{used_percent:25,observed_cost:30,observed_tokens:1000000,estimated_total_cost:120,estimated_total_tokens:4000000},seven_day:{used_percent:50,observed_cost:90,observed_tokens:3000000,estimated_total_cost:180,estimated_total_tokens:6000000}}]});
    const stats=descendants(d.elements.get('quota-cards')).filter(n=>n.className==='windowstats'),suffix=language==='en'?'tokens':'token';
    assert.deepEqual(stats[0].children[0].children.map(n=>n.textContent),language==='en'?['Used (observed)','Est. total']:['已用（观测）','预估总额']);
    assert.deepEqual(stats[0].children[1].children.map(n=>n.textContent),['$30.00','$120.00']);assert.deepEqual(stats[1].children[1].children.map(n=>n.textContent),['$90.00','$180.00']);
    assert.deepEqual(stats[0].children[2].children.map(n=>n.textContent),['1m '+suffix,'4m '+suffix]);assert.deepEqual(stats[1].children[2].children.map(n=>n.textContent),['3m '+suffix,'6m '+suffix]);
    assert.match(stats[0].children[2].children[0].title,/1,000,000/);assert.match(stats[0].children[2].children[0].title,language==='en'?/this CPA instance/:/本机/);assert.match(stats[0].children[1].children[0].title,language==='en'?/not an official bill/:/不是官方账单/);
    d.api.setLanguage(language==='en'?'zh':'en');assert.deepEqual(descendants(d.elements.get('quota-cards')).filter(n=>n.className==='usage-label').slice(0,2).map(n=>n.textContent),language==='en'?['已用（观测）','预估总额']:['Used (observed)','Est. total']);
  });
  test(`${language}: quota used values preserve zero and unknown, and missing prices hide both dollar values only`,()=>{
    const d=dashboard({},undefined,{language}),suffix=language==='en'?'tokens':'token';d.api.renderSample({quotas:[{provider:'codex',five_hour:{used_percent:0,observed_cost:0,observed_tokens:0},seven_day:{used_percent:25,observed_tokens:1000000,estimated_total_tokens:4000000,missing_price:true}},{provider:'claude'}]});
    const stats=descendants(d.elements.get('quota-cards')).filter(n=>n.className==='windowstats');
    assert(stats.some(s=>s.children[1].children[0].textContent==='$0.000'&&s.children[2].children[0].textContent==='0 '+suffix&&s.children[1].children[1].textContent==='—'));
    const missing=stats.find(s=>s.children[2].children[0].textContent==='1m '+suffix);assert.deepEqual(missing.children[1].children.map(n=>n.textContent),['—','—']);assert.equal(missing.children[2].children[1].textContent,'4m '+suffix);assert(missing.children[1].children.every(n=>n.title===(language==='en'?'Model price missing; amount unavailable':'缺少模型单价，金额暂不可用')));
    assert(stats.some(s=>s.children[1].children.every(n=>n.textContent==='—')&&s.children[2].children.every(n=>n.textContent==='— '+suffix)));
  });
}
test('quota metrics retain readable sizing; compact cards stack windows only below 350px',()=>{
  const css=html.match(/<style>([\s\S]*?)<\/style>/)[1];assert.match(css,/\.usage-row\{display:grid;grid-template-columns:repeat\(2,minmax\(0,1fr\)\)/);assert.match(css,/\.observed-cost\{font-size:15px/);assert.match(css,/\.observed-value\{font-size:12px/);assert.match(css,/@media\(max-width:350px\)\{\.windows\{grid-template-columns:minmax\(0,1fr\)\}/);assert.match(css,/minmax\(min\(100%,320px\),336px\)/);assert.match(css,/\.provider-accounts\{display:flex;flex-direction:column;gap:8px/);assert.match(css,/\.account\{[^}]*white-space:nowrap;overflow:hidden;text-overflow:ellipsis/);assert(!/\.account-browser\{[^}]*position:sticky/.test(css));assert(!/\.panel\.has-groups\{grid-column:1\/-1/.test(css));
});
test('refresh and language controls live below tabs, outside CPA top-right overlay area',()=>{
  const d=dashboard(),root=d.elements.get('dashboard'),header=root.children.find(n=>n.tag==='header'),toolbar=root.children.find(n=>n.getAttribute('class')==='toolbar');
  assert(!descendants(header).includes(d.elements.get('refresh')));assert(!descendants(header).includes(d.elements.get('language')));
  assert(descendants(toolbar).includes(d.elements.get('refresh')));assert(descendants(toolbar).includes(d.elements.get('language')));
  assert(root.children.indexOf(toolbar)>root.children.findIndex(n=>n.getAttribute('role')==='tablist'));
  const actions=descendants(toolbar).find(n=>n.getAttribute('class')==='header-actions');assert.equal(actions.children[0],d.elements.get('refresh'));
  assert.equal(descendants(root).filter(n=>n.id==='refresh').length,1);
  const css=html.match(/<style>([\s\S]*?)<\/style>/)[1];assert.match(css,/\.toolbar \.header-actions\{flex-wrap:wrap;justify-content:flex-start;width:auto/);
});
test('sign-in gate keeps toolbar language selection while hiding private-data controls',()=>{
  const css=html.match(/<style>([\s\S]*?)<\/style>/)[1];assert(!/\.needs-login \.toolbar\s*[,\{]/.test(css));assert.match(css,/\.needs-login \.filter-control/);assert.match(css,/\.needs-login #refresh/);
});

for(const language of ['zh','en']){
  test(`${language}: every request has cost including zero, small, failed and unknown prices`,()=>{
    const d=dashboard({},undefined,{language});d.api.renderSample({recent:[{cost:1.25},{cost:.001185},{cost:0},{cost:.004925,failed:true},{cost:9,missing_price:true},{},{cost:-1},{cost:Infinity},{cost:'1.2'}]});
    const nodes=descendants(d.elements.get('recent-table')),costs=nodes.filter(n=>n.className==='num usd-value request-cost');
    assert(nodes.some(n=>n.tag==='th'&&n.textContent===(language==='en'?'Cost':'金额')));assert.deepEqual(costs.map(n=>n.textContent),['$1.25','$0.001185','$0.000','$0.004925','—','—','—','—','—']);assert.match(costs[1].title,/0\.001185 USD/);assert.match(costs[1].title,language==='en'?/not a subscription charge/:/不是订阅扣款/);assert.equal(costs[4].title,language==='en'?'Model price missing; amount unavailable':'缺少模型单价，金额暂不可用');
    assert.match(costs[0].title,language==='en'?/including applicable long-context tiers/:/包含适用的长上下文档位/);
    d.api.setLanguage(language==='en'?'zh':'en');const translated=descendants(d.elements.get('recent-table')).find(n=>n.className==='num usd-value request-cost');assert.match(translated.title,language==='en'?/不是订阅扣款/:/not a subscription charge/);
  });
  test(`${language}: incomplete and inconsistent usage explain unknown cost`,()=>{
    const d=dashboard({},undefined,{language});d.api.renderSample({recent:[{total:1000,missing_price:true,usage_quality:'unclassified'},{input:100,cache_read:200,missing_price:true,usage_quality:'inconsistent'}]});
    const costs=descendants(d.elements.get('recent-table')).filter(n=>n.className==='num usd-value request-cost');assert.deepEqual(costs.map(n=>n.textContent),['—','—']);assert(costs.every(n=>n.title===(language==='en'?'Usage buckets are incomplete or inconsistent; amount unavailable':'用量分桶不完整或不一致，金额暂不可用')));
  });
  test(`${language}: previous-cycle forecasts explain their actual basis without quota arithmetic`,()=>{
    const d=dashboard({},undefined,{language});d.api.renderSample({quotas:[{provider:'codex',five_hour:{used_percent:0,observed_tokens:100,observed_cost:.01,estimated_total_tokens:6400,estimated_total_cost:.06,estimated_basis:'previous'}}]});
    const nodes=descendants(d.elements.get('quota-cards')),metric=nodes.find(n=>n.className==='estimate-value'),cost=nodes.find(n=>n.className==='estimate-cost');assert.equal(metric.textContent,'6.4k '+(language==='en'?'tokens':'token'));assert.match(metric.title,language==='en'?/previous cycle/:/上一周期/);assert.equal(cost.title,metric.title.split('\n').slice(1).join('\n'));assert(!/×|÷/.test(cost.title));
  });
}
test('total input handles provider cache contracts and aggregate total without deriving uncached input',()=>{
  const {api}=dashboard();assert.equal(api.totalInput({provider:'claude',input:2,cache_read:60590,cache_write:10960}),71552);assert.equal(api.totalInput({provider:'codex',input:1000,cache_read:600,cache_write:100}),1000);assert.equal(api.totalInput({total_input:71552,uncached_input:999}),71552);assert.equal(api.totalInput({provider:'codex',input:1000,cache_read:2000}),1000);assert.equal(api.totalInput({provider:'claude',input:2,cache_read:-1}),null);assert.equal(api.totalInput({}),null);assert.equal(api.totalInput({total_input:0}),0);assert(!html.includes('非缓存输入'));assert(!source.includes('remainingEstimate'));
});
for(const language of ['zh','en'])test(`${language}: missing previous price does not hide current known observed cost`,()=>{
  const d=dashboard({},undefined,{language});d.api.renderSample({quotas:[{provider:'codex',five_hour:{used_percent:0,observed_tokens:5000,observed_cost:.005,estimated_total_tokens:50000,estimated_basis:'previous',missing_price:true}}]});const nodes=descendants(d.elements.get('quota-cards')),actual=nodes.find(n=>n.className==='observed-cost'),estimate=nodes.find(n=>n.className==='estimate-cost');assert.equal(actual.textContent,'$0.005');assert.match(actual.title,language==='en'?/not an official bill/:/不是官方账单/);assert.equal(estimate.textContent,'—');
});
for(const language of ['zh','en'])test(`${language}: missing TTFT does not hide measurable request-average TPS`,()=>{
  const d=dashboard({},undefined,{language});d.api.renderSample({recent:[{output:20,latency_ms:1500},{output:20,latency_ms:1500,ttft_ms:100},{output:20,latency_ms:1500,ttft_ms:2000},{output:1,latency_ms:2500,stream:false}]});const nodes=descendants(d.elements.get('recent-table'));assert.deepEqual(nodes.filter(n=>n.className==='num tps').map(n=>n.textContent),['13.33','13.33','13.33','0.4']);assert(nodes.some(n=>n.className==='timing-value'&&n.textContent==='—'));
});
for(const language of ['zh','en'])test(`${language}: paired estimate remains visible while actuals advance and tooltip uses paired usage`,()=>{
  const d=dashboard({},undefined,{language}),win={used_percent:25,observed_tokens:110000,observed_cost:.11,estimated_total_tokens:400000,estimated_total_cost:.4,estimated_basis:'quota',estimated_at:'2026-10-10T02:00:00Z',estimate_observed_tokens:100000,estimate_observed_cost:.1};d.api.renderSample({quotas:[{provider:'codex',five_hour:win}]});let nodes=descendants(d.elements.get('quota-cards')),token=nodes.find(n=>n.className==='estimate-value'),amount=nodes.find(n=>n.className==='estimate-cost');assert.equal(token.textContent,'400k '+(language==='en'?'tokens':'token'));assert.equal(amount.textContent,'$0.400');assert.match(token.title,/100,000/);assert(!token.title.includes('110,000'));assert.match(token.title,language==='en'?/Estimate basis time/:/估算依据时间/);assert.match(amount.title,/\$0\.100/);
  d.api.renderSample({quotas:[{provider:'codex',five_hour:{...win,observed_tokens:120000,observed_cost:null,missing_price:true}}]});nodes=descendants(d.elements.get('quota-cards'));assert.equal(nodes.find(n=>n.className==='estimate-cost').textContent,'$0.400');assert.match(nodes.find(n=>n.className==='estimate-cost').title,language==='en'?/re-pairs/:/重新配对/);assert.equal(nodes.find(n=>n.className==='observed-cost').textContent,'—');
});
for(const language of ['zh','en'])test(`${language}: retained cycle forecast does not claim a new zero-percent formula`,()=>{
  const d=dashboard({},undefined,{language});d.api.renderSample({quotas:[{provider:'codex',five_hour:{used_percent:0,observed_tokens:110000,observed_cost:.11,estimated_total_tokens:400000,estimated_total_cost:.4,estimated_basis:'quota',estimate_used_percent:25,estimate_retained:true,estimated_at:'2026-10-10T02:00:00Z'}}]});const nodes=descendants(d.elements.get('quota-cards')),amount=nodes.find(n=>n.className==='estimate-cost'),token=nodes.find(n=>n.className==='estimate-value');assert.equal(amount.textContent,'$0.400');assert.equal(token.textContent,'400k '+(language==='en'?'tokens':'token'));assert.match(amount.title,language==='en'?/Retaining the last valid estimate/:/保留本周期上次有效预估/);assert(!/÷ 0%|\/ 0%/.test(amount.title));
});

test('tech palette separates official quota, actual cost and forecast with accessible contrast in both themes',()=>{
  const css=html.match(/<style>([\s\S]*?)<\/style>/)[1];
  assert.match(css,/\.percent\{[^}]*color:var\(--accent\)/);
  assert.match(css,/\.observed-cost\{[^}]*color:var\(--cost\)/);
  assert.match(css,/\.estimate-cost\{[^}]*color:var\(--forecast\)/);
  assert.match(css,/\.usage-label:nth-child\(2\)\{color:var\(--forecast\)/);
  assert.match(css,/--mono:ui-monospace/);assert(!/@import|url\(/.test(css));
  const themes=[...css.matchAll(/:root\{([^}]+)\}/g)].map(m=>Object.fromEntries([...m[1].matchAll(/(--[\w-]+):(#(?:[\da-f]{6}|[\da-f]{3}))(?=;|$)/g)].map(v=>[v[1],v[2]])));
  assert.equal(themes.length,2);
  function luminance(hex){let digits=hex.slice(1);if(digits.length===3)digits=[...digits].map(c=>c+c).join('');return digits.match(/../g).map(v=>parseInt(v,16)/255).map(v=>v<=.04045?v/12.92:((v+.055)/1.055)**2.4).reduce((sum,v,i)=>sum+v*[.2126,.7152,.0722][i],0);}
  function contrast(a,b){const values=[luminance(a),luminance(b)].sort((a,b)=>b-a);return (values[0]+.05)/(values[1]+.05);}
  for(const theme of themes){
    for(const key of ['--ink','--muted','--accent','--forecast','--cost','--bad'])assert(contrast(theme[key],theme['--panel'])>=4.5,`${key} must be readable on card surfaces`);
    assert.notEqual(theme['--accent'],theme['--forecast']);assert.notEqual(theme['--cost'],theme['--forecast']);
  }
});

for(const language of ['zh','en'])test(`${language}: unused five-hour quota displays zero totals without previous-cycle arithmetic`,()=>{
  const d=dashboard({},undefined,{language}),suffix=language==='en'?'tokens':'token';
  d.api.renderSample({quotas:[{provider:'codex',five_hour:{used_percent:0,observed_cost:0,observed_tokens:0,estimated_total_cost:0,estimated_total_tokens:0,estimated_basis:'unused'}}]});
  const nodes=descendants(d.elements.get('quota-cards')),stats=nodes.find(n=>n.className==='windowstats');
  assert.equal(nodes.find(n=>n.className==='percent').textContent,'100.0');
  assert.deepEqual(stats.children[0].children.map(n=>n.textContent),language==='en'?['Used (observed)','Est. total']:['已用（观测）','预估总额']);
  assert.deepEqual(stats.children[1].children.map(n=>n.textContent),['$0.000','$0.000']);
  assert.deepEqual(stats.children[2].children.map(n=>n.textContent),['0 '+suffix,'0 '+suffix]);
  for(const metric of stats.children.slice(1).flatMap(row=>row.children).filter(n=>n.className.startsWith('estimate-'))){
    assert.match(metric.title,language==='en'?/No usage yet in this five-hour window/:/本 5 小时窗口尚未使用/);
    assert(!/×|÷/.test(metric.title));
  }
  d.api.setLanguage(language==='en'?'zh':'en');
  assert.match(descendants(d.elements.get('quota-cards')).find(n=>n.className==='estimate-cost').title,language==='en'?/本 5 小时窗口尚未使用/:/No usage yet in this five-hour window/);
});

test('model observations hide matching responses and preserve other states and aliases without changing cost',()=>{
  const {api,elements}=dashboard();
  const recent=[
    {model:'gpt-5',requested_model:'client-alias',response_model:'gpt-5',model_status:'same',cost:.0028},
    {model:'gpt-5',response_model:'gpt-5-20260901',model_status:'variant'},
    {model:'gpt-5',response_model:'gpt-5-mini',model_status:'different'},
    {model:'legacy'},
    {model:'gpt-5',response_model:'',model_status:'same'},
  ];
  api.renderSample({recent,credentials:[],quotas:[]});
  const cells=descendants(elements.get('recent-table')).filter(n=>n.className==='request-model');
  assert.equal(cells.length,5);
  const badges=cells.map(c=>descendants(c).find(n=>n.className?.startsWith('model-badge')));
  assert.deepEqual(badges.map(n=>n?.textContent??null),[null,'版本变体','模型不一致','响应模型未知','响应模型未知']);
  assert(!descendants(cells[0]).some(n=>n.className==='model-observation model-response'));
  assert.equal(badges[2].parentNode.children[0].textContent,'↳ gpt-5-mini');
  assert.match(html,/\.model-badge\.different\{color:var\(--warm\);background:var\(--warm-soft\);border-color:var\(--warm\)/);
  assert.equal(cells[0].children[0].textContent,'client-alias');
  assert.match(cells[0].title,/执行模型（用量记录）: gpt-5/);
  assert.match(cells[0].title,/不保证等于最终出站模型/);
  assert.match(cells[3].title,/旧记录不会自动补全/);
  assert(descendants(elements.get('recent-table')).some(n=>n.textContent==='$0.0028'));
  api.setLanguage('en');
  const translated=descendants(elements.get('recent-table')).filter(n=>n.className?.startsWith('model-badge'));
  assert.deepEqual(translated.map(n=>n.textContent),['Name variant','Model mismatch','Response model unknown','Response model unknown']);
  assert.match(translated[1].title,/not proof of model authenticity/);
  assert(!/[\u4e00-\u9fff]/.test(translated[1].title));
});

test('identical model names render a single model line in both languages',()=>{
  const {api,elements}=dashboard();
  api.renderSample({recent:[{model:'gpt-6.1-sol',requested_model:'gpt-6.1-sol',response_model:'gpt-6.1-sol',model_status:'same'}]});
  for(const language of ['zh','en']){
    api.setLanguage(language);
    const cell=descendants(elements.get('recent-table')).find(n=>n.className==='request-model');
    assert.equal(cell.children.length,1);
    assert.equal(cell.children[0].textContent,'gpt-6.1-sol');
    assert.match(cell.title,/gpt-6\.1-sol/);
  }
});

test('response model strings are literal text, and missing execution name is unknown',()=>{
  const {api,elements}=dashboard();
  const attack='<img src=x onerror=alert(1)>';
  api.renderSample({recent:[{model:'safe',requested_model:attack,response_model:attack,model_status:'different'},{response_model:'gpt',model_status:'same'}]});
  const nodes=descendants(elements.get('recent-table'));
  assert.equal(nodes.filter(n=>n.textContent===attack||n.textContent==='↳ '+attack).length,2);
  assert(!nodes.some(n=>n.tag==='img'));
  assert.deepEqual(nodes.filter(n=>n.className?.startsWith('model-badge')).map(n=>n.textContent),['模型不一致','响应模型未知']);
});

test('storage read-only protection is explicit and cannot be toggled away',async()=>{
  const requests=[];
  const sample={credentials:[],quotas:[],recent:[],persistence:{enabled:true,directory:'/persistent/usage',read_only:true}};
  const d=dashboard(login(),async(url)=>{requests.push(url);return reply(sample);});
  await d.api.load();
  assert.equal(d.elements.get('persistence-enabled').disabled,true);
  assert.equal(d.elements.get('persistence-status').textContent,'只读保护');
  assert.match(d.elements.get('persistence-control').title,/仅保留在内存/);
  await d.api.savePersistence(false);
  assert(!requests.some(url=>url.endsWith('/settings')));
  d.api.setLanguage('en');
  assert.equal(d.elements.get('persistence-status').textContent,'Read-only protection');
  assert.match(d.elements.get('persistence-control').title,/memory only/);
  assert.match(d.elements.get('persistence-control').title,/\/persistent\/usage/);
});

const resetButton=card=>descendants(card).find(n=>n.className==='button account-action account-reset');
const toggleButton=card=>descendants(card).find(n=>n.className==='account-switch');
const accountNoteNode=card=>descendants(card).find(n=>n.className==='account-note');
function actionAccounts(){const sample=sampleSnapshot();return {...sample,quotas:[
  {auth_id:'codex-a',provider:'codex',name:'alice@example.com',manual_reset_remaining:2,five_hour:{used_percent:100}},
  {auth_id:'codex-zero',provider:'codex',name:'zero@example.com',manual_reset_remaining:0},
  {auth_id:'codex-off',provider:'codex',name:'off@example.com',manual_reset_remaining:3,disabled:true},
  {auth_id:'claude-a',provider:'claude',name:'claude@example.com',manual_reset_remaining:1},
]};}

for(const language of ['zh','en']){
  test(`${language}: reset and status controls render only where supported and are translated`,()=>{
    const d=dashboard({},undefined,{language});d.api.renderSample(actionAccounts());
    const a=panel(d.elements,'codex-a'),zero=panel(d.elements,'codex-zero'),off=panel(d.elements,'codex-off'),claude=panel(d.elements,'claude-a');
    assert(resetButton(a));assert(!resetButton(zero));assert(!resetButton(off));assert(resetButton(claude),'Claude accounts with credits get the same reset button');
    assert.equal(resetButton(a).textContent,language==='en'?'Use reset':'使用重置卡');assert.equal(resetButton(claude).textContent,language==='en'?'Use reset':'使用重置卡');
    assert.equal(resetButton(claude).id,'account-reset-claude-a');
    for(const card of [a,off]){assert.equal(toggleButton(card).getAttribute('role'),'switch');assert.equal(toggleButton(card).children[0].getAttribute('aria-hidden'),'true');}
    assert.equal(toggleButton(a).getAttribute('aria-checked'),'true');assert.equal(toggleButton(off).getAttribute('aria-checked'),'false');
    assert.equal(toggleButton(a).children[1].textContent,language==='en'?'Enabled':'已启用');assert.equal(toggleButton(off).children[1].textContent,language==='en'?'Disabled':'已禁用');
    assert.equal(toggleButton(a).getAttribute('aria-haspopup'),'dialog');assert.equal(toggleButton(off).getAttribute('aria-haspopup'),'false');
    assert.equal(toggleButton(a).title,language==='en'?'Disable alice@example.com':'停用 alice@example.com');assert.equal(toggleButton(off).title,language==='en'?'Enable off@example.com':'启用 off@example.com');
    assert(off.className.includes('account-disabled'));assert(descendants(off).some(n=>n.className==='account-state'&&n.textContent===(language==='en'?'Disabled':'已禁用')));
    assert(!descendants(a).some(n=>n.className==='account-state'));
    assert.equal(toggleButton(claude).getAttribute('aria-label'),language==='en'?'Account enabled status: claude@example.com':'账户启用状态：claude@example.com');
    if(language==='en')for(const node of descendants(d.elements.get('quota-cards'))){assert(!/[\u3400-\u9fff]/.test(node.textContent),node.textContent);assert(!/[\u3400-\u9fff]/.test(node.title||''),node.title);for(const v of Object.values(node.attributes))assert(!/[\u3400-\u9fff]/.test(v),v);}
  });
}

const resetDialog=d=>d.elements.get('reset-dialog');
const resetOptions=d=>descendants(d.elements.get('reset-body')).filter(n=>n.className==='reset-option');
const dialogText=d=>descendants(resetDialog(d)).map(n=>n.textContent).filter(Boolean);
const hour=3600000;
function creditList(){return {available_count:2,credits:[
  {id:'soon',title:'Full reset',expires_at:new Date(Date.now()+(2*24+3)*hour+hour/2).toISOString()},
  {id:'late',expires_at:new Date(Date.now()+30*24*hour).toISOString(),description:'Reset your current usage limits'},
]};}
function resetFetcher(calls,opts={}){
  return async(url,options)=>{calls.push({url,options});
    if(url.endsWith('reset-credits')){const list=typeof opts.list==='function'?opts.list():opts.list??creditList();return reply(list,opts.listStatus?.()??200);}
    if(url.endsWith('consume-reset'))return opts.consume?opts.consume(options):reply({code:'reset',windows_reset:2});
    if(url.endsWith('refresh-account'))return reply({refresh_finished_at:'before'},202);
    return reply({...(opts.snapshot?.()??actionAccounts()),account_refresh:{'codex-a':{refreshing:false,finished_at:'after'},'claude-a':{refreshing:false,finished_at:'after'}}});};
}
const useReset=async(d,id='codex-a')=>{await d.api.openReset(id);await d.api.confirmReset();};
const posted=(calls,suffix)=>calls.filter(c=>c.url.endsWith(suffix));

test('reset opens a dialog with the earliest-expiring credit preselected and spends nothing until confirmed',async()=>{
  const calls=[];const d=dashboard(login(),resetFetcher(calls));
  await d.api.load();assert.equal(resetDialog(d).hidden,true);
  await resetButton(panel(d.elements,'codex-a')).onclick();
  assert.equal(resetDialog(d).hidden,false);assert.equal(posted(calls,'consume-reset').length,0,'opening must not spend a credit');
  const list=posted(calls,'reset-credits');assert.equal(list.length,1);assert.equal(list[0].url,'/v0/management/plugins/cpa-mini-usage/reset-credits');assert.equal(list[0].options.method,'POST');assert.deepEqual(JSON.parse(list[0].options.body),{auth_id:'codex-a'});
  assert.equal(d.elements.get('reset-subtitle').textContent,'账户：alice@example.com · 可用 2 张');
  let options=resetOptions(d);assert.deepEqual(options.map(n=>n.dataset.creditId),['soon','late']);
  assert.deepEqual(options.map(n=>n.getAttribute('aria-checked')),['true','false']);assert.equal(options[0].getAttribute('role'),'radio');
  assert(descendants(options[0]).some(n=>n.className==='reset-badge'&&n.textContent==='最早过期'));assert(!descendants(options[1]).some(n=>n.className==='reset-badge'));
  assert.match(descendants(options[0]).find(n=>n.className==='reset-option-detail').textContent,/ 过期 · 剩 2 天 3 小时$/);
  assert(descendants(options[0]).some(n=>n.textContent==='完整重置'),'official default copy is localized');assert(descendants(options[1]).some(n=>n.textContent==='完整重置'));assert(descendants(options[1]).some(n=>n.textContent==='重置当前用量额度'));
  assert(!dialogText(d).some(text=>/Full reset|Reset your/.test(text)));
  assert.equal(d.elements.get('reset-confirm').textContent,'确认使用');assert.equal(d.elements.get('reset-confirm').disabled,false);assert.equal(d.elements.get('reset-retry').hidden,true);
  assert.equal(d.elements.get('reset-warning').textContent,'默认选中最早过期的一张。确认后立即消耗，无法撤销。');
  options[1].onclick();options=resetOptions(d);assert.deepEqual(options.map(n=>n.getAttribute('aria-checked')),['false','true']);
  assert.equal(posted(calls,'consume-reset').length,0);
  await d.elements.get('reset-confirm').onclick();
  assert.equal(resetDialog(d).hidden,true);
  const posts=posted(calls,'consume-reset');assert.equal(posts.length,1);assert.equal(posts[0].options.headers.Authorization,'Bearer key');
  const body=JSON.parse(posts[0].options.body);assert.equal(body.auth_id,'codex-a');assert.equal(body.credit_id,'late');assert.match(body.redeem_request_id,/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  assert.equal(posted(calls,'refresh-account').length,1);
  assert.equal(accountNoteNode(panel(d.elements,'codex-a')).textContent,'已使用重置卡，重置了 2 个额度窗口。');
  assert.equal(resetButton(panel(d.elements,'codex-a')).textContent,'使用重置卡');assert.equal(resetButton(panel(d.elements,'codex-a')).disabled,false);
});

test('the default choice spends the earliest-expiring credit',async()=>{
  const calls=[];const d=dashboard(login(),resetFetcher(calls));
  await d.api.load();await useReset(d);
  assert.equal(JSON.parse(posted(calls,'consume-reset')[0].options.body).credit_id,'soon');
});

test('cancel, backdrop and Escape close the dialog without spending',async()=>{
  const calls=[];const d=dashboard(login(),resetFetcher(calls));await d.api.load();
  await d.api.openReset('codex-a');d.elements.get('reset-cancel').onclick();assert.equal(resetDialog(d).hidden,true);
  await d.api.openReset('codex-a');resetDialog(d).onclick({target:d.elements.get('reset-card')});assert.equal(resetDialog(d).hidden,false,'clicks inside the card keep it open');
  resetDialog(d).onclick({target:resetDialog(d)});assert.equal(resetDialog(d).hidden,true);
  await d.api.openReset('codex-a');let prevented=false;d.events.keydown({key:'Escape',preventDefault(){prevented=true;}});assert.equal(resetDialog(d).hidden,true);assert(prevented);
  await d.api.confirmReset();assert.equal(posted(calls,'consume-reset').length,0,'a closed dialog cannot be confirmed');
});

test('missing details let OpenAI choose; failures offer a reload; zero credits cannot be confirmed',async()=>{
  let calls=[];let d=dashboard(login(),resetFetcher(calls,{list:{available_count:1,credits:[]}}));await d.api.load();
  await d.api.openReset('codex-a');let options=resetOptions(d);assert.equal(options.length,1);assert.equal(options[0].getAttribute('aria-checked'),'true');
  assert(dialogText(d).includes('由 OpenAI 自动选择'));assert(dialogText(d).includes('未返回卡片明细，将由 OpenAI 决定扣哪一张。'));
  assert.equal(d.elements.get('reset-warning').textContent,'确认后立即消耗一张重置卡，无法撤销。');
  await d.api.confirmReset();const body=JSON.parse(posted(calls,'consume-reset')[0].options.body);assert(!('credit_id' in body));

  let status=502,list={error:'access_token=secret reset credits unavailable'};calls=[];
  d=dashboard(login(),resetFetcher(calls,{list:()=>list,listStatus:()=>status}));await d.api.load();
  await d.api.openReset('codex-a');assert(dialogText(d).includes('同步失败，可手动重试。'));assert(!dialogText(d).some(text=>/secret/.test(text)));
  assert.equal(d.elements.get('reset-confirm').disabled,true);assert.equal(d.elements.get('reset-retry').hidden,false);
  list={error:'reset credits invalid'};await d.elements.get('reset-retry').onclick();assert(dialogText(d).includes('重置卡明细格式异常。'));
  status=200;list=creditList();await d.elements.get('reset-retry').onclick();
  assert.equal(resetOptions(d).length,2);assert.equal(d.elements.get('reset-confirm').disabled,false);assert.equal(d.elements.get('reset-retry').hidden,true);

  for(const [bad,want] of [[{available_count:0,credits:[]},'没有可用的重置卡。'],[{available_count:'2',credits:[]},'重置卡明细格式异常。'],[{available_count:1,credits:[{id:''}]},'重置卡明细格式异常。']]){
    calls=[];d=dashboard(login(),resetFetcher(calls,{list:bad}));await d.api.load();await d.api.openReset('codex-a');
    assert(dialogText(d).includes(want),want);assert.equal(d.elements.get('reset-confirm').disabled,true);
    await d.api.confirmReset();assert.equal(posted(calls,'consume-reset').length,0);
  }
});

test('reset outcomes are explained; ambiguous failures retry the same request and credit, rejections do not',async()=>{
  for(const [code,zh] of [['nothing_to_reset','当前没有需要重置的额度窗口。'],['no_credit','这张重置卡已不可用，请重新选择。'],['already_redeemed','该重置请求已处理，未重复扣除。']]){
    const d=dashboard(login(),resetFetcher([],{consume:()=>reply({code,windows_reset:0})}));
    await d.api.load();await useReset(d);
    assert.equal(accountNoteNode(panel(d.elements,'codex-a')).textContent,zh);
  }
  const d0=dashboard(login(),resetFetcher([],{list:{available_count:1,credits:[]},consume:()=>reply({code:'no_credit',windows_reset:0})}));
  await d0.api.load();await useReset(d0);assert.equal(accountNoteNode(panel(d0.elements,'codex-a')).textContent,'没有可用的重置卡。');
  const sent=[],calls=[];let mode=503;
  const d=dashboard(login(),resetFetcher(calls,{consume:options=>{sent.push(JSON.parse(options.body));return reply({error:mode===503?'reset endpoint unavailable':'reset_busy'},mode);}}));
  await d.api.load();
  await d.api.openReset('codex-a');resetOptions(d)[1].onclick();await d.api.confirmReset();
  assert.equal(accountError(panel(d.elements,'codex-a')).textContent,'重置接口不可用，请稍后重试。');
  await d.api.openReset('codex-a');
  assert.equal(posted(calls,'reset-credits').length,1,'an unresolved attempt is retried, not re-chosen');
  assert(dialogText(d).includes('上次请求结果未知。确认将用同一请求重试，不会重复扣除。'));assert.equal(resetOptions(d).length,0);
  assert.equal(d.elements.get('reset-confirm').textContent,'重试同一请求');assert.equal(d.elements.get('reset-confirm').disabled,false);
  await d.api.confirmReset();
  assert.equal(sent.length,2);assert.equal(sent[1].redeem_request_id,sent[0].redeem_request_id);assert.equal(sent[0].credit_id,'late');assert.equal(sent[1].credit_id,'late');
  mode=409;await useReset(d);assert.equal(accountError(panel(d.elements,'codex-a')).textContent,'重置卡请求正在处理中。');
  assert.equal(sent[2].redeem_request_id,sent[0].redeem_request_id);
  await useReset(d);
  assert.equal(sent.length,4);assert.equal(sent[3].redeem_request_id,sent[0].redeem_request_id,'busy after an unknown result is not a verdict; the same request stays retryable');
  assert.equal(sent[3].credit_id,'late');assert.equal(posted(calls,'reset-credits').length,1);
  mode=404;await useReset(d);assert.equal(sent[4].redeem_request_id,sent[0].redeem_request_id);
  mode=409;await useReset(d);
  assert.equal(sent.length,6);assert.notEqual(sent[5].redeem_request_id,sent[4].redeem_request_id,'a definitive rejection starts a new request id');
  assert.equal(sent[5].credit_id,'soon','a fresh request goes back to the earliest-expiring default');
  await useReset(d);assert.notEqual(sent[6].redeem_request_id,sent[5].redeem_request_id,'a first-attempt 4xx (even reset_busy) is definitive');
  assert(sent.every(body=>!('confirmation_token' in body)),'Codex sends no confirmation token');
});

test('reset errors never echo backend text',async()=>{
  const d=dashboard(login(),resetFetcher([],{consume:()=>reply({error:'access_token=secret'},502)}));
  await d.api.load();await useReset(d);
  assert(!/secret/.test(accountError(panel(d.elements,'codex-a')).textContent));assert(accountError(panel(d.elements,'codex-a')).textContent);
});

const CLAUDE_TOKEN='claude-confirm-token-0123456789';
function claudeCredits(){return {available_count:7,confirmation_token:CLAUDE_TOKEN,credits:[
  {id:'c-full',title:'Full reset',description:'Refills your 5-hour and weekly limits.',expires_at:new Date(Date.now()+(2*24+3)*hour+hour/2).toISOString(),remaining:3,usable_now:true},
  {id:'c-fable',title:'Fable reset',description:'Refills your Fable allowance and gives back what Fable used on your weekly limit.',expires_at:new Date(Date.now()+5*24*hour).toISOString(),remaining:1,usable_now:false,unavailable_reason:'needs_limit'},
  {id:'c-5h',title:'5-hour reset',description:'Refills your 5-hour limit.',expires_at:new Date(Date.now()+10*24*hour).toISOString(),remaining:2,usable_now:true},
  {id:'c-week',title:'Weekly reset',description:'Refills your weekly limit.',expires_at:new Date(Date.now()+20*24*hour).toISOString(),usable_now:false,unavailable_reason:'access_token=secret blocked'},
]};}
const badges=option=>descendants(option).filter(n=>/^reset-badge/.test(n.className)).map(n=>n.textContent);

test('Claude: the card button opens a dialog showing every credit batch with type, expiry, count and availability',async()=>{
  const calls=[];const d=dashboard(login(),resetFetcher(calls,{list:claudeCredits()}));await d.api.load();
  await resetButton(panel(d.elements,'claude-a')).onclick();
  assert.equal(resetDialog(d).hidden,false);assert.equal(posted(calls,'consume-reset').length,0);
  assert.deepEqual(JSON.parse(posted(calls,'reset-credits')[0].options.body),{auth_id:'claude-a'});
  assert.equal(d.elements.get('reset-subtitle').textContent,'账户：claude@example.com · 可用 7 张');
  const options=resetOptions(d);assert.deepEqual(options.map(n=>n.dataset.creditId),['c-full','c-fable','c-5h','c-week']);
  assert.deepEqual(options.map(n=>n.getAttribute('aria-checked')),['true','false','false','false']);
  assert.deepEqual(options.map(n=>descendants(n).find(c=>c.className==='reset-option-title').children[0].textContent),['完整重置','Fable 重置','5 小时重置','周额度重置']);
  assert.deepEqual(options.map(n=>descendants(n).find(c=>c.className==='reset-option-desc').textContent),['重置当前 5 小时和周额度。','重置 Fable 额度并返还 Fable 本周消耗的周额度。','重置当前 5 小时额度。','重置当前周额度。']);
  assert.match(descendants(options[0]).find(n=>n.className==='reset-option-detail').textContent,/ 过期 · 剩 2 天 3 小时$/);
  for(const option of options)assert.match(descendants(option).find(n=>n.className==='reset-option-detail').textContent,/ 过期 · 剩 \d+ 天/);
  assert.deepEqual(options.map(badges),[['最早过期','剩 3 次'],['暂不可用'],['剩 2 次'],['暂不可用']]);
  for(const i of [1,3]){assert.equal(options[i].disabled,true);assert.equal(options[i].getAttribute('aria-disabled'),'true');assert.equal(options[i].tabIndex,-1);}
  for(const i of [0,2]){assert.notEqual(options[i].disabled,true);assert.equal(options[i].getAttribute('aria-disabled'),null);}
  assert.equal(descendants(options[1]).find(n=>n.className==='reset-option-reason').textContent,'需达到额度上限后使用');
  assert(!descendants(options[3]).some(n=>n.className==='reset-option-reason'),'unknown reasons fall back to the generic badge only');
  assert(!dialogText(d).some(text=>/secret|access_token|blocked|Refills|Full reset/.test(text)),'backend reason/copy is never echoed');
  assert(!dialogText(d).includes('由 OpenAI 自动选择'));
  assert.equal(d.elements.get('reset-confirm').disabled,false);assert.equal(d.elements.get('reset-warning').textContent,'默认选中可用卡中最早过期的一张。确认后立即消耗，无法撤销。');
  options[1].onclick();assert.equal(resetOptions(d)[0].getAttribute('aria-checked'),'true','unavailable credits cannot be selected');
  const key=(name,target)=>{let prevented=false;d.events.keydown({key:name,target,preventDefault(){prevented=true;}});return prevented;};
  assert(key('ArrowDown',resetOptions(d)[0]));assert.deepEqual(resetOptions(d).map(n=>n.getAttribute('aria-checked')),['false','false','true','false'],'arrow keys skip unavailable credits');
  key('ArrowDown',resetOptions(d)[2]);assert.equal(resetOptions(d)[0].getAttribute('aria-checked'),'true');
  key('ArrowUp',resetOptions(d)[0]);assert.equal(resetOptions(d)[2].getAttribute('aria-checked'),'true');
  await d.elements.get('reset-confirm').onclick();assert.equal(resetDialog(d).hidden,true);
  const body=JSON.parse(posted(calls,'consume-reset')[0].options.body);assert.equal(body.auth_id,'claude-a');assert.equal(body.credit_id,'c-5h');
  assert.equal(accountNoteNode(panel(d.elements,'claude-a')).textContent,'已使用重置卡，重置了 2 个额度窗口。');
  assert.equal(posted(calls,'refresh-account').length,1);
});

test('Claude: the earliest usable credit is the default and unavailable-first lists still confirm the usable one',async()=>{
  const list=claudeCredits();list.credits=[list.credits[1],list.credits[0],list.credits[2]];
  const calls=[];const d=dashboard(login(),resetFetcher(calls,{list}));await d.api.load();await useReset(d,'claude-a');
  assert.equal(JSON.parse(posted(calls,'consume-reset')[0].options.body).credit_id,'c-full');
});

test('Claude: empty or fully unavailable credit lists cannot be confirmed and never fall back to OpenAI',async()=>{
  let calls=[];let d=dashboard(login(),resetFetcher(calls,{list:{available_count:2,credits:[]}}));await d.api.load();await d.api.openReset('claude-a');
  assert(dialogText(d).includes('没有可用的重置卡。'));assert(!dialogText(d).some(text=>/OpenAI/.test(text)));assert.equal(resetOptions(d).length,0);
  assert.equal(d.elements.get('reset-confirm').disabled,true);await d.api.confirmReset();assert.equal(posted(calls,'consume-reset').length,0);
  const all=claudeCredits();all.credits=all.credits.map(c=>({...c,usable_now:false,unavailable_reason:'cooldown'}));
  calls=[];d=dashboard(login(),resetFetcher(calls,{list:all}));await d.api.load();await d.api.openReset('claude-a');
  const options=resetOptions(d);assert.equal(options.length,4);assert(options.every(n=>n.disabled===true&&n.getAttribute('aria-checked')==='false'&&n.tabIndex===-1));
  assert(options.every(n=>descendants(n).find(c=>c.className==='reset-option-detail').textContent.includes('过期')),'expiry times stay visible');
  assert(options.every(n=>descendants(n).find(c=>c.className==='reset-option-reason').textContent==='冷却中，请稍后再试'));
  assert.equal(d.elements.get('reset-confirm').disabled,true);assert(!dialogText(d).some(text=>/OpenAI/.test(text)));
  assert.equal(d.elements.get('reset-warning').textContent,'当前没有立即可用的重置卡，可查看以上过期时间并在满足条件后使用。');
  d.events.keydown({key:'ArrowDown',target:options[0],preventDefault(){}});assert(resetOptions(d).every(n=>n.getAttribute('aria-checked')==='false'));
  await d.api.confirmReset();assert.equal(posted(calls,'consume-reset').length,0);
});

test('Claude: malformed optional credit fields are rejected',async()=>{
  for(const extra of [{remaining:0},{remaining:1.5},{remaining:'2'},{usable_now:'yes'},{usable_now:null},{unavailable_reason:5}]){
    const calls=[];const d=dashboard(login(),resetFetcher(calls,{list:{available_count:1,credits:[{id:'c',title:'Full reset',...extra}]}}));await d.api.load();await d.api.openReset('claude-a');
    assert(dialogText(d).includes('重置卡明细格式异常。'),JSON.stringify(extra));assert.equal(d.elements.get('reset-confirm').disabled,true);
  }
});

test('Claude: reset outcomes and new error codes are explained without echoing backend text',async()=>{
  for(const [result,zh] of [[{code:'cooldown',windows_reset:0},'重置卡处于冷却中，请稍后再试。'],[{code:'not_next_grant',windows_reset:0},'请先使用最早过期的同类型重置卡。'],[{code:'reset',windows_reset:0},'已使用重置卡，正在刷新额度。'],[{code:'reset',windows_reset:1},'已使用重置卡，重置了 1 个额度窗口。']]){
    const d=dashboard(login(),resetFetcher([],{list:claudeCredits(),consume:()=>reply(result)}));await d.api.load();await useReset(d,'claude-a');
    assert.equal(accountNoteNode(panel(d.elements,'claude-a')).textContent,zh);
  }
  for(const [error,status,zh] of [['reset_rate_limited',429,'重置请求过于频繁，请稍后重试。'],['reset_auth_failed',422,'该账户 OAuth 授权已失效或权限不足，请在 CPA 中重新登录或刷新后重试。'],['reset_account_changed',409,'该账户凭证已变更，请重新打开重置弹窗核对后再试。'],['reset_unconfirmed',502,'上次重置结果未能确认，请再次点击“使用重置卡”用同一请求安全重试。'],['reset organization unavailable',502,'无法读取账户所属组织信息，请稍后重试。'],['claude_reset_cli_version',422,'客户端版本过旧，暂不支持读取 Claude 重置卡。'],['claude_reset_surface',422,'当前请求身份未获 Claude 重置卡接口认可。'],['claude_reset_not_eligible',422,'当前账户不符合 Claude 重置卡使用条件。']]){
    const d=dashboard(login(),resetFetcher([],{list:claudeCredits(),consume:()=>reply({error},status)}));await d.api.load();await useReset(d,'claude-a');
    assert.equal(accountError(panel(d.elements,'claude-a')).textContent,zh,error);
  }
  const d=dashboard(login(),resetFetcher([],{list:claudeCredits(),consume:()=>reply({error:'access_token=secret'},422)}));await d.api.load();await useReset(d,'claude-a');
  assert(!/secret/.test(accountError(panel(d.elements,'claude-a')).textContent));
});

// consume-reset scripted step by step: 'lost' drops the response (network failure, no status).
function scriptedConsume(sent,steps){return options=>{sent.push(JSON.parse(options.body));const [status,error]=steps.shift()??[200];
  if(status==='lost')throw new TypeError('network connection lost');return status===200?reply({code:'reset',windows_reset:error??0}):reply({error},status);};}
const sameRequest=(sent,first)=>sent.every(body=>body.redeem_request_id===first.redeem_request_id&&body.credit_id===first.credit_id&&body.confirmation_token===first.confirmation_token);

test('Claude: the confirmation token is validated and sent with every attempt',async()=>{
  const sent=[],calls=[];const d=dashboard(login(),resetFetcher(calls,{list:claudeCredits(),consume:scriptedConsume(sent,[[502,'reset_unconfirmed'],[200]])}));
  await d.api.load();await useReset(d,'claude-a');await useReset(d,'claude-a');
  assert.equal(sent.length,2);assert.equal(sent[0].confirmation_token,CLAUDE_TOKEN);assert(sameRequest(sent,sent[0]),'the retry reuses the token');
  for(const token of [undefined,null,'',' short-token     ',42]){
    const list=claudeCredits();if(token===undefined)delete list.confirmation_token;else list.confirmation_token=token;
    const c=[];const b=dashboard(login(),resetFetcher(c,{list}));await b.api.load();await b.api.openReset('claude-a');
    assert(dialogText(b).includes('重置卡明细格式异常。'),String(token));assert.equal(b.elements.get('reset-confirm').disabled,true);
    await b.api.confirmReset();assert.equal(posted(c,'consume-reset').length,0,'usable Claude credits without a valid token fail closed');
  }
  const blocked=claudeCredits();delete blocked.confirmation_token;blocked.credits=blocked.credits.map(c=>({...c,usable_now:false}));
  const b=dashboard(login(),resetFetcher([],{list:blocked}));await b.api.load();await b.api.openReset('claude-a');
  assert.equal(resetOptions(b).length,4,'a token is only required when something can be spent');
  for(const [error,status,zh] of [['invalid_confirmation_token',400,'重置确认凭据无效，请重新打开弹窗后再试。'],['account_disabled',409,'账户已禁用，无法使用重置卡。']]){
    const e=dashboard(login(),resetFetcher([],{list:claudeCredits(),consume:()=>reply({error},status)}));await e.api.load();await useReset(e,'claude-a');
    assert.equal(accountError(panel(e.elements,'claude-a')).textContent,zh,error);
  }
});

test('Claude: a lost response stays retryable through busy and rate-limit replies until it succeeds',async()=>{
  const sent=[],calls=[];
  const d=dashboard(login(),resetFetcher(calls,{list:claudeCredits(),consume:scriptedConsume(sent,[['lost'],[409,'reset_busy'],[429,'reset_rate_limited'],[422,'reset_auth_failed'],[200]])}));
  await d.api.load();await d.api.openReset('claude-a');resetOptions(d)[2].onclick();await d.api.confirmReset();
  assert.equal(accountError(panel(d.elements,'claude-a')).textContent,'重置卡使用失败，请稍后重试。');
  for(const zh of ['重置卡请求正在处理中。','重置请求过于频繁，请稍后重试。','该账户 OAuth 授权已失效或权限不足，请在 CPA 中重新登录或刷新后重试。']){
    await d.api.openReset('claude-a');assert(dialogText(d).includes('上次请求结果未知。确认将用同一请求重试，不会重复扣除。'));assert.equal(d.elements.get('reset-confirm').textContent,'重试同一请求');
    await d.api.confirmReset();assert.equal(accountError(panel(d.elements,'claude-a')).textContent,zh);
    assert(panel(d.elements,'claude-a'),'a 4xx never clears the CPA session');assert.equal(d.api.guessKey(),'key');
  }
  await useReset(d,'claude-a');
  assert.equal(sent.length,5);assert(sameRequest(sent,sent[0]),'same request id, credit and token throughout');
  assert.equal(sent[0].credit_id,'c-5h');assert.equal(sent[0].confirmation_token,CLAUDE_TOKEN);assert.equal(posted(calls,'reset-credits').length,1);
  assert.equal(accountNoteNode(panel(d.elements,'claude-a')).textContent,'已使用重置卡，正在刷新额度。');
  await d.api.openReset('claude-a');assert.equal(posted(calls,'reset-credits').length,2,'success clears the pending request');
});

test('Claude: first-attempt 4xx and definitive retry failures clear the pending request without logging out',async()=>{
  for(const [steps,label] of [[[[429,'reset_rate_limited']],'first 429'],[[[422,'claude_reset_not_eligible']],'first 422'],[[[409,'reset_busy']],'first 409 busy'],
    [[[502,'reset_unconfirmed'],[400,'invalid_confirmation_token']],'retry 400'],[[[503,'reset endpoint unavailable'],[404,'account_not_found']],'retry 404'],
    [[['lost'],[409,'reset_account_changed']],'retry 409 changed'],[[[502,'reset_unconfirmed'],[409,'account_disabled']],'retry 409 disabled']]){
    const sent=[],calls=[];const d=dashboard(login(),resetFetcher(calls,{list:claudeCredits(),consume:scriptedConsume(sent,[...steps])}));
    await d.api.load();for(let i=0;i<steps.length;i++)await useReset(d,'claude-a');
    assert(sameRequest(sent,sent[0]),label);assert(panel(d.elements,'claude-a'),label);assert.equal(d.api.guessKey(),'key',label);
    const lists=posted(calls,'reset-credits').length;await useReset(d,'claude-a');
    assert.equal(posted(calls,'reset-credits').length,lists+1,label+' clears the pending request');
    assert.notEqual(sent.at(-1).redeem_request_id,sent[0].redeem_request_id,label+' starts a new request id');assert.equal(sent.at(-1).credit_id,'c-full');
  }
});

test('an unresolved reset keeps its retry entry when the balance later reads 0, missing or diagnostic',async()=>{
  for(const change of ['zero','missing','diagnostic']){
    const sent=[],calls=[];let after=false;
    const snapshot=()=>{const s=actionAccounts(),q=s.quotas.find(q=>q.auth_id==='claude-a');if(after){if(change==='zero')q.manual_reset_remaining=0;else delete q.manual_reset_remaining;if(change==='diagnostic')q.manual_reset_diagnostic='request_failed';}return s;};
    const d=dashboard(login(),resetFetcher(calls,{list:claudeCredits(),snapshot,consume:scriptedConsume(sent,[[502,'reset_unconfirmed'],[200,1]])}));
    await d.api.load();await useReset(d,'claude-a');
    after=true;await d.api.load();
    const card=panel(d.elements,'claude-a');assert(resetButton(card),change+': retry entry kept');assert.equal(resetButton(card).disabled,false);
    if(change==='diagnostic')assert(descendants(card).some(n=>n.className==='manual-reset-diagnostic'));
    await resetButton(card).onclick();assert(dialogText(d).includes('上次请求结果未知。确认将用同一请求重试，不会重复扣除。'),change);
    await d.elements.get('reset-confirm').onclick();
    assert.equal(sent.length,2,change);assert(sameRequest(sent,sent[0]),change+': original request, credit and token');assert.equal(posted(calls,'reset-credits').length,1);
    assert.equal(accountNoteNode(panel(d.elements,'claude-a')).textContent,'已使用重置卡，重置了 1 个额度窗口。');
    assert(!resetButton(panel(d.elements,'claude-a')),change+': no new redemption without a positive balance');
    await d.api.openReset('claude-a');assert.equal(resetDialog(d).hidden,true);assert.equal(posted(calls,'reset-credits').length,1);
  }
  // A disabled or replaced account never keeps the retry entry.
  const sent=[];let disabled=false;const snapshot=()=>{const s=actionAccounts();s.quotas.find(q=>q.auth_id==='claude-a').disabled=disabled;return s;};
  const d=dashboard(login(),resetFetcher([],{list:claudeCredits(),snapshot,consume:scriptedConsume(sent,[[502,'reset_unconfirmed']])}));
  await d.api.load();await useReset(d,'claude-a');disabled=true;await d.api.load();
  assert(!resetButton(panel(d.elements,'claude-a')));await d.api.openReset('claude-a');assert.equal(resetDialog(d).hidden,true);
});

test('Claude confirmation copy names usable credits; Codex keeps its copy',async()=>{
  for(const language of ['zh','en']){
    const d=dashboard(login(),resetFetcher([],{list:claudeCredits()}),{language});await d.api.load();await d.api.openReset('claude-a');
    assert.equal(d.elements.get('reset-warning').textContent,language==='en'?'The earliest-expiring usable reset is selected by default. Confirming spends it immediately; this cannot be undone.':'默认选中可用卡中最早过期的一张。确认后立即消耗，无法撤销。');
    d.elements.get('reset-cancel').onclick();await d.api.openReset('codex-a');
    assert.equal(d.elements.get('reset-warning').textContent,language==='en'?'The credit expiring first is selected by default. Confirming spends it immediately; this cannot be undone.':'默认选中最早过期的一张。确认后立即消耗，无法撤销。');
  }
});

test('en: the Claude reset dialog is fully translated',async()=>{
  const all=claudeCredits();all.credits[3].unavailable_reason='blocked_seven_day';
  const d=dashboard(login(),resetFetcher([],{list:all}),{language:'en'});await d.api.load();await d.api.openReset('claude-a');
  const options=resetOptions(d);
  assert.deepEqual(options.map(n=>descendants(n).find(c=>c.className==='reset-option-title').children[0].textContent),['Full reset','Fable reset','5-hour reset','Weekly reset']);
  assert.deepEqual(options.map(n=>descendants(n).find(c=>c.className==='reset-option-desc').textContent),['Refills your 5-hour and weekly limits.','Refills your Fable allowance and gives back what Fable used on your weekly limit.','Refills your 5-hour limit.','Refills your weekly limit.']);
  assert.deepEqual(options.map(badges),[['Expires first','3 left'],['Unavailable right now'],['2 left'],['Unavailable right now']]);
  assert.deepEqual([1,3].map(i=>descendants(options[i]).find(n=>n.className==='reset-option-reason').textContent),['Available once a limit is reached','Blocked by the weekly limit']);
  for(const node of descendants(resetDialog(d))){assert(!/[\u3400-\u9fff]/.test(node.textContent),node.textContent);for(const [name,v] of Object.entries(node.attributes))if(name!=='data-i18n')assert(!/[\u3400-\u9fff]/.test(v),v);}
  d.elements.get('reset-cancel').onclick();
  const blocked=claudeCredits();blocked.credits=blocked.credits.map(c=>({...c,usable_now:false}));
  const b=dashboard(login(),resetFetcher([],{list:blocked}),{language:'en'});await b.api.load();await b.api.openReset('claude-a');
  assert.equal(b.elements.get('reset-warning').textContent,'No reset credit can be used right now. Review the expiry times above and use one once it becomes available.');
});

test('en: the reset dialog is fully translated',async()=>{
  const d=dashboard(login(),resetFetcher([]),{language:'en'});await d.api.load();await d.api.openReset('codex-a');
  assert.equal(d.elements.get('reset-title').textContent,'Use reset');assert.equal(d.elements.get('reset-subtitle').textContent,'Account: alice@example.com · 2 available');
  const options=resetOptions(d);assert.match(descendants(options[0]).find(n=>n.className==='reset-option-detail').textContent,/^Expires .+ · 2d 3h left$/);
  assert(descendants(options[0]).some(n=>n.textContent==='Expires first'));assert(descendants(options[1]).some(n=>n.textContent==='Full reset'));
  assert.equal(d.elements.get('reset-confirm').textContent,'Use this reset');assert.equal(d.elements.get('reset-cancel').textContent,'Cancel');
  for(const node of descendants(resetDialog(d))){assert(!/[\u3400-\u9fff]/.test(node.textContent),node.textContent);for(const [name,v] of Object.entries(node.attributes))if(name!=='data-i18n')assert(!/[\u3400-\u9fff]/.test(v),v);}
});

test('disable requires confirmation, enable is direct, and state follows the snapshot',async()=>{
  const calls=[];let disabled=false;
  const snapshot=()=>{const s=actionAccounts();s.quotas=s.quotas.map(q=>q.auth_id==='codex-a'?{...q,disabled}:q);return s;};
  const d=dashboard(login(),async(url,options)=>{calls.push({url,options});if(url.endsWith('/auth-files/status')){disabled=JSON.parse(options.body).disabled;return reply({status:'ok',disabled});}return reply(snapshot());});
  await d.api.load();
  await toggleButton(panel(d.elements,'codex-a')).onclick();
  assert.equal(posted(calls,'/auth-files/status').length,0);
  await d.api.confirmDisable();
  const patch=calls.filter(c=>c.url.endsWith('/auth-files/status'));assert.equal(patch.length,1);
  assert.equal(patch[0].url,'/v0/management/auth-files/status');assert.equal(patch[0].options.method,'PATCH');assert.equal(patch[0].options.headers.Authorization,'Bearer key');assert.equal(patch[0].options.headers['Content-Type'],'application/json');
  assert.deepEqual(JSON.parse(patch[0].options.body),{name:'codex-a',disabled:true});
  assert(calls.at(-1).url.endsWith('/data'),'status change is followed by a snapshot');
  let card=panel(d.elements,'codex-a');assert(card.className.includes('account-disabled'));assert(!resetButton(card));assert.equal(toggleButton(card).children[1].textContent,'已禁用');assert.equal(toggleButton(card).getAttribute('aria-checked'),'false');
  assert.equal(accountNoteNode(card).textContent,'账户已禁用，CPA 不会再调度它。');
  await toggleButton(card).onclick();
  assert.deepEqual(JSON.parse(calls.filter(c=>c.url.endsWith('/auth-files/status'))[1].options.body),{name:'codex-a',disabled:false});
  card=panel(d.elements,'codex-a');assert(!card.className.includes('account-disabled'));assert(resetButton(card));assert.equal(toggleButton(card).children[1].textContent,'已启用');assert.equal(toggleButton(card).getAttribute('aria-checked'),'true');assert.equal(accountNoteNode(card).textContent,'账户已启用。');
});

test('status API failures are translated without leaking text, and auth failures return to sign-in',async()=>{
  for(const [status,error,want] of [[404,'auth file not found','未找到账户。'],[500,'failed to update auth: token=secret','同步失败，可手动重试。']]){
    const d=dashboard(login(),async url=>url.endsWith('/auth-files/status')?reply({error},status):reply(actionAccounts()));
    await d.api.load();await d.api.toggleAccount('codex-off',false);
    const card=panel(d.elements,'codex-off');assert.equal(accountError(card).textContent,want);assert(card.className.includes('account-disabled'));assert(!/secret/.test(accountError(card).textContent));
  }
  const d=dashboard(login(),async url=>url.endsWith('/auth-files/status')?reply({},401):reply(actionAccounts()));
  await d.api.load();await d.api.toggleAccount('codex-off',false);
  assert(!panel(d.elements,'codex-off'));assert(d.elements.get('dashboard').classes.has('needs-login'));
});

test('an in-flight action locks only its own card',async()=>{
  let release;const d=dashboard(login(),async url=>url.endsWith('reset-credits')?reply(creditList()):url.endsWith('consume-reset')?new Promise(r=>release=()=>r(reply({code:'nothing_to_reset'}))):url.endsWith('refresh-account')?reply({error:'account_not_found'},404):reply(actionAccounts()));
  await d.api.load();await d.api.openReset('codex-a');const pending=d.api.confirmReset();
  const a=panel(d.elements,'codex-a'),zero=panel(d.elements,'codex-zero');
  assert.equal(resetButton(a).disabled,true);assert.equal(resetButton(a).textContent,'正在使用…');assert.equal(toggleButton(a).disabled,true);assert.equal(refreshButton(a).disabled,true);
  assert.equal(toggleButton(zero).disabled,false);assert.equal(refreshButton(zero).disabled,false);
  release();await pending;assert.equal(toggleButton(panel(d.elements,'codex-a')).disabled,false);
});

test('the status button shows the pending action and recovers after failure',async()=>{
  let release;const d=dashboard(login(),async url=>url.endsWith('/auth-files/status')?new Promise(r=>release=()=>r(reply({error:'auth file not found'},404))):reply(actionAccounts()));
  await d.api.load();await toggleButton(panel(d.elements,'codex-a')).onclick();const pending=d.api.confirmDisable();
  let sw=toggleButton(panel(d.elements,'codex-a'));assert.equal(sw.children[1].textContent,'正在禁用…');assert.equal(sw.getAttribute('aria-busy'),'true');assert.equal(sw.disabled,true);assert.equal(sw.title,'正在禁用…');
  assert.equal(toggleButton(panel(d.elements,'codex-zero')).disabled,false);
  release();await pending;sw=toggleButton(panel(d.elements,'codex-a'));
  assert.equal(sw.children[1].textContent,'已启用');assert.equal(sw.getAttribute('aria-checked'),'true');assert.equal(sw.getAttribute('aria-busy'),'false');assert.equal(sw.disabled,false);assert.equal(accountError(panel(d.elements,'codex-a')).textContent,'未找到账户。');
});

for(const language of ['zh','en'])test(`${language}: disable confirmation defaults to cancel; cancel/backdrop/Escape make no requests`,async()=>{
  const calls=[],d=dashboard(login(),resetFetcher(calls),{language});await d.api.load();calls.length=0;
  const button=toggleButton(panel(d.elements,'codex-a'));button.focus();
  for(const method of ['cancel','backdrop','Escape']){
    await button.onclick();assert.equal(d.elements.get('disable-dialog').hidden,false);assert.equal(button.getAttribute('aria-checked'),'true');
    assert.equal(d.document.activeElement.id,'disable-cancel');assert.equal(d.elements.get('dashboard').inert,true);
    assert.equal(d.elements.get('disable-subtitle').textContent,language==='en'?'Account: alice@example.com':'账户：alice@example.com');
    assert.match(d.elements.get('disable-warning').textContent,language==='en'?/no longer route requests/:/不再调度/);
    assert.equal(calls.length,0,'opening the disable dialog performs no network I/O');
    await d.api.toggleAccount('codex-zero',true);await d.api.openReset('codex-a');assert.equal(resetDialog(d).hidden,true,'only one modal can be active');
    if(method==='cancel')d.elements.get('disable-cancel').onclick();
    else if(method==='backdrop'){
      d.elements.get('disable-dialog').onclick({target:d.elements.get('disable-card')});assert.equal(d.elements.get('disable-dialog').hidden,false);
      d.elements.get('disable-dialog').onclick({target:d.elements.get('disable-dialog')});
    }else d.events.keydown({key:'Escape',preventDefault(){}});
    assert.equal(d.elements.get('disable-dialog').hidden,true);assert.equal(d.elements.get('dashboard').inert,false);assert.equal(d.document.activeElement,button);assert.equal(button.getAttribute('aria-checked'),'true');
    await d.api.confirmDisable();assert.equal(calls.length,0);
  }
});

test('refresh is isolated from status/credits, and refresh locks out programmatic writes on its own card',async()=>{
  const calls=[];let release;const d=dashboard(login(),async(url,options)=>{calls.push({url,options});if(url.endsWith('refresh-account'))return new Promise(resolve=>release=()=>resolve(reply({error:'account_not_found'},404)));return reply(actionAccounts());});
  await d.api.load();calls.length=0;const card=panel(d.elements,'codex-a'),pending=refreshButton(card).onclick();
  assert.equal(toggleButton(card).disabled,true);assert.equal(resetButton(card).disabled,true);
  await d.api.toggleAccount('codex-a',true);await d.api.openReset('codex-a');await d.api.confirmReset();await d.api.confirmDisable();
  assert.deepEqual(calls.map(c=>c.url.split('/').pop()),['refresh-account']);release();await pending;
  assert.equal(toggleButton(panel(d.elements,'codex-a')).disabled,false);
});

test('confirmed writes submit once and block refresh/reset/status races until their reply',async()=>{
  for(const kind of ['disable','reset']){
    const calls=[];let release;const route=kind==='disable'?'status':'consume-reset';
    const d=dashboard(login(),async(url,options)=>{calls.push({url,options});if(url.endsWith('/'+route))return new Promise(resolve=>release=()=>resolve(reply({error:'account_not_found'},404)));if(url.endsWith('reset-credits'))return reply(creditList());return reply(actionAccounts());});
    await d.api.load();if(kind==='disable')await d.api.toggleAccount('codex-a',true);else await d.api.openReset('codex-a');
    const confirm=kind==='disable'?d.api.confirmDisable:d.api.confirmReset,pending=confirm();
    await confirm();await d.api.refreshAccount('codex-a');await d.api.toggleAccount('codex-a',true);await d.api.openReset('codex-a');await d.api.load();
    assert.equal(posted(calls,'/'+route).length,1);assert.equal(posted(calls,'refresh-account').length,0);assert.equal(posted(calls,'data').length,1);
    release();await pending;
  }
});

test('confirmations reject removed, renamed, replaced, duplicate and disabled account snapshots',async()=>{
  for(const kind of ['disable','reset'])for(const change of ['removed','name','provider','duplicate','disabled','refreshing']){
    const calls=[],d=dashboard(login(),resetFetcher(calls));await d.api.load();
    if(kind==='disable')await d.api.toggleAccount('codex-a',true);else await d.api.openReset('codex-a');
    const sample=actionAccounts();
    if(change==='removed')sample.quotas=sample.quotas.filter(q=>q.auth_id!=='codex-a');
    if(change==='duplicate')sample.quotas.push({...sample.quotas[0]});
    if(change==='name')sample.quotas[0].name='replacement@example.test';
    if(change==='provider')sample.quotas[0].provider='claude';
    if(change==='disabled')sample.quotas[0].disabled=true;
    if(change==='refreshing')sample.account_refresh={'codex-a':{refreshing:true}};
    d.api.acceptSnapshot(sample);await d.api.confirmDisable();await d.api.confirmReset();
    assert.equal(posted(calls,'/auth-files/status').length,0,kind+'/'+change);assert.equal(posted(calls,'consume-reset').length,0,kind+'/'+change);
  }
});

test('confirmation and late action replies cannot cross logout or credential replacement',async()=>{
  for(const kind of ['disable','reset'])for(const nextKey of ['',JSON.stringify('new-key')])for(const inFlight of [false,true]){
    const stored=login(),calls=[];let release;
    const d=dashboard(stored,async(url,options)=>{calls.push({url,options});if(url.endsWith('/status')||url.endsWith('consume-reset'))return new Promise(resolve=>release=()=>resolve(reply(url.endsWith('/status')?{disabled:true}:{code:'nothing_to_reset'})));if(url.endsWith('reset-credits'))return reply(creditList());return reply(actionAccounts());});
    await d.api.load();if(kind==='disable')await d.api.toggleAccount('codex-a',true);else await d.api.openReset('codex-a');
    const confirm=kind==='disable'?d.api.confirmDisable:d.api.confirmReset,pending=inFlight?confirm():null;
    stored.managementKey=nextKey;
    if(inFlight){release();await pending;}else await confirm();
    assert.equal(posted(calls,'/auth-files/status').length+posted(calls,'consume-reset').length,inFlight?1:0);
    assert.equal(posted(calls,'refresh-account').length,0,'late reset success must not refresh using replacement credentials');
    assert.equal(d.elements.get('disable-dialog').hidden,true);assert.equal(resetDialog(d).hidden,true);
    const card=panel(d.elements,'codex-a');if(card){assert.equal(accountNoteNode(card).textContent,'');assert(!card.className.includes('account-disabled'));}else assert(d.elements.get('dashboard').classes.has('needs-login'));
  }
});

test('a quota snapshot straddling another account status write cannot restore stale status',async()=>{
  let reads=0,release,disabled=false;
  const sample=()=>{const s=actionAccounts();s.quotas[0].disabled=disabled;s.account_refresh={'codex-zero':{refreshing:false,finished_at:'new'}};return s;};
  const d=dashboard(login(),async(url)=>{
    if(url.endsWith('refresh-account'))return reply({refresh_finished_at:'old'},202);
    if(url.endsWith('/status')){disabled=true;return reply({disabled:true});}
    if(++reads===2){const stale=sample();return new Promise(resolve=>release=()=>resolve(reply(stale)));}
    return reply(sample());
  });
  await d.api.load();const refresh=d.api.refreshAccount('codex-zero');
  for(let i=0;i<20&&!release;i++)await Promise.resolve();assert(release);
  await d.api.toggleAccount('codex-a',true);await d.api.confirmDisable();assert(panel(d.elements,'codex-a').className.includes('account-disabled'));
  release();await refresh;assert(panel(d.elements,'codex-a').className.includes('account-disabled'));assert.equal(reads,3);
});

test('modal identity labels localize on language changes and focus returns to their own trigger',async()=>{
  for(const kind of ['disable','reset']){
    const sample=actionAccounts();sample.quotas[0].name='codex-private-file.json';
    const d=dashboard(login(),async url=>reply(url.endsWith('reset-credits')?creditList():sample));await d.api.load();
    d.elements.get('refresh').focus(); // Pointer activation need not focus a button on every browser.
    if(kind==='disable')await d.api.toggleAccount('codex-a',true);else await d.api.openReset('codex-a');
    assert.match(d.elements.get(kind+'-subtitle').textContent,/账户 [0-9A-Z]{7}/);
    d.api.setLanguage('en');assert.match(d.elements.get(kind+'-subtitle').textContent,/Account [0-9A-Z]{7}/);assert(!d.elements.get(kind+'-subtitle').textContent.includes('.json'));
    d.elements.get(kind+'-cancel').onclick();assert.equal(d.document.activeElement.id,(kind==='disable'?'account-toggle-':'account-reset-')+'codex-a');
  }
});

test('zero credits invalidate an already-open reset confirmation',async()=>{
  const calls=[],d=dashboard(login(),resetFetcher(calls));await d.api.load();await d.api.openReset('codex-a');
  const sample=actionAccounts();sample.quotas[0].manual_reset_remaining=0;d.api.acceptSnapshot(sample);await d.api.confirmReset();
  assert.equal(resetDialog(d).hidden,true);assert.equal(posted(calls,'consume-reset').length,0);
});

test('late reset-credit reads are ignored after close, account change or logout',async()=>{
  for(const change of ['close','account','logout']){
    const stored=login(),calls=[];let release;
    const d=dashboard(stored,async(url,options)=>{calls.push({url,options});if(url.endsWith('reset-credits'))return new Promise(resolve=>release=()=>resolve(reply(creditList())));return reply(actionAccounts());});
    await d.api.load();const pending=d.api.openReset('codex-a');
    if(change==='close')d.elements.get('reset-cancel').onclick();
    if(change==='account'){const sample=actionAccounts();sample.quotas=[];d.api.acceptSnapshot(sample);}
    if(change==='logout')stored.managementKey='';
    release();await pending;await d.api.confirmReset();assert.equal(resetDialog(d).hidden,true);assert.equal(posted(calls,'consume-reset').length,0);
  }
});

test('custom upstream reset copy is shown as-is',async()=>{
  const list={available_count:1,credits:[{id:'promo',title:'Launch bonus reset',description:'Granted for the launch week',expires_at:new Date(Date.now()+5*hour).toISOString()}]};
  const d=dashboard(login(),resetFetcher([],{list}));await d.api.load();await d.api.openReset('codex-a');
  assert(dialogText(d).includes('Launch bonus reset'));assert(dialogText(d).includes('Granted for the launch week'));
  assert(!descendants(resetDialog(d)).some(n=>n.className==='reset-badge'),'a single credit needs no earliest badge');
});

const historySnapshot=()=>({...twoAccounts(),history_available:true});
function historyReply(params,events,total=events.length,next=''){
  const cursor=params.get('cursor')||'first-cursor';
  return {events,total,offset:cursor==='first-cursor'?0:20,cursor,limit:Number(params.get('limit')),next_cursor:next,generated_at:'2026-10-10T12:00:00Z',retention:{days:35,max_records:200000,retained_records:total,oldest_at:'2026-09-10T00:00:00Z',newest_at:'2026-10-10T11:00:00Z'}};
}
const historyRows=d=>descendants(d.elements.get('recent-table')).filter(n=>n.tag==='tr'&&n.children[0]?.tag==='td');
for(const language of ['zh','en'])test(`${language}: server history queries dates, provider and different, pages beyond 60, and refreshes GET-only`,async()=>{
  const calls=[];
  const d=dashboard(login(),async(url,options)=>{
    calls.push({url,options});if(!url.includes('/history?'))return reply(historySnapshot());
    const params=new URL(url,'https://cpa.test').searchParams;
    const older=params.get('cursor')&&params.get('cursor')!=='first-cursor';
    return reply(historyReply(params,Array.from({length:20},(_,i)=>({model:(older?'older-':'first-')+i,provider:'codex',reasoning_effort:'high',reasoning_effort_source:'host_usage'})),125,older?'':'opaque-cursor'));
  },{language});
  await d.api.load();d.api.setTab('requests');await waitForTest(()=>historyRows(d).length===20);
  assert.equal(d.elements.get('recent-page').textContent,'1 / 7');assert.match(d.elements.get('history-retention').textContent,/35.*200,000.*125/);
  assert.equal(d.elements.get('recent-table').getAttribute('aria-busy'),'false');
  assert.match(descendants(d.elements.get('recent-table')).find(n=>n.className==='request-reasoning').title,language==='en'?/not proof/:/不证明/);
  await d.elements.get('recent-next').onclick();assert.equal(d.elements.get('recent-page').textContent,'2 / 7');assert(historyRows(d).some(n=>descendants(n).some(c=>c.textContent==='older-0')));
  await d.elements.get('recent-prev').onclick();assert.equal(d.elements.get('recent-page').textContent,'1 / 7');assert.equal(new URL(calls.at(-1).url,'https://cpa.test').searchParams.get('cursor'),'first-cursor');
  d.elements.get('history-start').value='2026-09-15';d.elements.get('history-end').value='2026-09-16';d.elements.get('history-model').value='different';
  await d.elements.get('provider').onchange({target:{value:'codex'}});
  let params=new URL(calls.at(-1).url,'https://cpa.test').searchParams;
  assert.equal(params.get('provider'),'codex');assert.equal(params.get('model_status'),'different');assert(!params.has('cursor'));
  assert.equal(new Date(params.get('start')).getDate(),15);assert.equal(new Date(params.get('end')).getDate(),17);
  await d.elements.get('recent-size').onchange({target:{value:'40'}});params=new URL(calls.at(-1).url,'https://cpa.test').searchParams;assert.equal(params.get('limit'),'40');assert(!params.has('cursor'));
  await d.elements.get('refresh').onclick();assert(calls.every(c=>c.options.method==='GET'));assert.equal(d.elements.get('recent-page').textContent,'1 / 4');
  d.api.setLanguage(language==='en'?'zh':'en');assert.equal(d.elements.get('history-start').value,'2026-09-15');assert.match(d.elements.get('history-retention').textContent,language==='en'?/最多留存/:/Retained/);
});

test('history stale completions cannot overwrite changed filters or pagination; controls recover',async()=>{
  const pending=[];let delayed=false;
  const d=dashboard(login(),async(url)=>{
    if(!url.includes('/history?'))return reply(historySnapshot());
    const params=new URL(url,'https://cpa.test').searchParams;
    if(delayed)return new Promise(resolve=>pending.push({params,resolve}));
    return reply(historyReply(params,[{model:'initial'}]));
  });await d.api.load();await d.api.loadHistory();delayed=true;
  d.elements.get('history-model').value='different';const first=d.elements.get('history-model').onchange();
  const second=d.elements.get('provider').onchange({target:{value:'claude'}});
  assert.equal(d.elements.get('recent-table').getAttribute('aria-busy'),'true');assert.equal(d.elements.get('recent-next').disabled,true);
  pending[1].resolve(reply(historyReply(pending[1].params,[{model:'new-result'}])));await second;
  pending[0].resolve(reply(historyReply(pending[0].params,[{model:'private-stale'}])));await first;
  const text=descendants(d.elements.get('recent-table')).map(n=>n.textContent).join(' ');assert(text.includes('new-result'));assert(!text.includes('private-stale'));assert.equal(d.elements.get('history-retry').disabled,false);
});

test('invalid local dates block HTTP, cursor expiry is explicit, retry resets cursor, errors never masquerade as empty history',async()=>{
  let failure=0,calls=0;const requested=[];
  const d=dashboard(login(),async(url)=>{
    if(!url.includes('/history?'))return reply(historySnapshot());calls++;const params=new URL(url,'https://cpa.test').searchParams;requested.push(params);
    if(failure)return reply({error:'private upstream detail'},failure);
    return reply(historyReply(params,Array.from({length:20},()=>({model:'m'})),80,'next-token'));
  });await d.api.load();await d.api.loadHistory();
  d.elements.get('history-start').value='2026-02-30';await d.elements.get('history-start').onchange();assert.equal(calls,1);assert.match(d.elements.get('history-message').textContent,/有效日期/);assert.equal(historyRows(d).length,0);
  d.elements.get('history-start').value='2026-09-01';d.elements.get('history-end').value='2026-10-20';await d.elements.get('history-end').onchange();assert.equal(calls,1);
  d.elements.get('history-start').value='2026-10-09';d.elements.get('history-end').value='2026-10-09';await d.elements.get('history-retry').onclick();
  failure=409;await d.elements.get('recent-next').onclick();assert.match(d.elements.get('history-message').textContent,/分页已过期/);assert.equal(d.elements.get('recent-next').disabled,true);assert.equal(historyRows(d).length,0);
  failure=0;await d.elements.get('history-retry').onclick();assert(!requested.at(-1).has('cursor'));assert.equal(historyRows(d).length,20);
  failure=503;await d.elements.get('history-retry').onclick();assert.match(d.elements.get('history-message').textContent,/查询失败/);assert(!d.elements.get('history-message').textContent.includes('private'));assert.equal(d.elements.get('history-retry').disabled,false);
});

test('history credential changes discard late rows and auth failures clear private state',async()=>{
  for(const change of ['logout','replace','unauthorized']){
    const stored=login();let release,delayed=false;
    const d=dashboard(stored,async(url,options)=>{
      if(options.headers.Authorization==='Bearer new-key')return reply({quotas:[],credentials:[],recent:[]});
      if(!url.includes('/history?'))return reply(historySnapshot());
      const params=new URL(url,'https://cpa.test').searchParams;
      return delayed?new Promise(resolve=>release=()=>resolve(change==='unauthorized'?reply({},401):reply(historyReply(params,[{model:'private-stale'}])))):reply(historyReply(params,[]));
    });await d.api.load();await d.api.loadHistory();delayed=true;const pending=d.api.loadHistory();
    if(change==='logout')delete stored.managementKey;if(change==='replace')stored.managementKey=JSON.stringify('new-key');release();await pending;
    assert(!descendants(d.elements.get('recent-table')).some(n=>n.textContent==='private-stale'));assert.equal(historyRows(d).length,0);
    assert.equal(d.elements.get('history-retry').disabled,true);
  }
});

test('reasoning is literal host metadata, never inferred from suffixes, tokens or unknown source; missing records are bilingual',()=>{
  const d=dashboard();const attack='<img src=x onerror=alert(1)>';
  d.api.renderSample({recent:[{model:'m',reasoning_effort:'high',reasoning_effort_source:'host_usage'},{model:'m(high)',reasoning:1000},{reasoning_effort:'high',reasoning_effort_source:'model_suffix'},{reasoning_effort:attack,reasoning_effort_source:'host_usage'}]});
  const cells=()=>descendants(d.elements.get('recent-table')).filter(n=>n.className==='request-reasoning');
  assert.deepEqual(cells().map(n=>n.textContent),['high','未提供','未提供',attack]);assert(!descendants(d.elements.get('recent-table')).some(n=>n.tag==='img'));
  d.api.setLanguage('en');assert.deepEqual(cells().map(n=>n.textContent),['high','Not provided','Not provided',attack]);assert.match(cells()[1].title,/never inferred or backfilled/);
});

test('rolling history keeps empty dates and exact 24-hour signed parameters across paging, sync and refresh',async()=>{
  const requested=[],anchor=Date.parse('2026-03-09T13:45:12.345Z');
  const d=dashboard(login(),async url=>{
    if(!url.includes('/history?'))return reply(historySnapshot());
    const params=new URL(url,'https://cpa.test').searchParams;requested.push(params);
    return reply(historyReply(params,Array.from({length:20},()=>({model:'rolling'})),80,'next-token'));
  },{fakeClock:true,now:anchor});
  await d.api.load();d.api.setTab('requests');await waitForTest(()=>historyRows(d).length===20);
  assert.equal(d.elements.get('history-start').value,'');assert.equal(d.elements.get('history-end').value,'');
  const first=requested.at(-1),sameQuery=p=>{for(const name of ['start','end','provider','model_status','limit'])assert.equal(p.get(name),first.get(name));};
  assert.equal(first.get('end'),new Date(anchor).toISOString());assert.equal(Date.parse(first.get('end'))-Date.parse(first.get('start')),86400000);
  assert.match(d.elements.get('history-range').textContent,/滚动 24 小时/);
  d.advanceTime(120000);await d.elements.get('recent-next').onclick();sameQuery(requested.at(-1));assert.equal(requested.at(-1).get('cursor'),'next-token');
  const count=requested.length;await d.api.load();assert.equal(requested.length,count,'the minute-sync load path cannot replace a deeper page');
  d.advanceTime(60000);await d.elements.get('recent-prev').onclick();sameQuery(requested.at(-1));assert.equal(requested.at(-1).get('cursor'),'first-cursor');
  await d.api.load();assert.equal(requested.at(-1).get('end'),new Date(anchor+180000).toISOString());assert(!requested.at(-1).has('cursor'));
  d.advanceTime(60000);await d.elements.get('refresh').onclick();assert.equal(requested.at(-1).get('end'),new Date(anchor+240000).toISOString());assert(!requested.at(-1).has('cursor'));
  for(const change of [()=>d.elements.get('provider').onchange({target:{value:'codex'}}),()=>d.elements.get('history-model').onchange(),()=>d.elements.get('recent-size').onchange({target:{value:'40'}})]){
    d.advanceTime(1000);await change();assert(!requested.at(-1).has('cursor'));assert.equal(Date.parse(requested.at(-1).get('end'))-Date.parse(requested.at(-1).get('start')),86400000);
  }
  d.elements.get('history-start').value='2026-03-08';d.elements.get('history-end').value='2026-03-08';await d.elements.get('history-retry').onclick();
  assert.match(d.elements.get('history-range').textContent,/所选日期/);
  await d.elements.get('history-rolling').onclick();assert.equal(d.elements.get('history-start').value,'');assert.equal(d.elements.get('history-end').value,'');assert(!requested.at(-1).has('cursor'));
  assert.equal(Date.parse(requested.at(-1).get('end'))-Date.parse(requested.at(-1).get('start')),86400000);
});

test('half-filled, reversed and malformed custom dates do not silently become rolling history',async()=>{
  let requests=0;const d=dashboard(login(),async url=>{if(!url.includes('/history?'))return reply(historySnapshot());requests++;return reply(historyReply(new URL(url,'https://cpa.test').searchParams,[]));});
  await d.api.load();
  for(const [start,end] of [['2026-10-09',''],['','2026-10-09'],['2026-10-10','2026-10-09'],['2026-02-30','2026-03-01']]){
    d.elements.get('history-start').value=start;d.elements.get('history-end').value=end;await d.elements.get('history-retry').onclick();
    assert.equal(requests,0);assert.match(d.elements.get('history-message').textContent,/有效日期/);assert.equal(d.elements.get('history-range').textContent,'');
  }
});

const visibleAccounts=d=>descendants(d.elements.get('quota-cards')).filter(n=>n.dataset.authId);
const manyAccounts=()=>({...twoAccounts(),quotas:Array.from({length:137},(_,i)=>({auth_id:'credential-'+i,name:'team-'+String(i).padStart(3,'0')+'@example.test',provider:['codex','claude','antigravity'][i%3],disabled:i%5===0,manual_reset_remaining:2,five_hour:{used_percent:i%100},seven_day:{used_percent:20}}))});
for(const language of ['zh','en'])test(`${language}: many accounts have bounded cards, searchable IDs, status/provider filters, sorting and page sizes`,async()=>{
  const sample=manyAccounts(),d=dashboard({},undefined,{language});d.api.renderSample(sample);
  assert.equal(visibleAccounts(d).length,12);assert.equal(d.elements.get('account-page').textContent,'1 / 12');assert.equal(d.elements.get('account-prev').disabled,true);
  await d.elements.get('account-next').onclick();assert.equal(d.elements.get('account-page').textContent,'2 / 12');assert(visibleAccounts(d).some(n=>n.dataset.authId==='credential-36'));
  d.api.acceptSnapshot({...sample,generated_at:'next'});assert.equal(d.elements.get('account-page').textContent,'2 / 12');
  d.elements.get('account-search').oninput({target:{value:'  CREDENTIAL-132  '}});assert.deepEqual(visibleAccounts(d).map(n=>n.dataset.authId),['credential-132']);assert.equal(d.elements.get('account-page').textContent,'1 / 1');
  const card=visibleAccounts(d)[0];assert(refreshButton(card));assert(toggleButton(card));assert(resetButton(card));assert(!descendants(card).some(n=>n.className==='account-id'||n.textContent==='credential-132'));
  await d.elements.get('account-clear').onclick();d.elements.get('account-search').oninput({target:{value:'TEAM-01'}});assert.equal(visibleAccounts(d).length,10);
  d.elements.get('account-state').onchange({target:{value:'disabled'}});assert.equal(visibleAccounts(d).length,2);assert(visibleAccounts(d).every(n=>n.className.includes('account-disabled')));
  await d.elements.get('provider').onchange({target:{value:'codex'}});assert.equal(visibleAccounts(d).length,1);assert.equal(visibleAccounts(d)[0].dataset.authId,'credential-15');
  d.api.setLanguage(language==='zh'?'en':'zh');assert.equal(visibleAccounts(d).length,1,'language switching preserves search and filters');
  d.elements.get('account-search').oninput({target:{value:'<img src=x onerror=alert(1)>'}});assert.equal(visibleAccounts(d).length,0);assert.equal(d.elements.get('account-next').disabled,true);assert(!descendants(d.elements.get('quota-cards')).some(n=>n.tag==='img'));
  await d.elements.get('account-clear').onclick();d.elements.get('account-size').onchange({target:{value:'48'}});assert.equal(visibleAccounts(d).length,48);assert.equal(d.elements.get('account-page-bottom').textContent,'1 / 3');
  await d.elements.get('account-next-bottom').onclick();await d.elements.get('account-next-bottom').onclick();assert.equal(visibleAccounts(d).length,41);assert.equal(d.elements.get('account-next-bottom').disabled,true);
  d.api.acceptSnapshot({...sample,quotas:sample.quotas.slice(0,3)});assert.equal(d.elements.get('account-page').textContent,'1 / 1');assert.equal(visibleAccounts(d).length,3);
  d.api.renderSample(sample);d.elements.get('account-size').onchange({target:{value:'12'}});d.elements.get('account-sort').onchange({target:{value:'remaining'}});
  const expected=sample.quotas.filter(q=>q.provider==='codex').sort((a,b)=>Math.min(100-a.five_hour.used_percent,80)-Math.min(100-b.five_hour.used_percent,80)||a.name.localeCompare(b.name)).slice(0,12);
  assert.deepEqual(visibleAccounts(d).map(n=>n.dataset.authId),expected.map(q=>q.auth_id),'sort applies to the full provider group before shared pagination');
  d.elements.get('account-sort').onchange({target:{value:'name'}});assert.deepEqual(visibleAccounts(d).map(n=>n.dataset.authId),Array.from({length:12},(_,i)=>'credential-'+i*3));
  assert.equal(d.elements.get('quota-cards').children[0].children[0].children[1].textContent,'12 / 46');
});

test('account paging preserves pending jobs, errors and actions without retaining off-page cards',async()=>{
  let release;const sample=manyAccounts(),d=dashboard(login(),async url=>url.endsWith('refresh-account')?new Promise(resolve=>release=()=>resolve(reply({error:'account_not_found'},404))):reply(sample));
  await d.api.load();const job=d.api.refreshAccount('credential-3');assert.equal(refreshButton(panel(d.elements,'credential-3')).disabled,true);
  await d.elements.get('account-next').onclick();assert(!panel(d.elements,'credential-3'));assert.equal(visibleAccounts(d).length,12);
  await d.elements.get('account-prev').onclick();assert.equal(refreshButton(panel(d.elements,'credential-3')).disabled,true);
  await d.elements.get('account-next').onclick();release();await job;await d.elements.get('account-prev').onclick();
  const card=panel(d.elements,'credential-3');assert.equal(refreshButton(card).disabled,false);assert.equal(accountError(card).textContent,'未找到账户。');assert(toggleButton(card));assert(resetButton(card));
});

for(const language of ['zh','en'])test(`${language}: filenames never become account/request labels, tooltips or accessible names`,async()=>{
  const d=dashboard({},undefined,{language});
  const records=[
    {auth_id:'codex-a-friendly@example.test-team.json',name:'Friendly account',provider:'codex'},
    {auth_id:'codex-b-hidden@example.test-team.json',name:'codex-b-hidden@example.test-team.json',provider:'codex'},
    {auth_id:'codex-c-hidden@example.test-team.json',provider:'codex'},
    {auth_id:'claude-d-hidden@example.test.json',name:' /internal/claude-d-hidden@example.test.JSON ',provider:'claude'},
    {auth_id:'ag-e-hidden@example.test.json',name:'  ',provider:'antigravity'},
    {auth_id:'ag-f@example.test.json',name:'visible@example.test',provider:'antigravity'},
    {auth_id:'non-file-internal-id',name:17,provider:'codex'},
  ];
  const sample={quotas:records,credentials:records.map(s=>({...s,requests:1,models:[]})),recent:[
    ...records.map(s=>({auth_id:s.auth_id,provider:s.provider})),
    {auth_id:'orphan-filename.json',credential_name:'orphan-filename.json',provider:'codex'},
    {auth_id:'orphan-without-name.json',provider:'codex'},
  ]};
  d.api.renderSample(sample);
  const names=()=>records.map(record=>descendants(panel(d.elements,record.auth_id)).find(n=>n.className==='account').textContent);
  const original=names(),placeholder=language==='en'?/^Account [0-9A-Z]{7}$/:/^账户 [0-9A-Z]{7}$/;
  assert.equal(original[0],'Friendly account');assert.equal(original[5],'visible@example.test');
  for(const i of [1,2,3,4,6])assert.match(original[i],placeholder);
  assert.equal(new Set(original).size,records.length,'unnamed accounts are distinguishable');
  const safe=()=>{
    for(const id of ['quota-cards','credential-table','recent-table','reset-card'])for(const node of descendants(d.elements.get(id))){
      for(const text of [node.textContent,node.title,node.getAttribute('aria-label')])assert(!/\.json\b|\/internal\/|non-file-internal-id/i.test(String(text||'')),`${id}: internal identifier leaked`);
    }
  };
  safe();
  const requestLabels=descendants(d.elements.get('credential-table')).filter(n=>n.className==='credential-name').map(n=>n.textContent);
  assert.deepEqual(requestLabels,original);
  const recentLabels=descendants(d.elements.get('recent-table')).filter(n=>n.className==='credential-name').map(n=>n.textContent);
  assert.deepEqual(recentLabels.slice(0,records.length),original);assert(recentLabels.slice(records.length).every(name=>placeholder.test(name)));
  d.elements.get('account-search').oninput({target:{value:original[2]}});assert.equal(visibleAccounts(d).length,1,'safe placeholders are searchable');
  await d.elements.get('account-clear').onclick();assert.deepEqual(names(),original);
  d.api.acceptSnapshot({...sample,quotas:records.map(s=>({...s,fetched_at:'later'}))});assert.deepEqual(names(),original);safe();
  d.api.setLanguage(language==='en'?'zh':'en');safe();
});

test('provider stacks retain group order, sort within each group and preserve unchanged cards',()=>{
  const d=dashboard(),quotas=[
    {auth_id:'z',name:'Zulu',provider:'codex',five_hour:{used_percent:20}},
    {auth_id:'a',name:'Alpha',provider:'claude',five_hour:{used_percent:70}},
    {auth_id:'m',name:'Mike',provider:'antigravity',five_hour:{used_percent:50}},
    {auth_id:'b',name:'jessica510tang@gmail.com',provider:'codex',five_hour:{used_percent:90}},
    {auth_id:'c',name:'Very-long-'.repeat(30)+'@example.test',provider:'claude'},
  ];
  d.api.renderSample({quotas});const cards=new Map(visibleAccounts(d).map(card=>[card.dataset.authId,card])),stacks=[...d.elements.get('quota-cards').children];
  const ids=()=>visibleAccounts(d).map(card=>card.dataset.authId);
  assert.deepEqual(ids(),['z','b','a','c','m']);
  d.elements.get('account-sort').onchange({target:{value:'name'}});assert.deepEqual(ids(),['b','z','a','c','m']);
  d.elements.get('account-sort').onchange({target:{value:'remaining'}});assert.deepEqual(ids(),['b','z','a','c','m']);
  assert.deepEqual(d.elements.get('quota-cards').children,stacks);
  for(const id of ['b','c']){const label=descendants(cards.get(id)).find(n=>n.className==='account');assert.equal(label.title,label.textContent);assert.equal(label.getAttribute('aria-label'),label.textContent);assert.equal(label.tabIndex,0);assert.equal(label.id,'account-name-'+encodeURIComponent(id));}
  assert.match(d.elements.get('account-sort-hint').textContent,/排序仅在组内生效/);
  d.api.acceptSnapshot({quotas:quotas.map(q=>q.auth_id==='z'?{...q,manual_reset_remaining:2}:q)});
  assert.equal(panel(d.elements,'a'),cards.get('a'));assert.equal(panel(d.elements,'m'),cards.get('m'));assert.notEqual(panel(d.elements,'z'),cards.get('z'));
  assert.deepEqual(d.elements.get('quota-cards').children,stacks);
});

test('hidden filename IDs still drive refresh, reset and CPA status operations unchanged',async()=>{
  const id='codex-c5d52f1d-hidden@example.test-team.json',calls=[];
  let finished=0,sample={...sampleSnapshot(),quotas:[{auth_id:id,name:id,provider:'codex',manual_reset_remaining:2}]};
  const d=dashboard(login(),async(url,options)=>{
    calls.push({url,options});
    if(url.endsWith('refresh-account')){const baseline=String(finished++);return reply({refresh_finished_at:baseline},202);}
    if(url.endsWith('reset-credits'))return reply({available_count:1,credits:[{id:'safe-credit'}]});
    if(url.endsWith('consume-reset'))return reply({code:'nothing_to_reset'});
    if(url.endsWith('/auth-files/status')){sample={...sample,quotas:sample.quotas.map(q=>({...q,disabled:true}))};return reply({disabled:true});}
    return reply({...sample,account_refresh:{[id]:{refreshing:false,finished_at:String(finished)}}});
  });
  await d.api.load();const card=panel(d.elements,id),name=descendants(card).find(n=>n.className==='account').textContent;
  assert.match(name,/^账户 [0-9A-Z]{7}$/);
  await refreshButton(card).onclick();assert.equal(JSON.parse(posted(calls,'refresh-account')[0].options.body).auth_id,id);
  await resetButton(panel(d.elements,id)).onclick();assert(!d.elements.get('reset-subtitle').textContent.includes('.json'));assert(d.elements.get('reset-subtitle').textContent.includes(name));
  await d.api.confirmReset();
  for(const suffix of ['reset-credits','consume-reset'])assert.equal(JSON.parse(posted(calls,suffix)[0].options.body).auth_id,id);
  await toggleButton(panel(d.elements,id)).onclick();assert(!d.elements.get('disable-subtitle').textContent.includes('.json'));await d.api.confirmDisable();
  assert.deepEqual(JSON.parse(posted(calls,'/auth-files/status')[0].options.body),{name:id,disabled:true});
  assert.equal(posted(calls,'/auth-files/status')[0].options.method,'PATCH');
  assert.equal(toggleButton(panel(d.elements,id)).children[1].textContent,'已禁用');
});

test('browser local midnight uses calendar boundaries across DST rather than adding 24 hours',()=>{
  const previous=process.env.TZ;process.env.TZ='America/New_York';
  try{
    const d=dashboard();d.elements.get('history-start').value='2026-03-08';d.elements.get('history-end').value='2026-03-08';
    let p=d.api.historyParams();assert.equal(new Date(p.get('end'))-new Date(p.get('start')),23*3600000);
    d.elements.get('history-start').value='2026-11-01';d.elements.get('history-end').value='2026-11-01';p=d.api.historyParams();assert.equal(new Date(p.get('end'))-new Date(p.get('start')),25*3600000);
  }finally{if(previous===undefined)delete process.env.TZ;else process.env.TZ=previous;}
});

const diagnosticExpectations = [
  ['request_failed', '未能读取（请求失败）', 'Could not read (request failed)'],
  ['cli_version', '未能读取（客户端版本）', 'Could not read (CLI version)'],
  ['surface', '未能读取（入口不受支持）', 'Could not read (surface unsupported)'],
  ['not_eligible', '未能读取（无资格）', 'Could not read (not eligible)'],
  ['unavailable', '未能读取（不可用）', 'Could not read (unavailable)'],
  ['invalid_response', '未能读取（响应格式异常）', 'Could not read (invalid response)'],
];

for(const language of ['zh','en'])test(`${language}: Claude reset inventory diagnostic renders all 6 allowed enums, preserves known counts and never invents zero`,()=>{
  const d=dashboard({},undefined,{language});
  for(const [diagnostic,zhText,enText] of diagnosticExpectations){
    const expectedLabel=language==='en'?'Resets remaining':'可重置',expectedText=language==='en'?enText:zhText;
    d.api.renderSample({quotas:[{auth_id:'claude-diag',provider:'claude',name:'claude@example.com',manual_reset_diagnostic:diagnostic}]});
    const card=panel(d.elements,'claude-diag');
    const resets=descendants(card).filter(n=>n.className==='manual-reset');
    assert.equal(resets.length,1,`diagnostic ${diagnostic} must render manual-reset container`);
    assert.deepEqual(resets[0].children.map(c=>c.textContent),[expectedLabel,expectedText]);
    assert.equal(descendants(card).find(n=>n.className==='button account-action account-reset'),undefined,'Claude never receives a reset button');
    if(language==='en')for(const node of descendants(resets[0]))assert(!/[\u3400-\u9fff]/.test(node.textContent),node.textContent);
  }
  // Known counts (positive and explicit 0) must always take precedence over diagnostic and show count, not diagnostic or false zero.
  d.api.renderSample({quotas:[{auth_id:'claude-known-2',provider:'claude',name:'claude@example.com',manual_reset_remaining:2,manual_reset_diagnostic:'request_failed'},{auth_id:'claude-known-0',provider:'claude',name:'claude0@example.com',manual_reset_remaining:0,manual_reset_diagnostic:'unavailable'}]});
  const reset2=descendants(panel(d.elements,'claude-known-2')).find(n=>n.className==='manual-reset');
  assert.deepEqual(reset2.children.map(c=>c.textContent),language==='en'?['Resets remaining','2']:['可重置','2','次']);
  const reset0=descendants(panel(d.elements,'claude-known-0')).find(n=>n.className==='manual-reset');
  assert.deepEqual(reset0.children.map(c=>c.textContent),language==='en'?['Resets remaining','0']:['可重置','0','次']);

  // Invalid/unrecognized count values fall back to diagnostic rather than reporting 0 or counts.
  for(const invalidRemaining of [null, undefined, -1, '2', NaN, false]){
    d.api.renderSample({quotas:[{auth_id:'claude-invalid-rem',provider:'claude',name:'claude@example.com',manual_reset_remaining:invalidRemaining,manual_reset_diagnostic:'request_failed'}]});
    const resetInvalid=descendants(panel(d.elements,'claude-invalid-rem')).find(n=>n.className==='manual-reset');
    assert.deepEqual(resetInvalid.children.map(c=>c.textContent),language==='en'?['Resets remaining','Could not read (request failed)']:['可重置','未能读取（请求失败）']);
  }
});

test('Claude reset diagnostic compatibility: unknown values and absent fields render nothing and leak no upstream data',()=>{
  const d=dashboard();
  const unknownValues=['unknown_status','request_timeout','<script>alert(1)</script>','Bearer secret_token',123,true,{},[],null,''];
  for(const val of unknownValues){
    d.api.renderSample({quotas:[{auth_id:'claude-leak',provider:'claude',name:'claude@example.com',manual_reset_diagnostic:val}]});
    const card=panel(d.elements,'claude-leak');
    const resets=descendants(card).filter(n=>n.className==='manual-reset');
    assert.equal(resets.length,0,`unknown diagnostic value ${String(val)} must stay hidden`);
    assert(!descendants(card).some(n=>typeof val==='string'&&val.length>3&&n.textContent.includes(val)),`raw value ${String(val)} leaked into card`);
  }
  // Old backend without manual_reset_diagnostic and without manual_reset_remaining renders nothing.
  d.api.renderSample({quotas:[{auth_id:'claude-old',provider:'claude',name:'old@example.com'}]});
  assert.equal(descendants(panel(d.elements,'claude-old')).filter(n=>n.className==='manual-reset').length,0);
});

test('Codex and Antigravity reset behaviors are completely unaffected by diagnostic values',()=>{
  const d=dashboard();
  // Codex with known count gets count display and reset button.
  // Codex with unknown count and diagnostic gets NO diagnostic display and NO reset button.
  // Antigravity with diagnostic gets NO diagnostic display.
  d.api.renderSample({quotas:[
    {auth_id:'codex-known',provider:'codex',name:'c1@example.com',manual_reset_remaining:2,manual_reset_diagnostic:'request_failed'},
    {auth_id:'codex-unknown',provider:'codex',name:'c2@example.com',manual_reset_diagnostic:'request_failed'},
    {auth_id:'ag-unknown',provider:'antigravity',name:'ag@example.com',manual_reset_diagnostic:'cli_version'}
  ]});
  const codexKnown=panel(d.elements,'codex-known');
  const codexKnownReset=descendants(codexKnown).find(n=>n.className==='manual-reset');
  assert.deepEqual(codexKnownReset.children.map(c=>c.textContent),['可重置','2','次']);
  assert(descendants(codexKnown).some(n=>n.className==='button account-action account-reset'&&n.textContent==='使用重置卡'));

  const codexUnknown=panel(d.elements,'codex-unknown');
  assert.equal(descendants(codexUnknown).filter(n=>n.className==='manual-reset').length,0);
  assert.equal(descendants(codexUnknown).filter(n=>n.className==='button account-action account-reset').length,0);

  const agCard=panel(d.elements,'ag-unknown');
  assert.equal(descendants(agCard).filter(n=>n.className==='manual-reset').length,0);
});

test('Claude reset diagnostic updates card cache on snapshot changes and preserves card controls',()=>{
  const d=dashboard();
  d.api.acceptSnapshot({quotas:[{auth_id:'c-test',provider:'claude',name:'c@example.com',manual_reset_diagnostic:'request_failed'}]});
  let card=panel(d.elements,'c-test');
  let reset=descendants(card).find(n=>n.className==='manual-reset');
  assert.deepEqual(reset.children.map(c=>c.textContent),['可重置','未能读取（请求失败）']);
  assert(refreshButton(card),'refresh button present');
  assert(toggleButton(card),'toggle button present');

  // Updating to cli_version updates the cached card.
  d.api.acceptSnapshot({quotas:[{auth_id:'c-test',provider:'claude',name:'c@example.com',manual_reset_diagnostic:'cli_version'}]});
  card=panel(d.elements,'c-test');
  reset=descendants(card).find(n=>n.className==='manual-reset');
  assert.deepEqual(reset.children.map(c=>c.textContent),['可重置','未能读取（客户端版本）']);

  // Updating to known count replaces diagnostic with count.
  d.api.acceptSnapshot({quotas:[{auth_id:'c-test',provider:'claude',name:'c@example.com',manual_reset_remaining:1}]});
  card=panel(d.elements,'c-test');
  reset=descendants(card).find(n=>n.className==='manual-reset');
  assert.deepEqual(reset.children.map(c=>c.textContent),['可重置','1','次']);

  // Clearing to old backend state removes manual-reset entirely.
  d.api.acceptSnapshot({quotas:[{auth_id:'c-test',provider:'claude',name:'c@example.com'}]});
  card=panel(d.elements,'c-test');
  assert.equal(descendants(card).filter(n=>n.className==='manual-reset').length,0);
});

for(const language of ['zh','en'])test(`${language}: adding new subscription updates card list, provider stacks, counters, and cache`,async()=>{
  const d=dashboard({},undefined,{language});
  const sample1={
    quotas:[{auth_id:'codex-1',provider:'codex',name:'alice@example.com',five_hour:{used_percent:20}}],
  };
  d.api.renderSample(sample1);
  assert.equal(visibleAccounts(d).length,1);
  assert.equal(d.elements.get('quota-count').textContent,'1');
  assert.equal(d.elements.get('account-results').textContent,language==='zh'?'显示 1–1 / 匹配 1 个，共 1 个账户':'Showing 1–1 / 1 matching, 1 accounts total');

  // New account of existing provider and new account of a new provider appear in snapshot
  const sample2={
    quotas:[
      {auth_id:'codex-1',provider:'codex',name:'alice@example.com',five_hour:{used_percent:20}},
      {auth_id:'codex-2',provider:'codex',name:'bob@example.com',five_hour:{used_percent:35}},
      {auth_id:'claude-1',provider:'claude',name:'clara@example.com',five_hour:{used_percent:10}},
    ],
  };
  d.api.acceptSnapshot(sample2);
  assert.equal(visibleAccounts(d).length,3);
  assert.equal(d.elements.get('quota-count').textContent,'3');
  assert.equal(d.elements.get('account-results').textContent,language==='zh'?'显示 1–3 / 匹配 3 个，共 3 个账户':'Showing 1–3 / 3 matching, 3 accounts total');
  const stacks=d.elements.get('quota-cards').children;
  assert.equal(stacks.length,2);
  assert.deepEqual([...stacks].map(s=>s.dataset.provider),['codex','claude']);
  assert.equal(stacks[0].children[0].children[1].textContent,'2 / 2');
  assert.equal(stacks[1].children[0].children[1].textContent,'1 / 1');
  const card1=panel(d.elements,'codex-1'),card2=panel(d.elements,'codex-2'),card3=panel(d.elements,'claude-1');
  assert(card1&&card2&&card3);
  assert(refreshButton(card2));assert(toggleButton(card2));
  assert(refreshButton(card3));assert(toggleButton(card3));
});

for(const language of ['zh','en'])test(`${language}: manual refresh accepts new subscription snapshot and updates cards immediately`,async()=>{
  let calls=0;
  const sample1={
    quotas:[{auth_id:'codex-1',provider:'codex',name:'alice@example.com',five_hour:{used_percent:20}}],
    quota_refresh_finished_at:'2026-10-09T01:00:00Z',
  };
  const sample2={
    quotas:[
      {auth_id:'codex-1',provider:'codex',name:'alice@example.com',five_hour:{used_percent:20}},
      {auth_id:'codex-2',provider:'codex',name:'bob@example.com',five_hour:{used_percent:30}},
    ],
    quota_refresh_finished_at:'2026-10-09T02:00:00Z',
  };
  const d=dashboard(login(),async(url)=>{
    calls++;
    if(url.endsWith('refresh'))return reply({quota_refresh_finished_at:'2026-10-09T01:00:00Z'},202);
    return reply(calls<=2?sample1:sample2);
  },{language});

  await d.api.load();
  assert.equal(visibleAccounts(d).length,1);
  assert.equal(d.elements.get('quota-count').textContent,'1');

  // Trigger manual refresh
  await d.elements.get('refresh').onclick();
  assert.equal(visibleAccounts(d).length,2);
  assert.equal(d.elements.get('quota-count').textContent,'2');
  assert.equal(d.elements.get('account-results').textContent,language==='zh'?'显示 1–2 / 匹配 2 个，共 2 个账户':'Showing 1–2 / 2 matching, 2 accounts total');
});

for(const language of ['zh','en'])test(`${language}: pagination boundary, sorting and filter limits when new subscription appears`,async()=>{
  const d=dashboard(login(),undefined,{language});
  const twelveAccounts=Array.from({length:12},(_,i)=>({
    auth_id:'acc-'+String(i).padStart(2,'0'),
    provider:'codex',
    name:'user-'+String(i).padStart(2,'0')+'@example.test',
    five_hour:{used_percent:20},
  }));
  d.api.renderSample({quotas:twelveAccounts});
  assert.equal(visibleAccounts(d).length,12);
  assert.equal(d.elements.get('account-page').textContent,'1 / 1');
  assert.equal(d.elements.get('account-next').disabled,true);

  // 13th account arrives: triggers second page
  const thirteenAccounts=[...twelveAccounts,{
    auth_id:'acc-12',
    provider:'codex',
    name:'user-12@example.test',
    five_hour:{used_percent:20},
  }];
  d.api.acceptSnapshot({quotas:thirteenAccounts});
  assert.equal(visibleAccounts(d).length,12);
  assert.equal(d.elements.get('account-page').textContent,'1 / 2');
  assert.equal(d.elements.get('account-next').disabled,false);
  assert.equal(d.elements.get('account-results').textContent,language==='zh'?'显示 1–12 / 匹配 13 个，共 13 个账户':'Showing 1–12 / 13 matching, 13 accounts total');

  // Navigate to page 2: only 13th account is on page 2
  await d.elements.get('account-next').onclick();
  assert.equal(visibleAccounts(d).length,1);
  assert.equal(visibleAccounts(d)[0].dataset.authId,'acc-12');
  assert.equal(d.elements.get('account-page').textContent,'2 / 2');
  assert.equal(d.elements.get('account-results').textContent,language==='zh'?'显示 13–13 / 匹配 13 个，共 13 个账户':'Showing 13–13 / 13 matching, 13 accounts total');

  // Filter limits: filter by provider hides accounts of other providers but preserves total count
  const withClaude=[...thirteenAccounts,{
    auth_id:'claude-new',
    provider:'claude',
    name:'claudia@example.test',
    five_hour:{used_percent:10},
  }];
  await d.elements.get('provider').onchange({target:{value:'codex'}});
  d.api.acceptSnapshot({quotas:withClaude});
  // Page reset to 0 by provider onchange, only codex matched
  assert.equal(visibleAccounts(d).length,12);
  assert.equal(d.elements.get('quota-count').textContent,'13');
  assert.equal(d.elements.get('account-results').textContent,language==='zh'?'显示 1–12 / 匹配 13 个，共 14 个账户':'Showing 1–12 / 13 matching, 14 accounts total');

  // Clear filter reveals all accounts including new Claude subscription
  await d.elements.get('account-clear').onclick();
  assert.equal(d.elements.get('quota-count').textContent,'14');
  assert.equal(d.elements.get('account-results').textContent,language==='zh'?'显示 1–12 / 匹配 14 个，共 14 个账户':'Showing 1–12 / 14 matching, 14 accounts total');
});

test('dedicated scheduled wakeup tab supports 5-hour cron presets, persistence status, cadence builder, model selects and full/single wakeup', async () => {
  const calls = [];
  let persisted = false;
  let savedConfig = {
    enabled: false, mode: 'daily', cron: '0 6,11,16,21 * * *', timezone_offset_min: 480, skip_active_window: true,
    providers: { claude: true, codex: true, antigravity: true }, excluded_auth_ids: [], models: { 'antigravity:claude-gpt': 'claude-sonnet-4-6' }, message: 'hi',
  };
  let runSeq = 0;
  let results = {};
  let running = false;
  let runningAuths = [];
  let finishedAt = '';
  const snapshotPayload = () => ({
    quotas: [
      { auth_id: 'claude-a', provider: 'claude', name: 'alice@example.com', five_hour: { used_percent: 0 } },
      { auth_id: 'codex-full', provider: 'codex', name: 'full@example.com', five_hour: { used_percent: 100 } },
      { auth_id: 'codex-b', provider: 'codex', name: 'bob@example.com', disabled: true, five_hour: { used_percent: 0 } },
      { auth_id: 'ag-gemini', provider: 'antigravity', name: 'google@example.test', groups: [{ key:'claude-gpt', five_hour:{used_percent:100} },{ key:'gemini', five_hour:{used_percent:10} }] },
    ],
    persistence: { enabled: true, directory: '/tmp/cpa' },
    wakeup: {
      config: savedConfig,
      config_path: '/tmp/cpa/wakeup.json',
      persisted,
      running,
      running_auth_ids: runningAuths,
      last_run_at: finishedAt,
      last_run_finished_at: finishedAt,
      last_run_trigger: runSeq ? (lastForce ? 'test' : 'manual') : '',
      next_scheduled_at: savedConfig.enabled ? '2026-11-25T03:00:00Z' : '',
      next_scheduled_runs: savedConfig.enabled ? ['2026-11-25T03:00:00Z', '2026-11-25T08:00:00Z'] : [],
      results,
    },
  });
  let lastForce = false;
  const d = dashboard({ managementKey: 'key' }, async (url, options = {}) => {
    calls.push({ url, options });
    if (url.startsWith('/v0/management/auth-files/models')) {
      if (url.includes('ag-gemini')) {
        return reply({ models: ['claude-sonnet-4-6','gpt-oss-120b-medium','gemini-3-flash-image','gemini-3.5-flash-lite'].map(id=>({id})) });
      }
      if (url.includes('codex-full')) {
        return { ok: true, status: 200, json: async () => ({ models: [{ id: 'gpt-6-astra' }, { id: 'codex-auto-review' }, { id: 'gpt-6-luna' }, { id: 'gpt-5.1-codex-mini' }] }) };
      }
      return { ok: true, status: 200, json: async () => ({ models: [{ id: 'claude-opus-4-6' }, { id: 'claude-haiku-4-5-20251001' }, { id: 'claude-sonnet-4-6' }] }) };
    }
    if (url.endsWith('/wakeup/config')) {
      savedConfig = JSON.parse(options.body);
      persisted = true;
      return { ok: true, status: 200, json: async () => ({ wakeup: snapshotPayload().wakeup }) };
    }
    if (url.endsWith('/wakeup/exclude')) {
      const body = JSON.parse(options.body);
      savedConfig = { ...savedConfig, excluded_auth_ids: body.excluded ? [body.auth_id] : [] };
      persisted = true;
      return { ok: true, status: 200, json: async () => ({ wakeup: snapshotPayload().wakeup }) };
    }
    if (url.endsWith('/wakeup/run')) {
      const body = JSON.parse(options.body);
      lastForce = Boolean(body.force);
      runSeq++;
      finishedAt = `2026-11-25T06:00:0${runSeq}Z`;
      running = false;
      runningAuths = [];
      results = {
        'claude-a': { auth_id: 'claude-a', provider: 'claude', name: 'alice@example.com', status: 'sent', model: 'claude-haiku-4-5-20251001', at: finishedAt, window_reset_at: '2026-11-25T11:00:00Z' },
        'codex-full': { auth_id: 'codex-full', provider: 'codex', name: 'full@example.com', status: 'exhausted', reason: 'quota_exhausted', at: finishedAt },
      };
      return { ok: true, status: 202, json: async () => ({ status: 'running', auth_id: body.auth_id || '', last_run_finished_at: '', account_result_at: '' }) };
    }
    return { ok: true, status: 200, json: async () => snapshotPayload() };
  });

  // Verify frontend Cron cadence builder and parser match 5-hour daytime schedule.
  assert.equal(d.api.buildCadenceCron(6, 0, 5, 22, '*'), '0 6,11,16,21 * * *');
  assert.equal(d.api.buildCadenceCron(6, 5, 5, 22, '1-5'), '5 6,11,16,21 * * 1-5');
  assert(d.api.nextCronRunsJS('0 6,11,16,21 * * *', 480, 4).length === 4);
  assert.equal(d.api.nextCronRunsJS('bad cron', 480, 4), null);

  await d.api.load();
  assert.equal(d.elements.get('tab-wakeup').getAttribute('aria-selected'), 'false');
  assert.equal(d.elements.get('wakeup-tab-badge').textContent, '关');
  assert.equal(d.elements.get('page-wakeup').getAttribute('class'), 'hide');

  // Switch to dedicated wakeup tab: panel becomes visible and shows persistence file path.
  d.api.setTab('wakeup');
  assert.equal(d.elements.get('tab-wakeup').getAttribute('aria-selected'), 'true');
  assert(!d.elements.get('page-wakeup').classes.has('hide'));
  assert(d.elements.get('page-quota').classes.has('hide'));
  assert.equal(d.elements.get('wakeup-cron').value, '0 6,11,16,21 * * *');
  assert.match(d.elements.get('wakeup-persist').textContent, /\/tmp\/cpa\/wakeup\.json/);
  assert.equal(d.elements.get('wakeup-scope-text').textContent, '参与账户：3 / 3 个已启用');
  assert(descendants(d.elements.get('wakeup-accounts-list').children[2]).some(n=>n.textContent==='5h 剩 90%'));
  assert(descendants(d.elements.get('wakeup-cron-preview')).filter(n => n.className === 'wakeup-preview-chip').length === 4);

  // Wait a microtask for model hints fetch: auto recommendation prefers lightweight mini/haiku, never astra or auto-review.
  await new Promise(resolve => setImmediate(resolve));
  const codexOpts = d.elements.get('wakeup-model-select-codex').children;
  assert.match(codexOpts[0].textContent, /gpt-5\.1-codex-mini/);
  assert(!codexOpts[0].textContent.includes('astra'));
  assert(!codexOpts.some(o => o.value === 'codex-auto-review'));
  assert.match(d.elements.get('wakeup-model-select-claude').children[0].textContent, /claude-haiku-4-5-20251001/);
  assert.doesNotMatch(html, /wakeup-model-select-ag-claude/);
  const gemini = d.elements.get('wakeup-model-select-ag-gemini');
  assert.match(gemini.children[0].textContent, /gemini-3\.5-flash-lite/);
  assert(gemini.children.every(n=>!/(claude|gpt|image)/i.test(n.value||'')));

  // Clicking a preset updates Cron expression immediately.
  const weekdayPreset = d.elements.get('wakeup-presets').children.find(b => b.dataset.cron === '0 6,11,16,21 * * 1-5');
  assert(weekdayPreset, 'expected weekday 6AM preset');
  weekdayPreset.onclick();
  assert.equal(d.elements.get('wakeup-cron').value, '0 6,11,16,21 * * 1-5');
  assert.match(d.elements.get('wakeup-persist').textContent, /未保存更改/);

  // Cadence builder generates 5-hour schedule automatically on change.
  d.elements.get('wakeup-gen-start').value = '6';
  d.elements.get('wakeup-gen-min').value = '5';
  d.elements.get('wakeup-gen-step').value = '5';
  d.elements.get('wakeup-gen-end').value = '22';
  d.elements.get('wakeup-gen-days').value = '*';
  d.elements.get('wakeup-gen-step').onchange();
  assert.equal(d.elements.get('wakeup-cron').value, '5 6,11,16,21 * * *');

  // Validation rejects invalid Cron and invalid custom model names.
  d.elements.get('wakeup-enabled').checked = true;
  d.elements.get('wakeup-cron').value = 'invalid cron';
  assert.equal(await d.api.saveWakeupConfig(), false);
  assert.match(d.elements.get('wakeup-message-box').textContent, /Cron 表达式无效/);

  d.elements.get('wakeup-cron').value = '0 6,11,16,21 * * *';
  d.elements.get('wakeup-model-select-claude').value = '__custom__';
  d.elements.get('wakeup-model-select-claude').onchange();
  assert.equal(d.elements.get('wakeup-model-claude').hidden, false);
  d.elements.get('wakeup-model-claude').value = 'bad model name';
  assert.equal(await d.api.saveWakeupConfig(), false);
  assert.match(d.elements.get('wakeup-message-box').textContent, /模型名称格式无效/);

  // Draft input survives background acceptSnapshot without overwriting edited fields.
  d.elements.get('wakeup-model-select-claude').value = 'claude-haiku-4-5-20251001';
  d.elements.get('wakeup-model-select-claude').onchange();
  assert.equal(d.elements.get('wakeup-model-claude').hidden, true);
  gemini.value = '__custom__';
  gemini.onchange();
  d.elements.get('wakeup-model-ag-gemini').value = 'claude-sonnet-4-6';
  assert.equal(await d.api.saveWakeupConfig(), false);
  assert.match(d.elements.get('wakeup-message-box').textContent, /仅支持使用 Gemini 文本模型/);
  gemini.value = 'gemini-3.5-flash-lite';
  gemini.onchange();
  const dotQuickBtn = d.elements.get('wakeup-text').parentNode.children.find(n => n.dataset?.msg === '.');
  assert(dotQuickBtn, 'expected quick message button');
  dotQuickBtn.onclick();
  assert.equal(d.elements.get('wakeup-text').value, '.');
  d.api.acceptSnapshot(snapshotPayload());
  assert.equal(d.elements.get('wakeup-text').value, '.');

  // Flat account list in wakeup tab toggles exclusion and clears exclusion cleanly.
  const accRow = d.elements.get('wakeup-accounts-list').children[0];
  const accCheckbox = descendants(accRow).find(n => n.className === 'wakeup-account-item');
  accCheckbox.onclick();
  assert.match(d.elements.get('wakeup-scope-text').textContent, /已排除 1/);
  d.elements.get('wakeup-clear-excluded').onclick();
  assert.equal(d.elements.get('wakeup-scope-text').textContent, '参与账户：3 / 3 个已启用');

  // Save valid Cron config: persistence banner confirms durable disk save across CPA updates.
  assert.equal(await d.api.saveWakeupConfig(), true);
  const configCall = calls.find(c => c.url.endsWith('/wakeup/config'));
  assert(configCall, 'expected POST wakeup/config');
  const sentCfg = JSON.parse(configCall.options.body);
  assert.equal(sentCfg.enabled, true);
  assert.equal(sentCfg.mode, 'daily');
  assert.equal(sentCfg.cron, '0 6,11,16,21 * * *');
  assert.deepEqual(sentCfg.models, { claude:'claude-haiku-4-5-20251001', 'antigravity:gemini':'gemini-3.5-flash-lite' });
  assert.equal(sentCfg.message, '.');
  assert.match(d.elements.get('wakeup-tab-badge').textContent, /^下次 \d{2}:\d{2}$/);
  assert.match(d.elements.get('wakeup-persist').textContent, /已持久保存/);

  // Test all accounts button sends force=true and renders inline OK (green) & exhausted (amber) badges per account row.
  await d.elements.get('wakeup-test-all').onclick();
  const testAllCall = calls.filter(c => c.url.endsWith('/wakeup/run')).at(-1);
  assert.deepEqual(JSON.parse(testAllCall.options.body), { force: true });
  assert.match(d.elements.get('wakeup-message-box').textContent, /测试完成：OK 1，额度已用完 1，失败 0/);
  const rowBadges = descendants(d.elements.get('wakeup-accounts-list')).filter(n => String(n.className || '').startsWith('wakeup-badge '));
  assert(rowBadges.some(b => b.className === 'wakeup-badge sent' && descendants(b).some(c => c.textContent === 'OK')));
  assert(rowBadges.some(b => b.className === 'wakeup-badge exhausted' && descendants(b).some(c => c.textContent === '额度已用完')));
  assert(descendants(d.elements.get('wakeup-results')).some(n => n.textContent.includes('claude-haiku-4-5-20251001')));

  // Switch back to quota tab: enabled account card exposes wake button, schedule exclude toggle and inline status strip.
  d.api.setTab('quota');
  const cards = visibleAccounts(d);
  const claudeCard = cards.find(c => c.dataset.authId === 'claude-a');
  const disabledCard = cards.find(c => c.dataset.authId === 'codex-b');
  const wakeBtn = descendants(claudeCard).find(n => n.className === 'button account-action account-wakeup');
  const toggleBtn = descendants(claudeCard).find(n => n.className === 'button account-action account-wakeup-toggle');
  const statusStrip = descendants(claudeCard).find(n => String(n.className || '').startsWith('account-wakeup-status'));
  assert(wakeBtn, 'enabled card should have Wake button');
  assert(toggleBtn, 'enabled card should show Auto toggle when schedule is enabled');
  assert(statusStrip && statusStrip.hidden === false && statusStrip.textContent.includes('claude-haiku-4-5-20251001'));
  assert.equal(descendants(disabledCard).find(n => n.className === 'button account-action account-wakeup'), undefined);

  // Toggle exclusion for claude-a.
  await toggleBtn.onclick();
  const excludeCall = calls.find(c => c.url.endsWith('/wakeup/exclude'));
  assert(excludeCall);
  assert.deepEqual(JSON.parse(excludeCall.options.body), { auth_id: 'claude-a', excluded: true });

  // Single-account wake button sends force=true and reports synced quota note on the current card.
  await d.api.runAccountWakeup('claude-a');
  const singleRun = calls.filter(c => c.url.endsWith('/wakeup/run')).at(-1);
  assert.deepEqual(JSON.parse(singleRun.options.body), { auth_id: 'claude-a', force: true });
  const latestClaudeCard = visibleAccounts(d).find(c => c.dataset.authId === 'claude-a');
  const noteNode = descendants(latestClaudeCard).find(n => n.className === 'account-note');
  assert(noteNode && noteNode.textContent.includes('额度已同步'));

  // Language switch translates wakeup tab badge and section headings.
  d.api.setLanguage('en', false);
  assert.match(d.elements.get('wakeup-tab-badge').textContent, /^Next \d{2}:\d{2}$/);
  assert.equal(d.elements.get('tab-wakeup').children[0].textContent, 'Scheduled wakeup');
});

test('wakeup lists enabled Antigravity accounts and sends the selected Gemini 4', async () => {
  const enabled={auth_id:'ag-on',provider:'antigravity',name:'enabled@example.test'};
  const disabled={auth_id:'ag-off',provider:'antigravity',name:'disabled@example.test',disabled:true};
  const extra={auth_id:'ag-extra',provider:'antigravity',name:'extra@example.test'};
  const model='gemini-4-argon-high',calls=[];
  let accounts=[disabled],results={},finished='';
  let config={mode:'daily',cron:'0 6,11,16,21 * * *',providers:{claude:true,codex:true,antigravity:true},models:{'antigravity:gemini':model}};
  const snapshot=()=>({quotas:accounts,wakeup:{config,results,last_run_finished_at:finished}});
  const d=dashboard(login(),async (url,options)=>{
    calls.push({url,options});
    if(url.includes('/auth-files/models?')){
      return reply({models:(url.endsWith('ag-on')?[model,'gemini-4-carbon-high','gemini-3.5-flash-lite','gemini-4-image','claude-sonnet-4-6']:['gemini-4-neon-xhigh',model]).map(id=>({id}))});
    }
    if(url.endsWith('/wakeup/config')){config=JSON.parse(options.body);return reply({wakeup:snapshot().wakeup});}
    if(url.endsWith('/wakeup/run')){
      finished='2026-11-25T06:00:03Z';
      results={'ag-on':{...enabled,status:'sent',model:config.models['antigravity:gemini'],at:finished,groups:[{group:'gemini',status:'sent',model:config.models['antigravity:gemini']}]}};
      return reply({account_result_at:''},202);
    }
    return reply(snapshot());
  });
  await d.api.load();d.api.setTab('wakeup');await new Promise(resolve=>setImmediate(resolve));
  assert.match(d.elements.get('wakeup-scope-text').textContent,/暂无已启用账户/);
  const select=d.elements.get('wakeup-model-select-ag-gemini');
  assert.equal(select.value,'__custom__');
  d.elements.get('wakeup-text').value='hello';d.elements.get('wakeup-text').oninput();
  accounts=[disabled,enabled];await d.elements.get('refresh').onclick();
  assert.equal(select.value,model);
  assert.equal(d.elements.get('wakeup-model-ag-gemini').hidden,true);
  const rows=d.elements.get('wakeup-accounts-list').children;
  assert.equal(rows.length,1);
  assert(descendants(rows[0]).some(node=>node.textContent===enabled.name));
  const testButton=descendants(rows[0]).find(node=>node.className==='button wakeup-account-test');
  assert.equal(testButton.disabled,false);
  assert.deepEqual(select.children.slice(1,-1).map(node=>node.value),['gemini-3.5-flash-lite',model,'gemini-4-carbon-high']);
  await testButton.onclick();
  const mutations=calls.filter(c=>c.options.method==='POST');
  assert.deepEqual(mutations.map(c=>c.url.split('/').at(-1)),['config','run']);
  assert(!calls.some(c=>c.url.endsWith('/wakeup/accounts')),'wakeup must use the existing snapshot roster');
  assert.equal(JSON.parse(mutations[0].options.body).models['antigravity:gemini'],model);
  assert.deepEqual(JSON.parse(mutations[1].options.body),{auth_id:'ag-on',force:true});
  assert.match(d.elements.get('wakeup-message-box').textContent,/尚未确认 5 小时窗口/);
  accounts=[disabled,enabled,extra];await d.elements.get('refresh').onclick();
  assert.equal(select.children.filter(node=>node.value===model).length,1);
  assert(select.children.some(node=>node.value==='gemini-4-neon-xhigh'));
  assert(!calls.some(c=>c.url.includes('name=ag-off')),'disabled account catalog was queried');
});

test('Google Antigravity wakeup errors never display reset-credit disabled copy and surface CPA TOS auto-disable badge', async () => {
  const sample = {
    quotas: [
      {
        auth_id: 'ag-tos',
        provider: 'antigravity',
        name: 'google-tos@example.com',
        disabled: true,
        disable_reason: 'tos_violation',
        groups: [{ key: 'gemini', label: 'Gemini', five_hour: { used_percent: 0 } }],
      },
      {
        auth_id: 'ag-manual',
        provider: 'antigravity',
        name: 'google-manual@example.com',
        disabled: true,
        groups: [{ key: 'gemini', label: 'Gemini', five_hour: { used_percent: 0 } }],
      },
    ],
    wakeup: {
      config: { enabled: true, mode: 'daily', cron: '0 6,11,16,21 * * *', timezone_offset_min: 480, skip_active_window: true, providers: { claude: true, codex: true, antigravity: true }, models: {}, excluded_auth_ids: [], message: 'hi' },
      results: {
        'ag-tos': { auth_id: 'ag-tos', provider: 'antigravity', name: 'google-tos@example.com', status: 'failed', reason: 'tos_violation', at: '2026-11-25T06:00:00Z' },
        'ag-manual': { auth_id: 'ag-manual', provider: 'antigravity', name: 'google-manual@example.com', status: 'failed', reason: 'wakeup_account_disabled', at: '2026-11-25T06:00:01Z' },
      },
    },
  };
  const d = dashboard({ managementKey: 'key' }, async () => ({ ok: true, status: 200, json: async () => sample }));
  await d.api.load();

  const tosCard = visibleAccounts(d).find(c => c.dataset.authId === 'ag-tos');
  const manualCard = visibleAccounts(d).find(c => c.dataset.authId === 'ag-manual');
  const tosBadge = descendants(tosCard).find(n => n.className === 'account-state tos');
  const manualBadge = descendants(manualCard).find(n => n.className === 'account-state');
  assert(tosBadge && tosBadge.textContent === 'CPA 自动停用 (TOS)');
  assert(tosBadge.title.includes('TOS_VIOLATION'));
  assert(manualBadge && manualBadge.textContent === '已禁用');

  const tosStatus = descendants(tosCard).find(n => String(n.className || '').startsWith('account-wakeup-status'));
  const manualStatus = descendants(manualCard).find(n => String(n.className || '').startsWith('account-wakeup-status'));
  assert(tosStatus && tosStatus.textContent.includes('TOS_VIOLATION'));
  assert(!tosStatus.textContent.includes('重置卡'));
  assert(manualStatus && manualStatus.textContent.includes('该账户已被禁用，无法唤醒'));
  assert(!manualStatus.textContent.includes('重置卡'));

  d.api.acceptSnapshot({...sample,
    quotas:sample.quotas.map(q=>({...q,disabled:false,disable_reason:undefined})),
    wakeup:{...sample.wakeup,results:Object.fromEntries(Object.entries(sample.wakeup.results).map(([id,res])=>[id,{
      ...res,status:'sent',reason:undefined,groups:[{group:'gemini',status:'sent',model:'gemini-3.5-flash-lite',window_reset_at:id==='ag-tos'?'2026-11-25T11:00:00Z':undefined}],
    }]))},
  });
  const groupStatus=id=>descendants(panel(d.elements,id)).find(n=>String(n.className || '').startsWith('account-wakeup-status'));
  assert.match(groupStatus('ag-tos').textContent,/5 小时窗口已确认.*重置时间/);
  assert.equal(groupStatus('ag-manual').className,'account-wakeup-status unconfirmed');
  assert.match(groupStatus('ag-manual').textContent,/窗口待确认/);
});

test('wakeup defaults unpersisted timezone to browser, flags timezone mismatch, and shows window notes in recent results', async () => {
  const localMin = (-new Date().getTimezoneOffset()) || 0;
  const otherMin = localMin === 480 ? -480 : 480;
  const sample = (persisted) => ({
    quotas: [
      { auth_id: 'cx-ok', provider: 'codex', name: 'ok@example.com', five_hour: { used_percent: 1 } },
      { auth_id: 'cx-run', provider: 'codex', name: 'run@example.com', five_hour: { used_percent: 30 } },
      { auth_id: 'cl-retry', provider: 'claude', name: 'retry@example.com', five_hour: { used_percent: 0 } },
    ],
    wakeup: {
      persisted,
      config: { enabled: true, mode: 'daily', cron: '0 6,11,16,21 * * *', timezone_offset_min: otherMin, providers: { claude: true, codex: true, antigravity: true }, models: {}, excluded_auth_ids: [], message: 'hi' },
      results: {
        'cx-ok': { auth_id: 'cx-ok', provider: 'codex', name: 'ok@example.com', status: 'sent', model: 'gpt-6-luna', at: '2026-11-26T03:14:55Z', window_reset_at: '2026-11-26T08:14:55Z' },
        'cx-run': { auth_id: 'cx-run', provider: 'codex', name: 'run@example.com', status: 'sent', model: 'gpt-6-luna', at: '2026-11-26T03:14:55Z', window_reset_at: '2026-11-26T06:14:08Z', window_existing: true },
        'cl-retry': { auth_id: 'cl-retry', provider: 'claude', name: 'retry@example.com', status: 'sent', model: 'claude-haiku-5-5', at: '2026-11-26T03:14:55Z', retry_at: '2026-11-26T03:16:55Z' },
      },
    },
  });

  // Unpersisted config defaults to browser local timezone without mismatch warning.
  const d = dashboard({ managementKey: 'key' }, async () => reply(sample(false)));
  await d.api.load();
  d.api.setTab('wakeup');
  assert.equal(Number(d.elements.get('wakeup-tz').value), localMin);
  assert.equal(descendants(d.elements.get('wakeup-cron-preview')).some(n => n.className === 'wakeup-preview-tz-warn'), false);

  // Persisted config keeps saved timezone and warns clearly when it differs from browser timezone.
  d.api.acceptSnapshot(sample(true));
  assert.equal(Number(d.elements.get('wakeup-tz').value), otherMin);
  const warn = descendants(d.elements.get('wakeup-cron-preview')).find(n => n.className === 'wakeup-preview-tz-warn');
  assert(warn && warn.textContent.includes('与本机') && warn.textContent.includes('下次即本机'));

  // Recent wakeup records list displays window confirmation, running-window and retry details inline.
  const metas = descendants(d.elements.get('wakeup-results')).filter(n => n.className === 'wakeup-result-meta').map(n => n.textContent);
  assert(metas.some(s => s.includes('5 小时窗口已确认，重置时间：')), `expected confirmed window note: ${metas}`);
  assert(metas.some(s => s.includes('已有 5 小时窗口进行中，重置时间：')), `expected running window note: ${metas}`);
  assert(metas.some(s => s.includes('新窗口未开启') && s.includes('自动重试')), `expected retry note: ${metas}`);
});

test('wakeup: Antigravity existing windows never read as unconfirmed, and exhausted near-reset retries are shown inline', async () => {
  const sample = {
    quotas: [
      { auth_id: 'ag-existing', provider: 'antigravity', name: 'ag-existing@example.com', groups: [{ key: 'gemini', label: 'Gemini', five_hour: { used_percent: 0 } }] },
      { auth_id: 'ag-legacy', provider: 'antigravity', name: 'ag-legacy@example.com', groups: [{ key: 'gemini', label: 'Gemini', five_hour: { used_percent: 40 } }] },
      { auth_id: 'ag-new', provider: 'antigravity', name: 'ag-new@example.com', groups: [{ key: 'gemini', label: 'Gemini', five_hour: { used_percent: 1 } }] },
      { auth_id: 'cx-soon', provider: 'codex', name: 'cx-soon@example.com', five_hour: { used_percent: 100 } },
      { auth_id: 'ag-soon', provider: 'antigravity', name: 'ag-soon@example.com', groups: [{ key: 'gemini', label: 'Gemini', five_hour: { used_percent: 100 } }] },
    ],
    wakeup: {
      persisted: true,
      config: { enabled: true, mode: 'daily', cron: '0 6,11,16,21 * * *', timezone_offset_min: 480, providers: { claude: true, codex: true, antigravity: true }, models: {}, excluded_auth_ids: [], message: 'hi' },
      results: {
        // New contract: account-level window_reset_at + window_existing; the Gemini group may lack its own reset time.
        'ag-existing': { auth_id: 'ag-existing', provider: 'antigravity', name: 'ag-existing@example.com', status: 'sent', at: '2026-11-26T03:00:05Z', window_reset_at: '2026-11-26T05:20:00Z', window_existing: true, groups: [{ group: 'gemini', status: 'sent', model: 'gemini-3.5-flash-lite' }] },
        // Legacy record: only the Gemini group reset time plus an account retry_at.
        'ag-legacy': { auth_id: 'ag-legacy', provider: 'antigravity', name: 'ag-legacy@example.com', status: 'sent', at: '2026-11-26T03:00:04Z', retry_at: '2026-11-26T05:20:30Z', groups: [{ group: 'gemini', status: 'sent', model: 'gemini-3.5-flash-lite', window_reset_at: '2026-11-26T05:20:00Z' }] },
        'ag-new': { auth_id: 'ag-new', provider: 'antigravity', name: 'ag-new@example.com', status: 'sent', at: '2026-11-26T03:00:03Z', window_reset_at: '2026-11-26T07:59:00Z', groups: [{ group: 'gemini', status: 'sent', model: 'gemini-3.5-flash-lite', window_reset_at: '2026-11-26T07:59:00Z' }] },
        'cx-soon': { auth_id: 'cx-soon', provider: 'codex', name: 'cx-soon@example.com', status: 'exhausted', at: '2026-11-26T03:00:02Z', retry_at: '2026-11-26T03:00:40Z' },
        'ag-soon': { auth_id: 'ag-soon', provider: 'antigravity', name: 'ag-soon@example.com', status: 'exhausted', at: '2026-11-26T03:00:01Z', retry_at: '2026-11-26T03:04:30Z', groups: [{ group: 'gemini', status: 'exhausted' }] },
      },
    },
  };
  const d = dashboard({ managementKey: 'key' }, async () => reply(sample));
  await d.api.load();
  d.api.setTab('wakeup');
  const accountStatus = id => descendants(panel(d.elements, id)).find(n => String(n.className || '').startsWith('account-wakeup-status'));
  const resultRow = id => descendants(d.elements.get('wakeup-results')).find(n => n.className === 'wakeup-result-row' && descendants(n).some(c => c.className === 'wakeup-result-name' && c.textContent.startsWith(id + '@')));
  const meta = id => descendants(resultRow(id)).find(n => n.className === 'wakeup-result-meta').textContent;
  const badge = id => descendants(resultRow(id)).find(n => String(n.className || '').startsWith('wakeup-badge '));

  for (const id of ['ag-existing', 'ag-legacy']) {
    const status = accountStatus(id);
    assert.equal(status.className, 'account-wakeup-status sent', id);
    assert.match(status.textContent, /已有 5 小时窗口进行中 · Gemini 模型: gemini-3\.5-flash-lite · 已有 5 小时窗口进行中，重置时间：/, id);
    assert.equal(badge(id).className, 'wakeup-badge sent', id);
    assert.equal(descendants(badge(id)).map(n => n.textContent).join(''), 'OK', id);
    assert.match(meta(id), /已有 5 小时窗口进行中，重置时间：/, id);
    for (const text of [status.textContent, meta(id), badge(id).title]) {
      assert(!/待确认|新窗口未开启|已确认|自动重试/.test(text), `${id} contradicts itself: ${text}`);
    }
  }
  // A freshly opened Antigravity window stays "confirmed", not "already running".
  assert.equal(accountStatus('ag-new').className, 'account-wakeup-status sent');
  assert.match(accountStatus('ag-new').textContent, /5 小时窗口已确认 · .*5 小时窗口已确认，重置时间：/);
  assert(!/已有 5 小时窗口|待确认/.test(accountStatus('ag-new').textContent));

  // Exhausted at a schedule slot with a near reset: the queued retry time is shown in both places.
  for (const id of ['cx-soon', 'ag-soon']) {
    assert.equal(accountStatus(id).className, 'account-wakeup-status exhausted', id);
    assert.match(accountStatus(id).textContent, /额度已用完 · .*即将重置，.+ 自动重试$/, id);
    assert.match(meta(id), /即将重置，.+ 自动重试/, id);
    assert.equal(badge(id).className, 'wakeup-badge exhausted', id);
    assert(!/新窗口未开启|待确认/.test(accountStatus(id).textContent), id);
  }

  d.api.setLanguage('en');
  assert.match(accountStatus('ag-existing').textContent, /5-hour window already running · .*5-hour window already running; resets at /);
  assert.match(accountStatus('ag-legacy').textContent, /5-hour window already running; resets at /);
  assert(!/unconfirmed|No new window|retrying/.test(accountStatus('ag-legacy').textContent));
  assert.match(accountStatus('cx-soon').textContent, /Resets soon, retrying .+$/);
  assert.match(meta('ag-soon'), /Resets soon, retrying /);
  assert(!/[\u4e00-\u9fff]/.test(accountStatus('cx-soon').textContent + meta('ag-soon')), 'English near-reset note must be fully translated');
});

test('wakeup: Claude/Codex sends without a confirmed reset read as pending even without retry_at', async () => {
  const cfg = { enabled: true, mode: 'daily', cron: '0 6,11,16,21 * * *', timezone_offset_min: 480, providers: { claude: true, codex: true, antigravity: true }, models: {}, excluded_auth_ids: [], message: 'hi' };
  const quota = (id, provider) => ({ auth_id: id, provider, name: id + '@example.com', five_hour: { used_percent: 0 } });
  const sent = (id, provider, extra = {}) => ({ auth_id: id, provider, name: id + '@example.com', status: 'sent', model: 'm-' + provider, at: '2026-11-26T03:00:00Z', ...extra });
  let results = {
    'cl-busy': sent('cl-busy', 'claude'),
    'cx-busy': sent('cx-busy', 'codex'),
    'cl-retry': sent('cl-retry', 'claude', { retry_at: '2026-11-26T03:02:00Z' }),
    'cx-new': sent('cx-new', 'codex', { window_reset_at: '2026-11-26T08:00:00Z' }),
    'cl-run': sent('cl-run', 'claude', { window_reset_at: '2026-11-26T05:00:00Z', window_existing: true }),
  };
  const ids = Object.keys(results);
  const snapshot = () => ({ quotas: ids.map(id => quota(id, results[id].provider)), wakeup: { persisted: true, config: cfg, results, last_run_at: '2026-11-26T03:00:00Z', last_run_finished_at: '2026-11-26T03:00:01Z' } });
  const d = dashboard(login(), async url => {
    if (url.endsWith('/wakeup/run')) {
      results = { ...results, 'cl-busy': sent('cl-busy', 'claude', { at: '2026-11-26T03:10:00Z' }) };
      return reply({ account_result_at: '' }, 202);
    }
    return reply(snapshot());
  });
  await d.api.load();
  d.api.setTab('wakeup');
  const accountStatus = id => descendants(panel(d.elements, id)).find(n => String(n.className || '').startsWith('account-wakeup-status'));
  const resultRow = id => descendants(d.elements.get('wakeup-results')).find(n => n.className === 'wakeup-result-row' && descendants(n).some(c => c.className === 'wakeup-result-name' && c.textContent.startsWith(id + '@')));
  const badge = id => descendants(resultRow(id)).find(n => String(n.className || '').startsWith('wakeup-badge '));
  const badgeText = id => descendants(badge(id)).map(n => n.textContent).join('');

  for (const id of ['cl-busy', 'cx-busy']) {
    assert.equal(accountStatus(id).className, 'account-wakeup-status unconfirmed', id);
    assert.match(accountStatus(id).textContent, /已发送，窗口待确认/, id);
    assert(!/OK|已确认|自动重试|进行中/.test(accountStatus(id).textContent), `${id}: ${accountStatus(id).textContent}`);
    assert.equal(badge(id).className, 'wakeup-badge unconfirmed', id);
    assert.equal(badgeText(id), '已发送，窗口待确认', id);
  }
  // A queued retry stays pending and still names its retry time.
  assert.equal(accountStatus('cl-retry').className, 'account-wakeup-status unconfirmed');
  assert.match(accountStatus('cl-retry').textContent, /新窗口未开启，.+ 自动重试/);
  // Confirmed new and running windows keep the plain OK.
  for (const id of ['cx-new', 'cl-run']) {
    assert.equal(accountStatus(id).className, 'account-wakeup-status sent', id);
    assert.equal(badgeText(id), 'OK', id);
  }
  assert.match(accountStatus('cx-new').textContent, /5 小时窗口已确认，重置时间：/);
  assert.match(accountStatus('cl-run').textContent, /已有 5 小时窗口进行中，重置时间：/);
  const summary = descendants(d.elements.get('wakeup-summary')).map(n => n.textContent || '').find(s => s.includes('最近执行'));
  assert.match(summary, /OK 2 · 待确认 3 ·/);

  // The single-account notice must not claim the quota was synced.
  await d.api.runAccountWakeup('cl-busy');
  const msg = d.elements.get('wakeup-message-box').textContent;
  assert.match(msg, /尚未确认 5 小时窗口/);
  assert(!/额度已同步|自动重试/.test(msg), msg);
  d.api.setLanguage('en');
  assert.match(d.elements.get('wakeup-message-box').textContent, /could not be confirmed/);
});

test('wakeup: legacy Antigravity group time plus retry_at counts as a running window only under the old reset+30s queue formula', async () => {
  const gemini = (extra = {}) => ({ group: 'gemini', status: 'sent', model: 'gemini-3.5-flash-lite', ...extra });
  const quota = id => ({ auth_id: id, provider: 'antigravity', name: id + '@example.com', groups: [{ key: 'gemini', label: 'Gemini', five_hour: { used_percent: 1 } }] });
  const ids = ['ag-stale', 'ag-edge', 'ag-late', 'ag-offtol', 'ag-after', 'ag-queued', 'ag-tol', 'ag-top', 'ag-noretry'];
  const sample = {
    quotas: ids.map(quota),
    wakeup: {
      persisted: true,
      config: { enabled: true, mode: 'daily', cron: '0 6,11,16,21 * * *', timezone_offset_min: 480, providers: { claude: true, codex: true, antigravity: true }, models: {}, excluded_auth_ids: [], message: 'hi' },
      results: {
        // v0.1.46: sent 11:00, group observed the old window resetting 11:00:10, real quick retry at 11:02.
        'ag-stale': { auth_id: 'ag-stale', provider: 'antigravity', name: 'ag-stale@example.com', status: 'sent', at: '2026-11-26T11:00:00Z', retry_at: '2026-11-26T11:02:00Z', groups: [gemini({ window_reset_at: '2026-11-26T11:00:10Z' })] },
        // Exactly at+5m is still an expiring observation.
        'ag-edge': { auth_id: 'ag-edge', provider: 'antigravity', name: 'ag-edge@example.com', status: 'sent', at: '2026-11-26T11:00:00Z', retry_at: '2026-11-26T11:05:30Z', groups: [gemini({ window_reset_at: '2026-11-26T11:05:00Z' })] },
        // Verified late (11:04:40): group 11:05:10 is beyond at+5m, but 11:06:40 is not the old running-window queue (11:05:40): a real miss.
        'ag-late': { auth_id: 'ag-late', provider: 'antigravity', name: 'ag-late@example.com', status: 'sent', at: '2026-11-26T11:00:00Z', retry_at: '2026-11-26T11:06:40Z', groups: [gemini({ window_reset_at: '2026-11-26T11:05:10Z' })] },
        // Just outside the ±2s tolerance of reset+30s.
        'ag-offtol': { auth_id: 'ag-offtol', provider: 'antigravity', name: 'ag-offtol@example.com', status: 'sent', at: '2026-11-26T21:00:00Z', retry_at: '2026-11-26T21:20:32.001Z', groups: [gemini({ window_reset_at: '2026-11-26T21:20:00Z' })] },
        // Beyond at+5m and queued at reset+30s: a legacy running window.
        'ag-queued': { auth_id: 'ag-queued', provider: 'antigravity', name: 'ag-queued@example.com', status: 'sent', at: '2026-11-26T21:00:00Z', retry_at: '2026-11-26T21:20:30Z', groups: [gemini({ window_reset_at: '2026-11-26T21:20:00Z' })] },
        'ag-tol': { auth_id: 'ag-tol', provider: 'antigravity', name: 'ag-tol@example.com', status: 'sent', at: '2026-11-26T21:00:00Z', retry_at: '2026-11-26T21:20:28Z', groups: [gemini({ window_reset_at: '2026-11-26T21:20:00Z' })] },
        'ag-after': { auth_id: 'ag-after', provider: 'antigravity', name: 'ag-after@example.com', status: 'sent', at: '2026-11-26T11:00:00Z', retry_at: '2026-11-26T11:05:31Z', groups: [gemini({ window_reset_at: '2026-11-26T11:05:01Z' })] },
        // A top-level window_reset_at is always the confirmation.
        'ag-top': { auth_id: 'ag-top', provider: 'antigravity', name: 'ag-top@example.com', status: 'sent', at: '2026-11-26T11:00:00Z', window_reset_at: '2026-11-26T11:03:00Z', window_existing: true, groups: [gemini({ window_reset_at: '2026-11-26T11:03:00Z' })] },
        // Without retry_at a near group time is still the confirmed window.
        'ag-noretry': { auth_id: 'ag-noretry', provider: 'antigravity', name: 'ag-noretry@example.com', status: 'sent', at: '2026-11-26T11:00:00Z', groups: [gemini({ window_reset_at: '2026-11-26T11:00:10Z' })] },
      },
    },
  };
  const d = dashboard({ managementKey: 'key' }, async () => reply(sample));
  await d.api.load();
  d.api.setTab('wakeup');
  const accountStatus = id => descendants(panel(d.elements, id)).find(n => String(n.className || '').startsWith('account-wakeup-status'));
  const resultRow = id => descendants(d.elements.get('wakeup-results')).find(n => n.className === 'wakeup-result-row' && descendants(n).some(c => c.className === 'wakeup-result-name' && c.textContent.startsWith(id + '@')));
  const meta = id => descendants(resultRow(id)).find(n => n.className === 'wakeup-result-meta').textContent;
  const badge = id => descendants(resultRow(id)).find(n => String(n.className || '').startsWith('wakeup-badge '));

  for (const id of ['ag-stale', 'ag-edge', 'ag-late', 'ag-offtol']) {
    assert.equal(accountStatus(id).className, 'account-wakeup-status unconfirmed', id);
    assert.equal(badge(id).className, 'wakeup-badge unconfirmed', id);
    assert.match(accountStatus(id).textContent, /已发送，窗口待确认 · Gemini 模型: gemini-3\.5-flash-lite · 已发送，窗口待确认 · 新窗口未开启，.+ 自动重试$/, id);
    assert.match(meta(id), /Gemini 模型: gemini-3\.5-flash-lite · 已发送，窗口待确认 · 新窗口未开启，.+ 自动重试/, id);
    for (const text of [accountStatus(id).textContent, meta(id), badge(id).title]) {
      assert(!/已确认|已有 5 小时窗口|重置时间/.test(text), `${id} must not claim a confirmed window: ${text}`);
    }
  }
  for (const id of ['ag-after', 'ag-queued', 'ag-tol']) {
    assert.equal(accountStatus(id).className, 'account-wakeup-status sent', id);
    assert.equal(badge(id).className, 'wakeup-badge sent', id);
    assert.match(accountStatus(id).textContent, /^.*已有 5 小时窗口进行中 · Gemini 模型: gemini-3\.5-flash-lite · 已有 5 小时窗口进行中，重置时间：[^·]+$/, id);
    assert.match(meta(id), /已有 5 小时窗口进行中，重置时间：/, id);
  }
  assert.equal(accountStatus('ag-top').className, 'account-wakeup-status sent');
  assert.match(meta('ag-top'), /已有 5 小时窗口进行中，重置时间：/);
  assert.equal(accountStatus('ag-noretry').className, 'account-wakeup-status sent');
  assert.match(accountStatus('ag-noretry').textContent, /5 小时窗口已确认 · .*5 小时窗口已确认，重置时间：/);
  for (const id of ['ag-after', 'ag-queued', 'ag-tol', 'ag-top', 'ag-noretry']) assert(!/待确认|新窗口未开启|自动重试/.test(accountStatus(id).textContent + meta(id)), id);

  d.api.setLanguage('en');
  assert.match(accountStatus('ag-stale').textContent, /Sent, window unconfirmed · .*Sent, window unconfirmed · No new window yet, retrying .+$/);
  assert(!/confirmed;|already running/.test(accountStatus('ag-stale').textContent));
  assert.match(accountStatus('ag-late').textContent, /Sent, window unconfirmed · .*Sent, window unconfirmed · No new window yet, retrying .+$/);
  assert(!/confirmed;|already running/.test(accountStatus('ag-late').textContent));
  assert.match(accountStatus('ag-queued').textContent, /5-hour window already running · .*5-hour window already running; resets at /);
  assert(!/unconfirmed|No new window|retrying/.test(accountStatus('ag-queued').textContent));
});

test('Claude quota 429 error copy and 30-minute footer cadence render in zh and en', async () => {
  const sample = {
    quotas: [
      {
        auth_id: 'cl-429',
        provider: 'claude',
        name: 'cl@example.com',
        error: 'Quota endpoint rate limited',
        five_hour: { used_percent: 12, reset_at: '2026-11-26T17:00:00Z' },
      },
    ],
  };
  const d = dashboard({ managementKey: 'key' }, async () => reply(sample));
  await d.api.load();
  const cardText = () => descendants(panel(d.elements, 'cl-429')).map(n => n.textContent).join(' ');
  assert.match(cardText(), /额度查询触发上游限流（429），已自动退避，不影响模型正常使用。/);
  assert.match(d.elements.get('footer').textContent, /每 15 分钟检查（Claude 30 分钟）/);

  d.api.setLanguage('en');
  assert.match(cardText(), /Quota endpoint rate limited \(429\); backing off automatically while models remain usable\./);
  assert.match(d.elements.get('footer').textContent, /every 15 min \(Claude 30 min\)/);
});

// Routing cooldown: consume-reset feedback, legacy CPA management fallback and the manual clear action.
const coolBadge=card=>descendants(card).find(n=>n.className==='account-state cooling');
const clearButton=card=>descendants(card).find(n=>n.className==='button account-action account-clear-cooldown');
const flush=async(times=5)=>{for(let i=0;i<times;i++)await new Promise(r=>setImmediate(r));};
const coolRetry=new Date(Date.now()+2*hour).toISOString();
function coolAccounts(cooling=true,retry=coolRetry){
  const s=actionAccounts();
  if(cooling){Object.assign(s.quotas[0],{routing_cooling:true,next_retry_after:retry});Object.assign(s.quotas[1],{routing_cooling:true});}
  Object.assign(s.quotas[2],{routing_cooling:true,next_retry_after:retry}); // Disabled accounts never show the cooldown UI.
  return s;
}
function coolFetcher(calls,routes={},state={cooling:true}){
  return async(url,options)=>{calls.push({url,options});
    for(const [suffix,handler] of Object.entries(routes))if(url.endsWith(suffix))return handler(options,url);
    if(url.endsWith('reset-credits'))return reply(creditList());
    if(url.endsWith('consume-reset'))return reply({code:'reset',windows_reset:2,cooldown_cleared:true});
    if(url.endsWith('refresh-account'))return reply({refresh_finished_at:'before'},202);
    return reply({...coolAccounts(state.cooling),account_refresh:{'codex-a':{refreshing:false,finished_at:'after'}}});
  };
}
const legacyFiles=()=>reply({files:[{id:'codex-zero',auth_index:'idx-zero'},{id:'codex-a',auth_index:'idx-a'}]});
const legacyCalls=calls=>calls.filter(c=>/\/v0\/management\/auth-files$|\/v8\/management\/routing\/cooldown\/reset$|\/v0\/management\/reset-quota$/.test(c.url));
const cooldownFailed='已使用重置卡，但未能清除 CPA 路由冷却。';
const cooldownHint='可稍后点击“清除冷却”重试，或等待冷却自然结束。';
const legacyFailed='当前 CPA 版本不支持清除路由冷却，管理接口兜底也未成功。';

for(const language of ['zh','en'])test(`${language}: cooling accounts show a routing-cooldown badge with retry hint and a clear button; disabled accounts do not`,()=>{
  const d=dashboard({},undefined,{language});d.api.renderSample(coolAccounts());
  const a=panel(d.elements,'codex-a'),zero=panel(d.elements,'codex-zero'),off=panel(d.elements,'codex-off'),claude=panel(d.elements,'claude-a');
  assert.equal(coolBadge(a).textContent,language==='en'?'Routing cooldown':'路由冷却中');
  const time=new Date(coolRetry).toLocaleString(language==='en'?'en-US':'zh-CN',{hour12:false});
  assert.equal(coolBadge(a).title,language==='en'?`CPA has paused routing to this account until about ${time}; click Clear cooldown to restore it now.`:`CPA 暂停向此账户路由请求，预计 ${time} 恢复；可点击“清除冷却”立即恢复。`);
  assert.equal(coolBadge(zero).title,language==='en'?'CPA has paused routing to this account; click Clear cooldown to restore it now.':'CPA 暂停向此账户路由请求；可点击“清除冷却”立即恢复。','no retry time, no fabricated estimate');
  assert.equal(clearButton(a).id,'account-clear-cooldown-codex-a');assert.equal(clearButton(a).textContent,language==='en'?'Clear cooldown':'清除冷却');
  assert.equal(clearButton(a).getAttribute('aria-label'),language==='en'?'Clear the CPA routing cooldown for alice@example.com':'清除 alice@example.com 的 CPA 路由冷却');assert.equal(clearButton(a).disabled,false);
  assert(clearButton(zero));assert(!coolBadge(off)&&!clearButton(off),'disabled accounts never show cooldown UI');assert(!coolBadge(claude)&&!clearButton(claude));
  assert(descendants(off).some(n=>n.className==='account-state'),'the disabled badge is unchanged');
  if(language==='en')for(const node of descendants(d.elements.get('quota-cards'))){assert(!/[\u3400-\u9fff]/.test(node.textContent),node.textContent);assert(!/[\u3400-\u9fff]/.test(node.title||''),node.title);for(const v of Object.values(node.attributes))assert(!/[\u3400-\u9fff]/.test(v),v);}
});

test('routing_cooling and next_retry_after changes re-render only the affected card',async()=>{
  const d=dashboard(login(),async()=>reply(coolAccounts(false)));await d.api.load();
  let a=panel(d.elements,'codex-a');const zero=panel(d.elements,'codex-zero');assert(!coolBadge(a)&&!clearButton(a));
  d.api.acceptSnapshot(coolAccounts(true));assert.notEqual(panel(d.elements,'codex-a'),a);a=panel(d.elements,'codex-a');assert(coolBadge(a)&&clearButton(a));
  const s=coolAccounts(true,new Date(Date.now()+5*hour).toISOString());s.quotas[1]=coolAccounts(true).quotas[1];
  d.api.acceptSnapshot(s);assert.notEqual(panel(d.elements,'codex-a'),a,'a new retry time re-renders the card');assert.equal(panel(d.elements,'codex-zero')===zero,false,'zero changed from not cooling to cooling');
  const z=panel(d.elements,'codex-zero');d.api.acceptSnapshot(coolAccounts(false));assert(!coolBadge(panel(d.elements,'codex-a')));assert.notEqual(panel(d.elements,'codex-zero'),z);
  const stable=panel(d.elements,'claude-a');d.api.acceptSnapshot(coolAccounts(false));assert.equal(panel(d.elements,'claude-a'),stable,'unchanged cards keep their nodes');
});

for(const language of ['zh','en'])test(`${language}: reset with cooldown_cleared=true says so and optimistically clears the cooling badge`,async()=>{
  const calls=[],state={cooling:true};let release;
  const d=dashboard(login(),coolFetcher(calls,{'refresh-account':()=>new Promise(r=>release=()=>r(reply({refresh_finished_at:'before'},202)))},state),{language});
  await d.api.load();assert(coolBadge(panel(d.elements,'codex-a')));
  await d.api.openReset('codex-a');const pending=d.api.confirmReset();await flush();
  const a=panel(d.elements,'codex-a');assert(!coolBadge(a),'cleared before the follow-up refresh');assert(!clearButton(a));assert(coolBadge(panel(d.elements,'codex-zero')),'other cards keep their state');
  const want=language==='en'?'Reset credit used; 2 quota window(s) reset. CPA routing cooldown cleared; the account is back in rotation.':'已使用重置卡，重置了 2 个额度窗口。已清除 CPA 路由冷却，账户将重新参与路由。';
  assert.equal(accountNoteNode(a).textContent,want);assert.equal(legacyCalls(calls).length,0,'no fallback when the backend cleared it');
  state.cooling=false;release();await pending;
  assert.equal(accountNoteNode(panel(d.elements,'codex-a')).textContent,want);assert.equal(accountError(panel(d.elements,'codex-a')).textContent,'');assert(!coolBadge(panel(d.elements,'codex-a')));
});

test('already_redeemed also reports the cleared cooldown; other codes and old backends keep their original copy',async()=>{
  const cases=[
    [{code:'already_redeemed',windows_reset:0,cooldown_cleared:true},'该重置请求已处理，未重复扣除。已清除 CPA 路由冷却，账户将重新参与路由。'],
    [{code:'reset',windows_reset:2},'已使用重置卡，重置了 2 个额度窗口。'],
    [{code:'already_redeemed',windows_reset:0},'该重置请求已处理，未重复扣除。'],
    [{code:'reset',windows_reset:2,cooldown_cleared:'yes',cooldown_error:'host_unsupported'},'已使用重置卡，重置了 2 个额度窗口。'],
    [{code:'nothing_to_reset',windows_reset:0,cooldown_cleared:true},'当前没有需要重置的额度窗口。'],
    [{code:'nothing_to_reset',windows_reset:0,cooldown_cleared:false,cooldown_error:'host_unsupported'},'当前没有需要重置的额度窗口。'],
  ];
  for(const [result,want] of cases){
    const calls=[],d=dashboard(login(),coolFetcher(calls,{'consume-reset':()=>reply(result)},{cooling:true}));
    await d.api.load();await useReset(d);const a=panel(d.elements,'codex-a');
    assert.equal(accountNoteNode(a).textContent,want,JSON.stringify(result));assert.equal(accountError(a).textContent,'',JSON.stringify(result));
    assert.equal(legacyCalls(calls).length,0);assert(coolBadge(a),'without cooldown_cleared=true the live state is left to the snapshot');
  }
});

test('reset host_unsupported falls back to CPA management API: auth-files -> v8 cooldown reset -> v0 reset-quota on 404/405',async()=>{
  for(const [v8,quota,cleared] of [[200,null,true],[404,200,true],[405,200,true],[404,404,false],[405,500,false],[500,null,false],[409,null,false]]){
    const calls=[],state={cooling:true};
    const d=dashboard(login(),coolFetcher(calls,{
      'consume-reset':()=>reply({code:'reset',windows_reset:2,cooldown_cleared:false,cooldown_error:'host_unsupported'}),
      '/v0/management/auth-files':legacyFiles,
      '/v8/management/routing/cooldown/reset':()=>reply(v8===200?{status:'ok'}:{error:'access_token=secret upstream detail'},v8),
      '/v0/management/reset-quota':()=>reply(quota===200?{status:'ok'}:{error:'secret-token raw'},quota),
    },state));
    await d.api.load();await useReset(d);
    const legacy=legacyCalls(calls),label=`v8=${v8} quota=${quota}`;
    assert.equal(legacy[0].url,'/v0/management/auth-files',label);assert.equal(legacy[0].options.method,'GET');assert.equal(legacy[0].options.headers.Authorization,'Bearer key');
    assert.equal(legacy[1].url,'/v8/management/routing/cooldown/reset',label);assert.equal(legacy[1].options.method,'POST');assert.deepEqual(JSON.parse(legacy[1].options.body),{auth_index:'idx-a'});assert.equal(legacy[1].options.headers.Authorization,'Bearer key');
    if(quota===null)assert.equal(legacy.length,2,label+': only 404/405 fall back to reset-quota');
    else{assert.equal(legacy.length,3,label);assert.equal(legacy[2].url,'/v0/management/reset-quota');assert.equal(legacy[2].options.method,'POST');assert.deepEqual(JSON.parse(legacy[2].options.body),{auth_index:'idx-a'});}
    const a=panel(d.elements,'codex-a'),text=accountNoteNode(a).textContent+' '+accountError(a).textContent;
    assert(!/secret|upstream detail/.test(text),label+': upstream text never leaks');
    if(cleared){assert.equal(accountNoteNode(a).textContent,'已使用重置卡，重置了 2 个额度窗口。已清除 CPA 路由冷却，账户将重新参与路由。',label);assert.equal(accountError(a).textContent,'',label);}
    else{
      assert.equal(accountNoteNode(a).textContent,'已使用重置卡，重置了 2 个额度窗口。',label+': never claims the cooldown was cleared');
      assert.equal(accountError(a).textContent,cooldownFailed+legacyFailed+cooldownHint,label+': the warning survives the follow-up refresh');
      assert(coolBadge(a),label);
    }
    assert.equal(posted(calls,'refresh-account').length,1,label);
  }
});

test('reset fallback fails closed when auth_index cannot be resolved, and other cooldown errors skip the fallback',async()=>{
  for(const files of [reply({files:[{id:'codex-zero',auth_index:'idx-zero'}]}),reply({files:[{id:'codex-a',auth_index:''}]}),reply({files:[{id:'codex-a',auth_index:'x'},{id:'codex-a',auth_index:'y'}]}),reply({error:'secret'},500),reply({files:'bad'})]){
    const calls=[],d=dashboard(login(),coolFetcher(calls,{'consume-reset':()=>reply({code:'reset',windows_reset:2,cooldown_cleared:false,cooldown_error:'host_unsupported'}),'/v0/management/auth-files':()=>files}));
    await d.api.load();await useReset(d);const a=panel(d.elements,'codex-a');
    assert.equal(legacyCalls(calls).length,1,'no cooldown write without a unique auth_index');
    assert.equal(accountError(a).textContent,cooldownFailed+legacyFailed+cooldownHint);assert.equal(accountNoteNode(a).textContent,'已使用重置卡，重置了 2 个额度窗口。');
  }
  for(const [code,reason] of [['auth_not_found','CPA 中未找到该账户的路由记录。'],['cooldown_reset_failed',''],['access_token=secret raw','']]){
    const calls=[],d=dashboard(login(),coolFetcher(calls,{'consume-reset':()=>reply({code:'already_redeemed',windows_reset:0,cooldown_cleared:false,cooldown_error:code})}));
    await d.api.load();await useReset(d);const a=panel(d.elements,'codex-a');
    assert.equal(legacyCalls(calls).length,0,code);assert.equal(accountError(a).textContent,cooldownFailed+reason+cooldownHint,code);
    assert.equal(accountNoteNode(a).textContent,'该重置请求已处理，未重复扣除。');assert(!/secret/.test(accountError(a).textContent));
  }
  const d=dashboard(login(),coolFetcher([],{'consume-reset':()=>reply({code:'reset',windows_reset:2,cooldown_cleared:false,cooldown_error:'cooldown_reset_failed'})}),{language:'en'});
  await d.api.load();await useReset(d);
  assert.equal(accountError(panel(d.elements,'codex-a')).textContent,'The reset credit was used, but the CPA routing cooldown could not be cleared. Click Clear cooldown later to retry, or wait for the cooldown to expire.');
});

test('reset fallback stops on logout or credential change and returns to sign-in on management auth failure',async()=>{
  for(const nextKey of ['',JSON.stringify('new-key')]){
    const stored=login(),calls=[];let release;
    const d=dashboard(stored,coolFetcher(calls,{'consume-reset':()=>reply({code:'reset',windows_reset:2,cooldown_cleared:false,cooldown_error:'host_unsupported'}),'/v0/management/auth-files':()=>new Promise(r=>release=()=>r(legacyFiles()))}));
    await d.api.load();await d.api.openReset('codex-a');const pending=d.api.confirmReset();await flush();
    stored.managementKey=nextKey;release();await pending;
    assert.equal(posted(calls,'/v8/management/routing/cooldown/reset').length,0,'no cooldown write with replaced credentials');assert.equal(posted(calls,'refresh-account').length,0);
    const card=panel(d.elements,'codex-a');if(card){assert.equal(accountNoteNode(card).textContent,'');assert.equal(accountError(card).textContent,'');}else assert(d.elements.get('dashboard').classes.has('needs-login'));
  }
  const d=dashboard(login(),coolFetcher([],{'consume-reset':()=>reply({code:'reset',windows_reset:2,cooldown_cleared:false,cooldown_error:'host_unsupported'}),'/v0/management/auth-files':()=>reply({},401)}));
  await d.api.load();await useReset(d);assert(d.elements.get('dashboard').classes.has('needs-login'));assert(!panel(d.elements,'codex-a'));
});

test('manual clear cooldown posts once, locks only its card, then clears the badge and refreshes',async()=>{
  const calls=[],state={cooling:true};let release;
  const d=dashboard(login(),coolFetcher(calls,{'clear-cooldown':()=>new Promise(r=>release=()=>r(reply({status:'ok',auth_id:'codex-a',cooldown_cleared:true})))},state));
  await d.api.load();const dataReads=()=>calls.filter(c=>c.url.endsWith('/data')).length,before=dataReads();
  const pending=clearButton(panel(d.elements,'codex-a')).onclick();
  let a=panel(d.elements,'codex-a'),button=clearButton(a);
  assert.equal(button.textContent,'正在清除…');assert.equal(button.disabled,true);assert.equal(button.getAttribute('aria-busy'),'true');
  assert.equal(resetButton(a).disabled,true);assert.equal(refreshButton(a).disabled,true);assert.equal(toggleButton(a).disabled,true);
  assert.equal(toggleButton(a).children[1].textContent,'已启用','a cooldown job is not shown as a status change');assert.equal(toggleButton(a).getAttribute('aria-busy'),'false');
  const zero=panel(d.elements,'codex-zero');assert.equal(clearButton(zero).disabled,false);assert.equal(refreshButton(zero).disabled,false);
  await d.api.clearCooldown('codex-a');await d.api.refreshAccount('codex-a');await d.api.openReset('codex-a');await d.api.toggleAccount('codex-a',true);await d.api.load();
  assert.equal(posted(calls,'clear-cooldown').length,1);assert.equal(posted(calls,'refresh-account').length,0);assert.equal(posted(calls,'reset-credits').length,0);assert.equal(d.elements.get('disable-dialog').hidden,true);assert.equal(dataReads(),before);
  const post=posted(calls,'clear-cooldown')[0];assert.equal(post.url,'/v0/management/plugins/cpa-mini-usage/clear-cooldown');assert.equal(post.options.method,'POST');assert.deepEqual(JSON.parse(post.options.body),{auth_id:'codex-a'});assert.equal(post.options.headers.Authorization,'Bearer key');
  state.cooling=false;release();await pending;
  a=panel(d.elements,'codex-a');assert(!coolBadge(a)&&!clearButton(a));assert.equal(accountNoteNode(a).textContent,'已清除 CPA 路由冷却，账户将重新参与路由。');assert.equal(accountError(a).textContent,'');
  assert.equal(dataReads(),before+1,'a follow-up snapshot confirms the live state');assert.equal(legacyCalls(calls).length,0);
  assert.equal(resetButton(a).disabled,false);assert.equal(toggleButton(a).disabled,false);
  await d.api.clearCooldown('codex-a');assert.equal(posted(calls,'clear-cooldown').length,1,'no cooling state, no request');
  await d.api.clearCooldown('codex-off');await d.api.clearCooldown('missing');assert.equal(posted(calls,'clear-cooldown').length,1,'disabled or unknown accounts make no request');
});

for(const language of ['zh','en'])test(`${language}: manual clear cooldown errors use fixed safe copy and keep the cooling state`,async()=>{
  const cases=[
    [400,{error:'invalid_auth_id'},'无效的账户标识。','Invalid account ID.'],
    [404,{error:'account_not_found'},'未找到账户。',null],
    [409,{error:'account_disabled'},'账户已禁用，无需清除路由冷却。','The account is disabled; there is no routing cooldown to clear.'],
    [409,{error:'cooldown_busy'},'冷却清除请求正在处理中，请稍后重试。','A cooldown clear is already in progress. Please retry shortly.'],
    [502,{error:'cooldown_reset_failed'},'CPA 清除路由冷却失败，请稍后重试。','CPA failed to clear the routing cooldown. Please retry later.'],
    [502,{error:'host callbacks unavailable'},'主机接口不可用。','Host callbacks unavailable.'],
    [500,{error:'access_token=secret raw'},'清除路由冷却失败（HTTP 500），请稍后重试。','Clearing the routing cooldown failed (HTTP 500). Please retry later.'],
    [200,{status:'ok'},'清除冷却响应格式异常。','The cooldown clear response was not recognized.'],
  ];
  for(const [status,body,zh,en] of cases){
    const calls=[],d=dashboard(login(),coolFetcher(calls,{'clear-cooldown':()=>reply(body,status)}),{language});
    await d.api.load();const before=calls.length;await clearButton(panel(d.elements,'codex-a')).onclick();
    const a=panel(d.elements,'codex-a'),text=accountError(a).textContent;
    if(language==='zh')assert.equal(text,zh,JSON.stringify(body));else if(en)assert.equal(text,en,JSON.stringify(body));else assert(text&&!/[\u3400-\u9fff]/.test(text));
    assert(!/secret/.test(text));assert.equal(accountNoteNode(a).textContent,'');assert(coolBadge(a),'never optimistically cleared on failure');assert.equal(clearButton(a).disabled,false);
    assert.deepEqual(calls.slice(before).map(c=>c.url.split('/').pop()),['clear-cooldown'],'no fallback and no refresh after a definitive failure');
  }
});

test('manual clear on 501 host_unsupported uses the same management API fallback',async()=>{
  for(const [v8,quota,ok] of [[200,null,true],[404,200,true],[404,404,false]]){
    const calls=[],state={cooling:true};
    const d=dashboard(login(),coolFetcher(calls,{'clear-cooldown':()=>reply({error:'host_unsupported'},501),'/v0/management/auth-files':legacyFiles,
      '/v8/management/routing/cooldown/reset':()=>{if(v8===200)state.cooling=false;return reply({error:'secret'},v8);},'/v0/management/reset-quota':()=>{if(quota===200)state.cooling=false;return reply({error:'secret'},quota);}},state));
    await d.api.load();await d.api.clearCooldown('codex-a');
    const legacy=legacyCalls(calls).map(c=>c.url),a=panel(d.elements,'codex-a');
    assert.deepEqual(legacy,['/v0/management/auth-files','/v8/management/routing/cooldown/reset',...(quota?['/v0/management/reset-quota']:[])]);
    for(const c of legacyCalls(calls).slice(1))assert.deepEqual(JSON.parse(c.options.body),{auth_index:'idx-a'});
    if(ok){assert.equal(accountNoteNode(a).textContent,'已清除 CPA 路由冷却，账户将重新参与路由。');assert(!coolBadge(a));}
    else{assert.equal(accountError(a).textContent,legacyFailed);assert.equal(accountNoteNode(a).textContent,'');assert(coolBadge(a));assert(!/secret/.test(accountError(a).textContent));}
  }
});

test('manual clear replies cannot cross logout or credential replacement; auth failures return to sign-in',async()=>{
  for(const status of [200,501])for(const nextKey of ['',JSON.stringify('new-key')]){
    const stored=login(),calls=[];let release;
    const d=dashboard(stored,coolFetcher(calls,{'clear-cooldown':()=>new Promise(r=>release=()=>r(status===200?reply({status:'ok',auth_id:'codex-a',cooldown_cleared:true}):reply({error:'host_unsupported'},501))),'/v0/management/auth-files':legacyFiles}));
    await d.api.load();const pending=d.api.clearCooldown('codex-a');stored.managementKey=nextKey;release();await pending;
    assert.equal(legacyCalls(calls).length,0,'no fallback with replaced credentials');
    const card=panel(d.elements,'codex-a');if(card){assert.equal(accountNoteNode(card).textContent,'');assert.equal(accountError(card).textContent,'');assert(coolBadge(card));}else assert(d.elements.get('dashboard').classes.has('needs-login'));
    assert(calls.filter(c=>c.url.endsWith('/data')).every(c=>c.options.headers.Authorization!=='Bearer key'||calls.indexOf(c)<calls.findIndex(x=>x.url.endsWith('clear-cooldown'))),'no stale-key snapshot after the reply');
  }
  const d=dashboard(login(),coolFetcher([],{'clear-cooldown':()=>reply({},401)}));await d.api.load();await d.api.clearCooldown('codex-a');
  assert(d.elements.get('dashboard').classes.has('needs-login'));assert(!panel(d.elements,'codex-a'));
});
