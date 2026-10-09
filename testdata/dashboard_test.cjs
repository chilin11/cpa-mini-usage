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
  constructor(tag) { this.tag = tag; this.children = []; this.textContent = ''; this.attributes = {}; this.dataset = {}; this.style = {setProperty(){}}; this.classList = {add(){}, remove(){}, toggle(){}}; }
  append(child) { this.children.push(child); }
  replaceChildren(...children) { this.children = children; }
  setAttribute(name,value) { this.attributes[name] = value; }
  addEventListener() {}
}
function dashboard(stored = {}, fetcher = async () => ({ok:true,json:async()=>({})})) {
  const elements = new Map();
  const storage = {getItem(name){ return stored[name] ?? null; }};
  const context = vm.createContext({
    Intl, URL, TextEncoder, TextDecoder, atob,
    location:{protocol:'https:',host:'cpa.test',origin:'https://cpa.test'}, navigator:{userAgent:'test-agent'},
    document: {
      getElementById(id) { if (!elements.has(id)) elements.set(id, new Element('div')); return elements.get(id); },
      createElement(tag) { return new Element(tag); },
    }, localStorage:storage, sessionStorage:{getItem(){return null;}}, fetch:fetcher,
    setInterval(){}, setTimeout(callback){callback();}, addEventListener(){},
  });
  context.window=context;
  const instrumented = source.replace(/^load\(\);$/m, '').replace(/\}\)\(\);\s*$/, 'globalThis.dashboardTest = {tokens, fmt, remainingPercent, remainingEstimate, resetCountdown, guessKey, load, setTab, renderSample(sample, provider="") {data=sample; filter=provider; expanded="account"; render();}}; })();');
  vm.runInContext(instrumented, context);
  return {api: context.dashboardTest, elements};
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
  assert(quotaNodes.some(n => n.textContent === '1.8b tokens 剩余' && n.title === '1,800,000,000 tokens'));
  assert(quotaNodes.some(n => n.textContent === '2.4b tokens' && n.title === '2,400,000,000 tokens'));
  assert(quotaNodes.some(n => n.textContent === '—'));
  const requestNodes = descendants(elements.get('credential-table'));
  const cells = requestNodes.filter(n => n.tag === 'td');
  assert.deepEqual(cells.slice(2,8).map(n => n.textContent), ['1,234', '1.25m', '2.4b', '1.23k', '2.4b', '$3.50']);
  assert.equal(cells[6].title, '2,401,250,000 tokens');
  assert(requestNodes.some(n => n.textContent === '1,234 次请求 · 2.4b tokens · $3.50' && n.title === '2,401,250,000 tokens'));
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
  assert.deepEqual(resets.map(n=>n.textContent),['Manual reset 剩余 2 次','Manual reset 剩余 0 次']);
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
  const {api}=dashboard(stored,async(url,options)=>{requests.push({url,options});return {ok:true,json:async()=>({credentials:[],quotas:[]})};});
  assert.equal(api.guessKey(),'test-only-key');await api.load();
  assert.equal(requests[0].url,'/v0/management/plugins/cpa-mini-usage/data');
  assert.equal(requests[0].options.headers.Authorization,'Bearer test-only-key');
  assert.equal(requests[0].options.redirect,'error');
  assert(!/<input\b/.test(html));assert(!/localStorage\.setItem|sessionStorage\.setItem/.test(source));
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
  assert.equal(stats[0].children[0].textContent,'估算剩余价值');
  assert.equal(stats[0].children[1].className,'estimate-cost');
  assert.equal(stats[0].children[1].textContent,'$90.00');
  assert.equal(stats[0].children[2].textContent,'3m tokens 剩余');
  assert.equal(stats[1].children[1].textContent,'$0.000');
  const window=nodes.find(n=>n.className==='window');
  assert.equal(window.children[1].className,'quota-value');
  assert.equal(window.children[3].className,'windowstats');
});
test('missing model price does not display a false USD amount', () => {
  const {api,elements}=dashboard();
  api.renderSample({quotas:[{provider:'codex',five_hour:{used_percent:25,estimated_total_cost:120,estimated_total_tokens:4000000,missing_price:true}}]});
  const nodes=descendants(elements.get('quota-cards'));
  assert.equal(nodes.find(n=>n.className==='estimate-cost').textContent,'—');
  assert(nodes.some(n=>n.textContent==='3m tokens 剩余'));
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
