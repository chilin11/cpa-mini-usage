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
  let fakeNow=Date.now();const FakeDate=class extends Date {constructor(...args){super(...(args.length?args:[fakeNow]));}static now(){return fakeNow;}};
  const document={
    documentElement:descendants(root).find(n=>n.tag==='html'),hidden:false,
    getElementById(id) { const found=descendants(root).find(n=>n.id===id);if(found)return found;if(!elements.has(id))elements.set(id,new Element('div'));return elements.get(id); },
    querySelectorAll(selector) { return descendants(root).filter(n=>selector==='[data-reset-at]'?n.dataset.resetAt:selector==='[data-i18n]'?n.dataset.i18n:selector==='[aria-label]'?n.getAttribute('aria-label')!==null:false); },
    createElement(tag) { return new Element(tag); },
  };
  const context = vm.createContext({
    Intl, URL, TextEncoder, TextDecoder, atob, Date:options.fakeClock?FakeDate:Date,
    location:{protocol:'https:',host:'cpa.test',origin:'https://cpa.test'}, navigator:{userAgent:'test-agent',language:options.language||'zh-CN',languages:options.languages},
    document, localStorage:storage, sessionStorage:{getItem(){return null;}}, fetch:fetcher,
    setInterval(callback,ms){intervals.push({callback,ms});}, setTimeout(callback,ms){if(options.fakeClock)fakeNow+=ms;callback();}, addEventListener(name,callback){events[name]=callback;},
  });
  context.window=context;
  const instrumented = source.replace(/^load\(\);$/m, '').replace(/\}\)\(\);\s*$/, 'globalThis.dashboardTest = {tokens, fmt, remainingPercent, remainingEstimate, resetCountdown, updateCountdowns, acceptSnapshot, guessKey, load, refreshAccount, savePersistence, firstTokenTime, requestTPS, setLanguage, setAlert, setStatus, setTab, renderSample(sample, provider="") {data=sample; filter=provider; expanded="account"; render();}}; })();');
  vm.runInContext(instrumented, context);
  return {api: context.dashboardTest, elements, writes, document, intervals, events};
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
  assert(quotaNodes.some(n => n.textContent === '1.8b token' && n.title === '1,800,000,000 token'));
  assert(!quotaNodes.some(n => n.textContent === '2.4b token'));
  assert(quotaNodes.some(n => n.textContent === '—'));
  const requestNodes = descendants(elements.get('credential-table'));
  const cells = requestNodes.filter(n => n.tag === 'td');
  assert.deepEqual(cells.slice(2,8).map(n => n.textContent), ['1,234', '1.25m', '2.4b', '1.23k', '2.4b', '$3.50']);
  assert.equal(cells[6].title, '2,401,250,000 token');
  assert(requestNodes.some(n => n.textContent === '1,234 次请求 · 2.4b token · $3.50' && n.title === '2,401,250,000 token'));
  assert(descendants(elements.get('recent-table')).some(n => n.textContent === 'gpt'));
});

