'use strict';
// CSS-only guards plus optional real-browser checks. No package install is needed.
// Browser run: DASHBOARD_PLAYWRIGHT=/absolute/path/to/playwright \
// DASHBOARD_CHROMIUM=/absolute/path/to/chrome node --test testdata/dashboard_visual_test.cjs
// Reusable offline preview: node testdata/dashboard_visual_test.cjs --serve /absolute/scratch/path
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const { test } = require('node:test');
const html = fs.readFileSync(path.join(__dirname, '../web/index.html'), 'utf8');
const css = html.match(/<style>([\s\S]*?)<\/style>/)[1];
const source = html.match(/<script>([\s\S]*?)<\/script>/)[1];

function luminance(hex) {
  return hex.slice(1).match(/../g).map(x => parseInt(x, 16) / 255)
    .map(x => x <= .04045 ? x / 12.92 : ((x + .055) / 1.055) ** 2.4)
    .reduce((sum, x, i) => sum + x * [.2126, .7152, .0722][i], 0);
}
function contrast(a, b) {
  const [high, low] = [luminance(a), luminance(b)].sort((a, b) => b - a);
  return (high + .05) / (low + .05);
}
const themes = [...css.matchAll(/:root\{([^}]+)\}/g)].map(m => Object.fromEntries(
  [...m[1].matchAll(/(--[\w-]+):(#\w{6})(?=;|$)/g)].map(x => [x[1], x[2]])
));
const contrastPairs = [
  ...['--panel', '--subtle', '--hover', '--accent-soft'].flatMap(bg =>
    ['--ink', '--muted', '--cost'].map(fg => [fg, bg])),
  ['--accent', '--accent-soft'], ['--forecast', '--forecast-soft'],
  ['--good', '--good-soft'], ['--bad', '--bad-soft'], ['--warm', '--warm-soft'],
];

// Synthetic records only: rendered with the actual dashboard renderer, not copied markup.
function sample() {
  const win = (used, cost = 12.8) => ({used_percent: used, observed_cost: cost,
    observed_tokens: 1280000, estimated_total_cost: 64, estimated_total_tokens: 6400000,
    reset_at: '2026-10-11T12:00:00Z'});
  return {
    generated_at: '2026-10-09T10:24:00Z', quota_updated_at: '2026-10-09T10:20:00Z',
    persistence: {enabled: true, directory: '/demo-only/data'},
    quotas: [
      {provider: 'claude', auth_id: 'demo-claude', name: 'demo-team@example.test',
        manual_reset_remaining: 2, five_hour: win(24), seven_day: win(91)},
      {provider: 'codex', auth_id: 'demo-codex', name: 'demo-lab@example.test',
        five_hour: win(36), seven_day: win(62)},
      {provider: 'codex', auth_id: 'demo-long', name: 'demo-very-long-account-identifier-for-responsive-check@example.test',
        five_hour: win(0, 0), seven_day: null},
    ],
    credentials: [
      {provider: 'claude', auth_id: 'demo-claude', name: 'demo-team@example.test', requests: 1342,
        failures: 3, input: 980000, output: 124000, cached: 280000, cache_write: 20000,
        total: 1404000, cost: 12.8, models: [{model: 'claude-sonnet-demo', requests: 1342, tokens: 1404000, cost: 12.8}]},
      {provider: 'codex', auth_id: 'demo-codex', name: 'demo-lab@example.test', requests: 860,
        failures: 0, input: 2500000, output: 250000, cached: 500000, cache_write: 0,
        total: 2750000, cost: 6.4, models: [{model: 'gpt-demo', requests: 860, tokens: 2750000, cost: 6.4}]},
    ],
    recent: Array.from({length: 24}, (_, i) => ({
      provider: i % 2 ? 'codex' : 'claude', auth_id: i % 2 ? 'demo-codex' : 'demo-claude',
      at: '2026-10-09T10:24:00Z', model: i % 2 ? 'gpt-demo' : 'claude-sonnet-demo',
      requested_model: i === 2 ? 'demo-client-alias-with-a-long-model-name' : '',
      response_model: i % 4 === 3 ? '' : i % 4 === 2 ? 'different-demo-model' : i % 4 === 1 ? 'gpt-demo-20261009' : 'claude-sonnet-demo',
      model_status: ['same', 'variant', 'different', 'unknown'][i % 4],
      reasoning_effort: i % 3 ? 'high' : '', reasoning_effort_source: i % 3 ? 'host_usage' : '',
      input: 24000, output: 1200, cache_read: 4000, cache_write: 200,
      total: 29400, cost: i === 3 ? null : .0028, missing_price: i === 3,
      ttft_ms: 420, latency_ms: 12400, failed: i === 4,
    })),
  };
}

function mixedSample() {
  const data=sample(),base=data.quotas[1];
  data.quotas=[
    {...base,auth_id:'codex-a1b2-demo-primary@example.test-team.json',name:'jessica510tang@gmail.com',manual_reset_remaining:2},
    {...base,auth_id:'codex-b2c3-demo-secondary@example.test-team.json',name:'codex-b2c3-demo-secondary@example.test-team.json',five_hour:{...base.five_hour,used_percent:88}},
    {...base,auth_id:'codex-c3d4-demo-unnamed@example.test-team.json',name:'',five_hour:{...base.five_hour,used_percent:72}},
    {...base,auth_id:'codex-d4e5-demo-lab@example.test-plus.json',name:'demo-lab@example.test',five_hour:{...base.five_hour,used_percent:97}},
    {...data.quotas[0],auth_id:'claude-e5f6-demo@example.test.json',name:'demo-claude@example.test'},
    {provider:'antigravity',auth_id:'antigravity-f6a7-demo@example.test.json',name:'demo-google@example.test',
      groups:['gemini','claude-gpt'].map(key=>({key,five_hour:base.five_hour,seven_day:base.seven_day}))},
  ];
  data.credentials=data.quotas.map(q=>({...data.credentials[1],provider:q.provider,auth_id:q.auth_id,name:q.name}));
  data.recent=data.quotas.map(q=>({...data.recent[1],provider:q.provider,auth_id:q.auth_id}));
  return data;
}

function fixture(pageHTML=html) {
  const pageSource=pageHTML.match(/<script>([\s\S]*?)<\/script>/)[1];
  assert.match(pageSource, /^load\(\);$/m, 'fixture must suppress initial live load');
  const boot = `
const offlineFetch=window.fetch;
window.visualFixture = {
  show(state='ready') {
    window.fetch=offlineFetch;window.__fixtureKey='';
    actionJobs.clear();accountNotes.clear();pendingRedeem.clear();closeReset();
    accountJobs.clear();accountErrors.clear();cardCache.clear();
    settingsJob=null;settingsError=null;requestJob=null;loading=false;headerLoading=false;
    filter='';$('provider').value='';phase='ready';key='';expanded='';recentPage=0;
    accountSearch='';accountState='';accountSort='default';accountPage=0;accountSize=12;
    $('account-search').value='';$('account-state').value='';$('account-sort').value='default';$('account-size').value='12';
    historyQuery=null;historyPage=null;historyError='';historyBusy=false;historyIndex=0;historyCursors=[''];
    $('history-start').value='';$('history-end').value='';
    $('dashboard').classList.remove('needs-login');$('gate').classList.add('hide');setAlert('');
    data=${JSON.stringify(sample())};
    if(state==='mixed')data=${JSON.stringify(mixedSample())};
    if(state==='many')data.quotas=Array.from({length:137},(_,i)=>({...data.quotas[i%3],auth_id:'demo-credential-'+i,name:'demo-team-'+String(i).padStart(3,'0')+'@example.test',disabled:i%9===0,manual_reset_remaining:i%3===1?2:undefined}));
    if(state==='groups')data.quotas=Array.from({length:3},(_,i)=>({provider:'antigravity',auth_id:'demo-group-'+i,name:'demo-group-'+i+'@example.test',groups:['gemini','claude'].map(label=>({key:label,label:label+' demo',five_hour:data.quotas[0].five_hour,seven_day:data.quotas[0].seven_day}))}));
    if(state==='empty'){data.quotas=[];data.credentials=[];data.recent=[];}
    if(state==='loading'){data=null;loading=true;headerLoading=true;phase='loading';}
    if(state==='error'){data=null;phase='error';setAlert('无法连接 CPA。');}
    if(state==='account-error')data.quotas[0].error='quota endpoint failed';
    if(state==='busy')data.account_refresh={'demo-claude':{refreshing:true}};
    if(state==='long'){
      data.quotas[0].name='DEMO-'+('long-account-identifier-'.repeat(12))+'@example.test';
      data.quotas[0].five_hour.estimated_total_cost=123456789012.34;
      data.quotas[0].five_hour.estimated_total_tokens=123456789012345;
      data.recent[0].requested_model='demo-model-'.repeat(35);
    }
    setStatus(state==='loading'?'正在连接':state==='error'?'连接失败':'已连接');
    render();syncControls();
    if(state==='login')clearSnapshot(true,'');
    document.body.dataset.fixtureState=state;
  },
  // Real account handlers against an in-memory API, never a live endpoint or storage key.
  startActions() {
    key=window.__fixtureKey='DEMO-OFFLINE-NOT-A-CREDENTIAL';
    this.calls=[];this.held='';this.failure='';this.release=null;
    const state=this;let finished=0;
    window.fetch=async(address,options={})=>{
      const route=address.split('/').pop(),body=options.body?JSON.parse(options.body):null;
      state.calls.push({route,method:options.method,body});
      if(state.held===route){state.held='';await new Promise(resolve=>state.release=resolve);state.release=null;}
      const reply=(payload,status=200)=>({ok:status===200,status,json:async()=>JSON.parse(JSON.stringify(payload))});
      if(state.failure===route){state.failure='';return reply({error:'account_not_found'},404);}
      if(route==='status')return reply({disabled:body.disabled});
      if(route==='reset-credits')return reply({available_count:2,credits:[{id:'demo-first',expires_at:'2026-10-10T12:00:00Z'},{id:'demo-second',expires_at:'2026-10-12T12:00:00Z'}]});
      if(route==='consume-reset')return reply({code:'nothing_to_reset'});
      if(route==='refresh-account'){
        data={...data,account_refresh:{...data.account_refresh,[body.auth_id]:{refreshing:false,finished_at:'demo-finished-'+(++finished)}}};
        return reply({refresh_finished_at:'demo-previous'});
      }
      if(route==='data')return reply(data);
      throw Error('Unexpected offline API route: '+route);
    };
  },
  showHistory() {
    data.history_available=true;
    historyPage={events:data.recent.slice(0,20),total:124,offset:0,limit:20,next_cursor:'demo-only',generated_at:data.generated_at,
      retention:{days:35,max_records:200000,retained_records:148,oldest_at:'2026-09-10T10:24:00Z',newest_at:data.generated_at}};
    $('history-start').value='';$('history-end').value='';historyQuery=historyParams().toString();
    setTab('requests');renderRecent();
  }
};
const params=new URLSearchParams(location.search);
setLanguage(params.get('lang')==='en'?'en':'zh',false);
visualFixture.show(params.get('state')||'ready');
setTab(params.get('tab')==='requests'?'requests':'quota');
`;
  const instrumented = pageSource.replace(/^load\(\);$/m, '')
    .replace('function guessKey(){', "function guessKey(){return window.__fixtureKey||'';")
    .replace('const pause=()=>new Promise(resolve=>setTimeout(resolve,2000));','const pause=()=>Promise.resolve();')
    .replace(/\}\)\(\);\s*$/, boot + '\n})();');
  const safety = `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'none'; img-src 'none'; font-src 'none'; base-uri 'none'; form-action 'none'">
<script>
window.__blockedNetwork=0;window.__disabledTimers=0;
const NativeDate=Date;
window.Date=class extends NativeDate{constructor(...args){super(...(args.length?args:['2026-10-09T10:24:00Z']));}static now(){return NativeDate.parse('2026-10-09T10:24:00Z');}};
window.fetch=async()=>{window.__blockedNetwork++;throw Error('DEMO fixture: network disabled');};
window.XMLHttpRequest=function(){throw Error('DEMO fixture: network disabled');};
window.WebSocket=function(){throw Error('DEMO fixture: network disabled');};
window.EventSource=function(){throw Error('DEMO fixture: network disabled');};
window.setTimeout=window.setInterval=()=>{window.__disabledTimers++;return 0;};
window.clearTimeout=window.clearInterval=()=>{};
window.requestAnimationFrame=()=>0;
</script>`;
  // Separate labels never alter the production file or use live credentials.
  const demoLabel = '<aside aria-label="Demo fixture" style="padding:8px 12px;border-bottom:1px solid var(--line);font:12px/1.5 var(--mono);color:var(--muted);background:var(--subtle)">DEMO / 演示数据 · Offline fixture · Network &amp; timers disabled</aside>';
  return pageHTML.replace('<head>', '<head>' + safety).replace('<body>', '<body>' + demoLabel)
    .replace('<script>' + pageSource + '</script>', () => '<script>' + instrumented + '</script>');
}
function board() {
  const frames = [390, 1480].flatMap(width => ['zh', 'en'].flatMap(lang => ['quota', 'requests'].map(tab =>
    `<section><h2>${width}px · ${lang} · ${tab} · DEMO</h2><iframe title="${width}px ${lang} ${tab}" width="${width}" height="900" src="fixture.html?lang=${lang}&tab=${tab}"></iframe></section>`))).join('');
  return '<!doctype html><html lang="en"><meta charset="utf-8"><title>Dashboard demo checks</title><style>body{font:14px system-ui;background:#121a27;color:#deebff;margin:24px}section{margin:24px 0}iframe{display:block;border:1px solid #52657c;background:white}h2{font-size:14px}</style><h1>Offline dashboard demo · fixed-width frames</h1><p>Theme follows the browser. Test runner checks light/dark and reduced motion separately. No production API or credentials.</p>' + frames + '</html>';
}
async function serve(directory) {
  const workspace = path.resolve(__dirname, '..'), resolved = path.resolve(directory);
  assert(path.isAbsolute(directory) && resolved !== workspace && !resolved.startsWith(workspace + path.sep), 'fixture files must stay outside the workspace');
  fs.mkdirSync(directory, {recursive: true});
  fs.writeFileSync(path.join(directory, 'fixture.html'), fixture());
  fs.writeFileSync(path.join(directory, 'board.html'), board());
  const server = http.createServer((req, res) => {
    const name = new URL(req.url, 'http://localhost').pathname === '/board.html' ? 'board.html' : 'fixture.html';
    res.writeHead(200, {'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store'});
    res.end(fs.readFileSync(path.join(directory, name)));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  return {server, url: `http://127.0.0.1:${server.address().port}`};
}

if (process.argv[2] === '--serve') {
  const directory = process.argv[3];
  const workspace = path.resolve(__dirname, '..');
  assert(directory && path.isAbsolute(directory), 'supply an absolute scratch directory');
  assert(!path.resolve(directory).startsWith(workspace + path.sep) && path.resolve(directory) !== workspace, 'fixture files must stay outside the workspace');
  serve(directory).then(({url}) => console.log(JSON.stringify({url: url + '/fixture.html', board: url + '/board.html', directory}))).catch(error => {console.error(error);process.exitCode = 1;});
} else {
  test('semantic small-text colors meet 4.5:1 on actual tinted and hover surfaces', () => {
    assert.equal(themes.length, 2);
    for (const [i, theme] of themes.entries()) for (const [fg, bg] of contrastPairs)
      assert(contrast(theme[fg], theme[bg]) >= 4.5, `${i ? 'dark' : 'light'} ${fg} on ${bg}: ${contrast(theme[fg], theme[bg]).toFixed(2)}`);
  });
  test('CSS preserves metric hierarchy, cold-blue forecasts, orange mismatch and no motion or external assets', () => {
    assert.match(css, /\.percent\{font-size:22px/);
    for (const name of ['observed-cost', 'estimate-cost']) assert.match(css, new RegExp('\\.' + name + '\\{[^}]*font-size:15px'));
    for (const name of ['observed-value', 'estimate-value']) assert.match(css, new RegExp('\\.' + name + '\\{font-size:12px'));
    assert.match(css, /\.model-badge\.different\{color:var\(--warm\);background:var\(--warm-soft\)/);
    for (const theme of themes) {
      const rgb = theme['--forecast'].slice(1).match(/../g).map(x => parseInt(x, 16));
      assert(rgb[2] > rgb[1] && rgb[1] > rgb[0], 'forecast is blue, not purple');
    }
    assert(!/@import|url\(|@keyframes|\b(?:animation|transition)\s*:/.test(css));
    assert.match(css, /:focus-visible\{outline:2px solid var\(--accent\)/);
    assert.match(css, /\.hide,\[hidden\]\{display:none!important\}/);
    assert.match(css, /\.needs-login \.tabs,\.needs-login \[role=tabpanel\]/);
    assert(!/(?:body|main|html)\s*\{[^}]*overflow(?:-x)?\s*:\s*(?:hidden|clip)/.test(css));
    assert.match(css, /overscroll-behavior-x:contain/);
  });

  let playwright;
  try { playwright = require(process.env.DASHBOARD_PLAYWRIGHT || 'playwright'); } catch {}
  test('visible browser matrix: 390/1480, zh/en, light/dark, keyboard, states and reduced motion',
    {skip: !playwright && 'Set DASHBOARD_PLAYWRIGHT to an existing local Playwright installation', timeout: 120000}, async t => {
      const scratch = process.env.PI_SCRATCH_DIR || process.env.DASHBOARD_VISUAL_SCRATCH;
      assert(scratch && path.isAbsolute(scratch), 'set PI_SCRATCH_DIR or DASHBOARD_VISUAL_SCRATCH to a scratch directory');
      const directory = path.join(scratch, 'dashboard-visual');
      const {server, url} = await serve(directory);
      t.after(() => new Promise(resolve => server.close(resolve)));
      const browser = await playwright.chromium.launch({headless: true, ...(process.env.DASHBOARD_CHROMIUM ? {executablePath: process.env.DASHBOARD_CHROMIUM} : {})});
      t.after(() => browser.close());
      const report = [];
      for (const width of [390, 1480]) for (const lang of ['zh', 'en']) for (const theme of ['light', 'dark']) {
        const context = await browser.newContext({viewport: {width, height: 1000}, colorScheme: theme, reducedMotion: 'reduce', locale: lang === 'en' ? 'en-US' : 'zh-CN'});
        try {
          const page = await context.newPage(), errors = [], requests = [];
          page.on('pageerror', e => errors.push(e.message));
          await page.route('**/*', route => {
            const address = route.request().url();
            if (address.startsWith(url + '/fixture.html') && route.request().isNavigationRequest()) return route.continue();
            requests.push(address);return route.abort();
          });
          await page.goto(`${url}/fixture.html?lang=${lang}`, {waitUntil: 'load'});
          const key = `${width}-${lang}-${theme}`;
          async function layout(tab) {
            const measured = await page.evaluate(tab => {
              const root = document.querySelector('#page-' + tab);
              const visible = el => Boolean(el && el.getClientRects().length && el.getBoundingClientRect().width && el.getBoundingClientRect().height);
              const invalid = [...document.querySelectorAll('.panel,.window,.usage-row>*,.toolbar,.summary-item,.modelrow,td,th')]
                .filter(visible).filter(el => el.scrollWidth > el.clientWidth + 1).map(el => el.className);
              return {tab, visible: visible(root), login: document.querySelector('#dashboard').classList.contains('needs-login'),
                viewport: innerWidth, pageWidth: document.documentElement.scrollWidth, invalid,
                cards: [...document.querySelectorAll('.panel')].filter(visible).length,
                tables: [...document.querySelectorAll('.tablewrap')].filter(visible).map(el => ({width: el.clientWidth, scroll: el.scrollWidth, overflow: getComputedStyle(el).overflowX})),
                type: Object.fromEntries(['percent', 'observed-cost', 'estimate-cost', 'observed-value', 'estimate-value'].map(c => [c, getComputedStyle(document.querySelector('.' + c)).fontSize])),
                resets: [...document.querySelectorAll('.panel')].filter(visible).map(el => [...el.querySelectorAll('.reset')].map(node => node.getBoundingClientRect().bottom)),
                animations: document.getAnimations().length, network: window.__blockedNetwork, timers: window.__disabledTimers};
            }, tab);
            assert(measured.visible && !measured.login, `${key}: validate real visible content`);
            assert(measured.pageWidth <= width, `${key} ${tab}: page overflow ${measured.pageWidth}`);
            assert.deepEqual(measured.invalid, [], `${key} ${tab}: overflowing content`);
            assert.equal(measured.animations, 0);assert.equal(measured.network, 0);assert.equal(measured.timers, 2);
            if (tab === 'quota') {
              assert.equal(measured.cards, 3);
              assert.deepEqual(Object.values(measured.type), ['22px', '15px', '15px', '12px', '12px']);
              if (width === 1480) for (const [first, second] of measured.resets) assert(Math.abs(first - second) < 1, 'reset footers stay aligned when one window wraps');
            } else {
              assert.equal(measured.tables.length, 2);
              for (const table of measured.tables) {
                assert(table.width > 0);assert.equal(table.overflow, 'auto');
                if (width === 390) assert(table.scroll > table.width, 'mobile scrolling must stay inside tables');
                else assert.equal(table.scroll, table.width, 'desktop tables fit the wide request view');
              }
            }
            report.push({key, ...measured});
          }
          await layout('quota');
          await page.screenshot({path: path.join(directory, key + '-quota.png'), fullPage: true});
          // Real keyboard input: tab activation and visible focus, not programmatic pseudo-class checks.
          await page.keyboard.press('Tab');
          assert.equal(await page.locator(':focus').getAttribute('id'), 'tab-quota');
          await page.keyboard.press('ArrowRight');
          assert.equal(await page.locator(':focus').getAttribute('id'), 'tab-requests');
          assert.equal(await page.locator('#tab-requests').getAttribute('aria-selected'), 'true');
          assert(await page.locator('#tab-requests').evaluate(el => el.matches(':focus-visible') && getComputedStyle(el).outlineWidth === '2px'));
          await layout('requests');
          await page.screenshot({path: path.join(directory, key + '-requests.png'), fullPage: true});
          // Walk every toolbar control to the scrollable region using the keyboard.
          for (const id of ['refresh', 'language', 'provider', 'persistence-enabled', 'page-requests', 'recent-table']) {
            await page.keyboard.press('Tab');
            assert.equal(await page.locator(':focus').getAttribute('id'), id);
            assert(await page.locator('#' + id).evaluate(el => el.matches(':focus-visible') && getComputedStyle(el).outlineWidth === '2px'));
          }
          if (width === 390) {
            await page.locator('#recent-table').evaluate(el => {el.scrollLeft = 160;});
            assert((await page.locator('#recent-table').evaluate(el => el.scrollLeft)) > 0);
            assert.equal(await page.evaluate(() => scrollX), 0);
          }
          await page.locator('#recent-next').click();
          assert.equal(await page.locator('#recent-page').textContent(), '2 / 2');
          await page.locator('#recent-size').selectOption('40');
          assert.equal(await page.locator('#recent-page').textContent(), '1 / 1');
          await page.locator('.credential-button').first().click();
          assert.equal(await page.locator('.credential-button').first().getAttribute('aria-expanded'), 'true');
          assert(await page.locator('.details').isVisible());
          await layout('requests');
          await page.locator('#provider').selectOption('claude');
          assert.equal(await page.locator('#credential-count').textContent(), lang === 'en' ? '1 credentials' : '1 个凭证');
          await page.locator('#language').selectOption(lang === 'en' ? 'zh' : 'en');
          assert.equal(await page.locator('html').getAttribute('lang'), lang === 'en' ? 'zh-CN' : 'en');
          await page.locator('#language').selectOption(lang);
          await page.locator('#tab-quota').click();
          for (const state of ['busy', 'account-error', 'empty', 'loading', 'error', 'login', 'long']) {
            await page.evaluate(state => visualFixture.show(state), state);
            if (state === 'busy') {
              assert(await page.locator('.account-refresh').first().isDisabled());
              assert.equal(await page.locator('.account-refresh').first().evaluate(el => getComputedStyle(el).borderTopStyle), 'dashed');
            }
            if (state === 'account-error') assert(await page.locator('.account-error').first().isVisible());
            if (['empty', 'loading', 'error'].includes(state)) assert(await page.locator('#page-quota .empty').isVisible());
            if (state === 'loading') assert(await page.locator('#refresh').isDisabled());
            if (state === 'error') assert(await page.locator('#alert').isVisible());
            if (state === 'login') {
              assert(await page.locator('#gate').isVisible());
              assert(!(await page.locator('#page-quota').isVisible()));
              assert(!(await page.locator('.tabs').isVisible()));
              assert(await page.locator('#language').isVisible());
            }
            assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${key} ${state}: page overflow`);
          }
          await layout('quota');
          await page.locator('#tab-requests').click();await layout('requests');
          // Reuse the existing two viewport contexts, without adding a matrix.
          if (lang === 'zh' && theme === 'light') {
            await page.locator('#tab-quota').click();
            await page.evaluate(() => visualFixture.show('mixed'));
            const mixed=await page.evaluate(()=>{
              const root=document.querySelector('#quota-cards'),stacks=[...root.children],cards=[...root.querySelectorAll('article')];
              const rect=el=>{const r=el.getBoundingClientRect();return {x:r.x,y:r.y,width:r.width,height:r.height,bottom:r.bottom};};
              return {pageWidth:document.documentElement.scrollWidth,rects:cards.map(rect),stacks:stacks.map(rect),
                providers:stacks.map(el=>el.querySelector('h3').textContent),counts:stacks.map(el=>el.querySelectorAll('article').length),
                adjacent:stacks.every(stack=>[...stack.querySelectorAll('article')].every((card,i,cards)=>!i||Math.abs(rect(card).x-rect(cards[i-1]).x)<1&&Math.abs(rect(card).y-rect(cards[i-1]).bottom-8)<1)),
                names:cards.map(card=>{const el=card.querySelector('.account');return {text:el.textContent,title:el.title,aria:el.getAttribute('aria-label'),height:rect(el).height,lineHeight:parseFloat(getComputedStyle(el).lineHeight),clipped:el.scrollWidth>el.clientWidth};}),
                windows:cards.map(el=>el.querySelectorAll('.window').length),
                invalid:[...root.querySelectorAll('.panel,.window,.usage-row>*,.accounttop')].filter(el=>el.scrollWidth>el.clientWidth+1||el.getBoundingClientRect().right>innerWidth).map(el=>el.className),
                labels:[...root.querySelectorAll('*')].flatMap(el=>[el.textContent,el.title,el.getAttribute('aria-label')]).filter(Boolean)};
            });
            assert.equal(mixed.rects.length,6);assert.deepEqual(mixed.counts,[4,1,1]);assert(mixed.adjacent,'cards are adjacent vertically within each provider stack');
            assert.deepEqual(mixed.providers,['Codex','Claude','Antigravity']);assert.deepEqual(mixed.windows,[2,2,2,2,2,4]);
            assert.equal(mixed.pageWidth,width);assert.deepEqual(mixed.invalid,[]);
            assert(mixed.names.every(n=>n.height===n.lineHeight&&n.title===n.text&&n.aria===n.text),'names stay single-line with full safe accessible names');
            assert.equal(mixed.names[0].text,'jessica510tang@gmail.com');assert(!mixed.names[0].clipped,'the typical email must be fully visible, even beside the actions');
            assert(mixed.labels.every(text=>!text.includes('.json')),'no filename in visible text, titles or accessible names');
            if(width===1480){
              assert(mixed.rects.every(r=>r.width===336),'all provider cards share the same compact desktop width');
              assert(mixed.stacks.every(r=>Math.abs(r.y-mixed.stacks[0].y)<1),'all three independent stacks start on the same desktop row');
              assert(mixed.stacks[0].x<mixed.stacks[1].x&&mixed.stacks[1].x<mixed.stacks[2].x);
              assert(Math.max(...mixed.rects.slice(1,4).map(r=>r.height))-Math.min(...mixed.rects.slice(1,4).map(r=>r.height))<1,'ordinary cards have standard height');
              assert(mixed.rects[5].height>mixed.rects[4].height,'multi-model cards grow naturally without clipping');
              if(process.env.DASHBOARD_BASELINE_HTML){
                const baseline=await context.newPage();
                try{
                  await baseline.setContent(fixture(fs.readFileSync(process.env.DASHBOARD_BASELINE_HTML,'utf8')));
                  await baseline.evaluate(()=>visualFixture.show('mixed'));
                  const before=await baseline.locator('#quota-cards article').evaluateAll(cards=>cards.map(el=>{const r=el.getBoundingClientRect();return {id:el.dataset.authId,width:r.width,height:r.height};}));
                  assert.equal(before.length,6);
                  for(let i=0;i<5;i++)assert(mixed.rects[i].width<before[i].width&&mixed.rects[i].height<=before[i].height*.9,'each ordinary card is narrower and at least 10% shorter than v0.1.26');
                  report.push({key,tab:'v0.1.26-comparison',before,after:mixed.rects});
                  await baseline.screenshot({path:path.join(directory,'1480-v0.1.26-mixed-accounts.png'),fullPage:true});
                }finally{await baseline.close();}
              }
            }else{
              assert(mixed.stacks.every((r,i)=>!i||r.y>mixed.stacks[i-1].bottom),'mobile provider stacks follow one another without overflow');
            }
            await page.screenshot({path:path.join(directory,`${width}-mixed-accounts.png`),fullPage:true});
            report.push({key,tab:'mixed',...mixed,labels:undefined});
            await page.locator('#tab-requests').click();
            const requestIdentity=await page.locator('#credential-table,#recent-table').evaluateAll(roots=>roots.flatMap(root=>[...root.querySelectorAll('*')].flatMap(el=>[el.textContent,el.title,el.getAttribute('aria-label')]).filter(Boolean)));
            assert(requestIdentity.every(text=>!text.includes('.json')),'request-page identities also hide filenames');
            await page.locator('#tab-quota').click();
            await page.locator('#account-sort').selectOption('name');
            assert(await page.locator('.provider-accounts').evaluateAll(stacks=>stacks.every(stack=>{const names=[...stack.querySelectorAll('.account')].map(el=>el.textContent);return names.every((name,i)=>!i||names[i-1].localeCompare(name,'zh-CN',{numeric:true})<=0);})));
            await page.locator('#account-sort').selectOption('remaining');
            assert.deepEqual(await page.locator('.provider-stack').first().locator('article').evaluateAll(cards=>cards.map(el=>el.dataset.authId.split('-')[1])),['d4e5','b2c3','c3d4','a1b2']);
            await page.locator('#account-sort').selectOption('default');
            // Same 4+1+1 data, real controls and deferred replies: no production API.
            await page.evaluate(()=>visualFixture.startActions());
            const first=page.locator('.account-card').first(),sibling=page.locator('.account-card').nth(1);
            await page.evaluate(()=>visualFixture.held='refresh-account');await first.locator('.account-refresh').click();
            assert.equal(await first.locator('.account-refresh').getAttribute('aria-busy'),'true');assert(!(await sibling.locator('.account-refresh').isDisabled()));
            await page.locator('#language').selectOption('en');assert.match(await first.locator('.account-refresh').getAttribute('aria-label'),/Refreshing/);
            await page.evaluate(()=>visualFixture.release());await first.locator('.account-refresh[aria-busy=false]').waitFor();
            await page.locator('#language').selectOption('zh');
            await page.evaluate(()=>visualFixture.failure='refresh-account');await first.locator('.account-refresh').click();
            await first.locator('.account-error').filter({hasText:'未找到账户'}).waitFor();
            await page.evaluate(()=>visualFixture.held='status');await first.locator('.account-switch').click();
            assert.equal(await first.locator('.account-switch').getAttribute('aria-busy'),'true');assert.equal(await first.locator('.account-switch').getAttribute('aria-checked'),'false');
            assert(!(await sibling.locator('.account-switch').isDisabled()));
            await page.evaluate(()=>visualFixture.release());await first.locator('.account-switch[aria-busy=false]').waitFor();
            assert(await first.evaluate(el=>el.classList.contains('account-disabled')));
            await first.locator('.account-switch').click();await first.locator('.account-switch[aria-checked=true][aria-busy=false]').waitFor();
            const reset=first.locator('.account-reset');await reset.focus();await page.keyboard.press('Enter');
            await page.locator('.reset-option').first().waitFor();assert.equal(await page.locator('.reset-option').first().getAttribute('aria-checked'),'true');
            await page.keyboard.press('ArrowDown');assert.equal(await page.locator('.reset-option').nth(1).getAttribute('aria-checked'),'true');
            await page.keyboard.press('Escape');assert(!(await page.locator('#reset-dialog').isVisible()));assert(await reset.evaluate(el=>el===document.activeElement));
            assert.equal(await page.evaluate(()=>visualFixture.calls.filter(c=>c.route==='consume-reset').length),0);
            await reset.click();await page.locator('.reset-option').first().waitFor();
            await page.evaluate(()=>visualFixture.held='consume-reset');await page.locator('#reset-confirm').click();
            assert.equal(await reset.getAttribute('aria-busy'),'true');assert(await first.locator('.account-refresh').isDisabled());
            await page.evaluate(()=>visualFixture.release());
            await page.waitForFunction(()=>document.querySelector('.account-reset')?.getAttribute('aria-busy')==='false');
            assert.match(await first.locator('.account-note').textContent(),/没有需要重置/);
            const calls=await page.evaluate(()=>visualFixture.calls),accountID='codex-a1b2-demo-primary@example.test-team.json';
            assert(calls.filter(c=>c.route==='refresh-account'||c.route==='reset-credits'||c.route==='consume-reset').every(c=>c.body.auth_id===accountID));
            assert.deepEqual(calls.filter(c=>c.route==='status').map(c=>c.body),[{name:accountID,disabled:true},{name:accountID,disabled:false}]);
            assert.equal(calls.find(c=>c.route==='consume-reset').body.credit_id,'demo-first');
            report.push({key,tab:'mixed-actions',refresh:true,refreshError:true,toggleBothDirections:true,resetCancelKeyboard:true,resetConfirm:true});
            await page.evaluate(() => visualFixture.show('long'));
            assert(await page.locator('.account').first().evaluate(el=>el.scrollWidth>el.clientWidth&&el.textContent===el.title&&el.textContent===el.getAttribute('aria-label')));
            await page.locator('.account').first().focus();await page.keyboard.press('Tab');await page.keyboard.press('Shift+Tab');assert(await page.locator('.account').first().evaluate(el=>el===document.activeElement&&el.matches(':focus-visible')));
            await page.evaluate(() => visualFixture.show('many'));
            const density = await page.evaluate(() => {
              const rect = el => {const r=el.getBoundingClientRect();return {x:r.x,y:r.y,width:r.width,height:r.height,bottom:r.bottom};};
              const cards=[...document.querySelectorAll('#quota-cards .panel')];
              return {viewport:innerWidth,pageWidth:document.documentElement.scrollWidth,cards:cards.length,
                columns:getComputedStyle(document.querySelector('#quota-cards')).gridTemplateColumns.split(' ').length,
                fullyVisible:cards.filter(el=>{const r=rect(el);return r.y>=0&&r.bottom<=innerHeight;}).length,
                card:rect(cards.find(el=>!el.classList.contains('account-disabled'))),
                browser:rect(document.querySelector('.account-browser')),
                toolbar:rect(document.querySelector('.toolbar')),
                invalid:cards.filter(el=>el.scrollWidth>el.clientWidth+1).map(el=>el.dataset.authId),
                windows:cards.map(el=>el.querySelectorAll('.window').length)};
            });
            assert.equal(density.cards,12);assert.equal(density.pageWidth,width);assert.deepEqual(density.invalid,[]);
            assert(density.windows.every(n=>n===2),'both quota windows remain directly visible');
            assert(density.card.width>0&&density.card.height>0&&density.card.height<280,'normal account is visible and compact without reducing metric type');
            if (width === 1480) {
              assert(density.columns>=1);assert(density.fullyVisible>=3,'a provider stack fits three complete compact cards in the first 1000px screen');
              assert(density.browser.height<=72,'desktop browsing controls stay within two short lines');
              await page.screenshot({path:path.join(directory,'1480-compact-many.png'),fullPage:false});
            } else {
              assert.equal(density.columns,1);assert(density.browser.height<=150,'mobile filters stay compact');
              assert(await page.locator('.account-refresh').first().evaluate(el=>el.getBoundingClientRect().height>=36));
            }
            report.push({key,tab:'many',...density});
            await page.locator('#account-search').fill('demo-credential-133');
            assert.equal(await page.locator('#quota-cards .panel').count(),1);
            assert.equal(await page.locator('#account-page').textContent(),'1 / 1');
            assert(await page.locator('.account-reset').isVisible());
            await page.locator('#account-clear').click();await page.locator('#account-next').click();
            assert.equal(await page.locator('#account-page').textContent(),'2 / 12');
            await page.locator('#account-next').click();await page.locator('#account-next').click();
            assert.deepEqual(await page.locator('.provider-stack').evaluateAll(stacks=>stacks.map(el=>[el.dataset.provider,el.querySelectorAll('article').length])),[['claude',10],['codex',2]],'shared page boundary preserves adjacent provider sub-stacks');
            await page.locator('#account-size').selectOption('24');
            assert.equal(await page.locator('#quota-cards .panel').count(),24);
            await page.locator('#account-state').selectOption('disabled');
            assert.equal(await page.locator('#quota-cards .panel.account-disabled').count(),16);
            await page.locator('#account-sort').selectOption('remaining');
            assert.equal(await page.locator('#account-page').textContent(),'1 / 1');
            await page.locator('#account-clear').click();
            await page.evaluate(() => visualFixture.show('groups'));
            const groups=await page.locator('.has-groups').evaluateAll(cards=>cards.map(el=>({width:el.getBoundingClientRect().width,windows:el.querySelectorAll('.window').length,overflow:el.scrollWidth>el.clientWidth+1,folds:el.querySelectorAll('details').length})));
            assert.equal(groups.length,3);assert(groups.every(el=>el.windows===4&&!el.overflow&&!el.folds));
            if(width===1480)assert(groups.every(el=>el.width===336),'multi-model accounts use the same standard desktop width');
          }
          await page.evaluate(() => visualFixture.show('ready'));
          await page.emulateMedia({reducedMotion: 'no-preference'});
          assert.equal(await page.evaluate(() => document.getAnimations().length), 0);
          assert.deepEqual(errors, [], `${key}: browser errors`);assert.deepEqual(requests, [], `${key}: unexpected network requests`);
        } finally { await context.close(); }
      }
      fs.writeFileSync(path.join(directory, 'measurements.json'), JSON.stringify(report, null, 2));
      t.diagnostic(`Rendered measurements and screenshots: ${directory}`);
    });
}