test('remaining percentage clamps safely and unknown data stays unknown', () => {
  const {api}=dashboard();
  for(const [used,want] of [[0,100],[25,75],[100,0],[110,0],[-5,100],[null,null],[undefined,null],['',null],[Infinity,null]])assert.equal(api.remainingPercent({used_percent:used}),want);
  assert.equal(api.remainingEstimate(4000000,75),3000000);
  assert.equal(api.remainingEstimate(null,75),null);
});
test('providers stay in separate sections; absent manual reset stays hidden', () => {
  const {api,elements}=dashboard();
  const quotas=[{provider:'claude',name:'alice@example.com',manual_reset_remaining:2},{provider:'codex',name:'bob@example.com'},{provider:'codex',name:'carol@example.com',manual_reset_remaining:0}];
  api.renderSample({quotas});
  const sections=elements.get('quota-cards').children;
  assert.deepEqual(sections.map(s=>s.dataset.provider),['claude','codex']);
  const resets=descendants(elements.get('quota-cards')).filter(n=>n.className==='manual-reset');
  assert.deepEqual(resets.map(n=>n.children.map(c=>c.textContent)),[['可重置','2','次'],['可重置','0','次']]);
  api.renderSample({quotas},'claude');assert.equal(elements.get('quota-cards').children.length,1);
  assert.equal(elements.get('quota-cards').children[0].dataset.provider,'claude');
});
function encoded(value){
  const mask=Buffer.from('cli-proxy-api-webui::secure-storage|cpa.test|test-agent');
  return 'enc::v1::'+Buffer.from(Buffer.from(JSON.stringify(value)).map((b,i)=>b^mask[i%mask.length])).toString('base64');
}
test('reuse CPA encoded login without inputs, persistence or query credentials', async () => {
  const requests=[];
  const stored={'cli-proxy-auth':encoded({state:{apiBase:'https://cpa.test',managementKey:'test-only-key'}})};
  const {api,writes}=dashboard(stored,async(url,options)=>{requests.push({url,options});return {ok:true,json:async()=>({credentials:[],quotas:[]})};});
  assert.equal(api.guessKey(),'test-only-key');await api.load();
  assert.equal(requests[0].url,'/v0/management/plugins/cpa-mini-usage/data');
  assert.equal(requests[0].options.headers.Authorization,'Bearer test-only-key');
  assert.equal(requests[0].options.redirect,'error');
  assert.equal((html.match(/<input\b/g)||[]).length,1);assert.match(html,/<input id="persistence-enabled" type="checkbox"/);assert.deepEqual(writes,[]);api.setLanguage('en');assert.deepEqual(writes,[['cpa-mini-usage-language','en']]);assert(!/sessionStorage\.setItem/.test(source));
});
test('cross-origin or corrupt CPA credentials are not reused', () => {
  assert.equal(dashboard({'cli-proxy-auth':encoded({state:{apiBase:'https://other.test',managementKey:'wrong-key'}})}).api.guessKey(),'');
  assert.equal(dashboard({'cli-proxy-auth':'enc::v1::invalid'}).api.guessKey(),'');
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
  api.renderSample({quotas:[{provider:'codex',five_hour:{used_percent:25,estimated_total_cost:120,estimated_total_tokens:4000000},seven_day:{used_percent:100,estimated_total_cost:120,estimated_total_tokens:4000000}}]});
  const nodes=descendants(elements.get('quota-cards'));
  const stats=nodes.filter(n=>n.className==='windowstats');
  assert.equal(stats[0].children[0].className,'estimate-cost');
  assert.equal(stats[0].children[0].textContent,'$90.00');
  assert.equal(stats[0].children[0].children[0].textContent,'美元估算');
  assert.equal(stats[0].children[1].textContent,'3m token');
  assert.equal(stats[1].children[0].textContent,'$0.000');
  const window=nodes.find(n=>n.className==='window');
  assert.equal(window.children[1].className,'quota-value');
  assert.equal(window.children[3].className,'windowstats');
});
test('missing model price does not display a false USD amount', () => {
  const {api,elements}=dashboard();
  api.renderSample({quotas:[{provider:'codex',five_hour:{used_percent:25,estimated_total_cost:120,estimated_total_tokens:4000000,missing_price:true}}]});
  const nodes=descendants(elements.get('quota-cards'));
  assert.equal(nodes.find(n=>n.className==='estimate-cost').textContent,'—');
  assert(nodes.some(n=>n.textContent==='3m token'));
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
  const quota=elements.get('quota-cards').children[0],requests=elements.get('recent-table').children[0];
  api.acceptSnapshot(JSON.parse(JSON.stringify(sample)));
  api.acceptSnapshot({...sample,generated_at:'new',quota_updated_at:'2026-10-09T01:00:00Z',quotas:sample.quotas.map(q=>({...q,fetched_at:'new'}))});
  assert.equal(elements.get('quota-cards').children[0],quota);assert.equal(elements.get('recent-table').children[0],requests);
});
test('request-only changes preserve quota nodes; quota-only changes preserve requests',()=>{
  const {api,elements}=dashboard();let sample=sampleSnapshot();api.renderSample(sample);
  const quota=elements.get('quota-cards').children[0];
  sample={...sample,recent:[{auth_id:'account',model:'gpt-test',total:1000}]};api.acceptSnapshot(sample);
  assert.equal(elements.get('quota-cards').children[0],quota);
  const requests=elements.get('recent-table').children[0];
  api.acceptSnapshot({...sample,quotas:sample.quotas.map(q=>({...q,manual_reset_remaining:2}))});
  assert.equal(elements.get('recent-table').children[0],requests);
  assert.equal(elements.get('quota-cards').children[0],quota); // Provider section also survives a changed card.
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
  assert.match(html,/\.needs-login \.tabs,\.needs-login \.toolbar,\.needs-login \[role=tabpanel\],\.needs-login \.foot\{display:none\}/);
});

const reply=(payload,status=200)=>({ok:status>=200&&status<300,status,json:async()=>payload});
const login=()=>({managementKey:JSON.stringify('key')});
function twoAccounts(){const sample=sampleSnapshot();return {...sample,quotas:[...sample.quotas,{auth_id:'other',provider:'codex',name:'bob@example.com',manual_reset_remaining:2,five_hour:{used_percent:20}}]};}
const panel=(elements,id)=>descendants(elements.get('quota-cards')).find(n=>n.tag==='article'&&n.dataset.authId===id);
const refreshButton=card=>descendants(card).find(n=>n.className==='button account-refresh');
const accountError=card=>descendants(card).find(n=>n.className==='account-error');

test('language defaults follow browser, unsupported browsers fall back to zh, and only language persists',()=>{
  for(const [browser,stored,want] of [['en-GB',{},'en'],['zh-TW',{},'zh'],['fr-FR',{},'zh'],['en-US',{'cpa-mini-usage-language':'zh'},'zh'],['zh-CN',{'cpa-mini-usage-language':'en'},'en'],['en-US',{'cpa-mini-usage-language':'invalid'},'en']]){
    const d=dashboard(stored,undefined,{language:browser});assert.equal(d.elements.get('language').value,want);assert.equal(d.document.documentElement.lang,want==='en'?'en':'zh-CN');assert.deepEqual(d.writes,[]);
    d.api.setLanguage(want==='en'?'zh':'en');assert.deepEqual(d.writes,[['cpa-mini-usage-language',want==='en'?'zh':'en']]);
  }
  assert.equal(dashboard({},undefined,{languages:['en-AU','zh-CN']}).elements.get('language').value,'en');
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

test('backend refresh from another window changes only matching button, and rejects concurrent local requests',async()=>{
  let posts=0;const sample=twoAccounts(),d=dashboard(login(),async url=>{if(url.endsWith('refresh-account'))posts++;return reply(sample);});await d.api.load();const first=panel(d.elements,'account'),other=panel(d.elements,'other'),table=d.elements.get('recent-table').children[0];
  d.api.acceptSnapshot({...sample,generated_at:'new',account_refresh:{account:{refreshing:true}}});assert.equal(panel(d.elements,'account'),first);assert.equal(panel(d.elements,'other'),other);assert.equal(refreshButton(first).disabled,true);assert.equal(refreshButton(other).disabled,false);await d.api.refreshAccount('other');assert.equal(posts,0);assert.match(accountError(other).textContent,/正在进行/);assert.equal(d.elements.get('recent-table').children[0],table);
  d.api.acceptSnapshot({...sample,account_refresh:{account:{refreshing:false,finished_at:'done',error:'quota response invalid'}}});assert.equal(refreshButton(first).disabled,false);assert.equal(accountError(first).textContent,'额度响应格式异常。');
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

test('local individual refresh rejects a second account without toggling its button or posting twice',async()=>{
  let release,posts=0;const sample=twoAccounts(),d=dashboard(login(),async url=>{if(url.endsWith('refresh-account')){posts++;return new Promise(resolve=>release=()=>resolve(reply({refresh_finished_at:'old'},202)));}return reply({...sample,account_refresh:{account:{refreshing:false,finished_at:'done'}}});});
  await d.api.load();const other=panel(d.elements,'other'),pending=d.api.refreshAccount('account');await d.api.refreshAccount('other');assert.equal(posts,1);assert.equal(refreshButton(other).disabled,false);assert.match(accountError(other).textContent,/正在进行/);release();await pending;assert.equal(panel(d.elements,'other'),other);
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
  const css=html.match(/<style>([\s\S]*?)<\/style>/)[1];assert.match(css,/\.percent\{font-size:22px/);assert.match(css,/\.estimate-cost\{[^}]*font-size:22px/);assert.match(css,/\.estimate-value\{font-size:14px/);assert.match(css,/\.manual-reset strong\{font-size:18px/);assert.match(css,/\.masthead\{[^}]*flex-wrap:wrap/);
});

test('successful later refresh clears a prior local account error without replacing siblings',async()=>{
  const sample=twoAccounts(),d=dashboard(login(),async url=>url.endsWith('refresh-account')?reply({error:'refresh_busy'},409):reply(sample));
  await d.api.load();const other=panel(d.elements,'other');await d.api.refreshAccount('account');assert.match(accountError(panel(d.elements,'account')).textContent,/正在进行/);
  d.api.acceptSnapshot({...sample,quotas:sample.quotas.map(q=>q.auth_id==='account'?{...q,fetched_at:'successful-later'}:q),account_refresh:{account:{refreshing:false,finished_at:'completed'}}});
  assert.equal(accountError(panel(d.elements,'account')).textContent,'');assert.equal(panel(d.elements,'other'),other);
});

test('TPS excludes first-token wait, rejects absent/invalid timing, and never guesses',()=>{
  const {api}=dashboard(),event={output:100,latency_ms:2500,ttft_ms:500};assert.equal(api.requestTPS(event),50);
  for(const value of [null,undefined,NaN,Infinity,-1,'500','',true]){
    assert.equal(api.requestTPS({...event,ttft_ms:value}),null);assert.equal(api.requestTPS({...event,latency_ms:value}),null);
    assert.equal(api.firstTokenTime(value),'—');
  }
  for(const patch of [{latency_ms:500},{latency_ms:0},{latency_ms:499},{ttft_ms:0,latency_ms:0},{output:0},{output:-1},{output:NaN},{output:Infinity},{output:'100'},{output:null},{failed:true}, {ttft_ms:undefined}, {latency_ms:undefined}])assert.equal(api.requestTPS({...event,...patch}),null,JSON.stringify(patch));
  assert.equal(api.requestTPS({...event,ttft_ms:0}),40);assert.equal(api.firstTokenTime(0),'0 ms');assert.equal(api.firstTokenTime(450),'450 ms');assert.equal(api.firstTokenTime(1250),'1.25 s');
});

for(const language of ['zh','en']){
  test(`${language}: recent TTFT/TPS columns show units and full formula timing tooltips`,()=>{
    const d=dashboard({},undefined,{language});d.api.renderSample({recent:[{output:100,ttft_ms:500,latency_ms:2500,model:'gpt-test'},{output:100,ttft_ms:1250,latency_ms:3250},{output:100},{output:100,ttft_ms:500,latency_ms:2500,failed:true}]});
    const nodes=descendants(d.elements.get('recent-table')),ttft=nodes.filter(n=>n.className==='num first-token'),tps=nodes.filter(n=>n.className==='num tps');
    assert(nodes.some(n=>n.tag==='th'&&n.textContent===(language==='en'?'First token':'首字时间')));assert(nodes.some(n=>n.tag==='th'&&n.textContent==='TPS'));
    assert.deepEqual(ttft.map(n=>n.textContent),['500 ms','1.25 s','—','500 ms']);assert.deepEqual(tps.map(n=>n.textContent),['50','50','—','—']);
    assert.equal(tps[0].children[0].textContent,language==='en'?' tokens/s':' token/s');assert.match(tps[0].title,/2500 ms/);assert.match(tps[0].title,/500 ms/);assert.match(tps[0].title,/100 token/);assert.match(tps[0].title,/1000/);assert.match(tps[0].title,/−/);
    d.api.setLanguage(language==='en'?'zh':'en');const translated=descendants(d.elements.get('recent-table')).find(n=>n.className==='num tps');assert.match(translated.title,language==='en'?/首字时间/:/first-token time/);
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
  assert(!html.includes('\u8bcd\u5143'));const css=html.match(/<style>([\s\S]*?)<\/style>/)[1];assert.match(css,/\.tablewrap\{overflow-x:auto/);assert.match(css,/#recent-table table\{min-width:1120px/);assert.match(css,/\.tablewrap\{max-width:100%;min-width:0\}/);assert.match(html,/v0\.1\.5/);
});

test('enabling persistence succeeds using returned setting and directory without redrawing requests',async()=>{
  const sample={...twoAccounts(),persistence:{enabled:false,directory:'/old'}},d=dashboard(login(),async url=>url.endsWith('settings')?reply({persistence:{enabled:true,directory:'/flushed'}}):reply(sample));await d.api.load();const table=d.elements.get('recent-table').children[0];await d.api.savePersistence(true);assert.equal(d.elements.get('persistence-enabled').checked,true);assert.equal(d.elements.get('persistence-status').textContent,'开启');assert.match(d.elements.get('persistence-enabled').title,/\/flushed/);assert.equal(d.elements.get('recent-table').children[0],table);
});

test('persistence updates do not interfere with a running individual account refresh',async()=>{
  const sample={...twoAccounts(),persistence:{enabled:true,directory:'/data'}},posts=[];let release,refreshed=false;
  const d=dashboard(login(),async(url,options)=>{if(url.endsWith('refresh-account')){posts.push(url);return new Promise(resolve=>release=()=>{refreshed=true;resolve(reply({refresh_finished_at:'old'},202));});}if(url.endsWith('settings')){posts.push(url);return reply({persistence:{enabled:false,directory:'/data'}});}return reply(refreshed?{...sample,persistence:{enabled:false,directory:'/data'},account_refresh:{account:{refreshing:false,finished_at:'done'}}}:sample);});await d.api.load();const other=panel(d.elements,'other'),pending=d.api.refreshAccount('account');await d.api.savePersistence(false);assert.equal(refreshButton(panel(d.elements,'account')).disabled,true);assert.equal(refreshButton(other).disabled,false);release();await pending;assert.equal(panel(d.elements,'other'),other);assert.equal(d.elements.get('persistence-enabled').checked,false);assert.equal(posts.length,2);
});
