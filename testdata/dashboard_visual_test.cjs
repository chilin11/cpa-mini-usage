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
      input: 24000, output: 1200, cache_read: 4000, cache_write: 200,
      total: 29400, cost: i === 3 ? null : .0028, missing_price: i === 3,
      ttft_ms: 420, latency_ms: 12400, failed: i === 4,
    })),
  };
}

function fixture() {
  assert.match(source, /^load\(\);$/m, 'fixture must suppress initial live load');
  const boot = `
window.visualFixture = {
  show(state='ready') {
    accountJobs.clear();accountErrors.clear();cardCache.clear();
    settingsJob=null;settingsError=null;requestJob=null;loading=false;headerLoading=false;
    filter='';$('provider').value='';phase='ready';key='';expanded='';recentPage=0;
    $('dashboard').classList.remove('needs-login');$('gate').classList.add('hide');setAlert('');
    data=${JSON.stringify(sample())};
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
  }
};
const params=new URLSearchParams(location.search);
setLanguage(params.get('lang')==='en'?'en':'zh',false);
visualFixture.show(params.get('state')||'ready');
setTab(params.get('tab')==='requests'?'requests':'quota');
`;
  const instrumented = source.replace(/^load\(\);$/m, '').replace(/\}\)\(\);\s*$/, boot + '\n})();');
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
  return html.replace('<head>', '<head>' + safety).replace('<body>', '<body>' + demoLabel)
    .replace('<script>' + source + '</script>', () => '<script>' + instrumented + '</script>');
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
    assert.match(css, /\.percent\{font-size:26px/);
    for (const name of ['observed-cost', 'estimate-cost']) assert.match(css, new RegExp('\\.' + name + '\\{[^}]*font-size:17px'));
    for (const name of ['observed-value', 'estimate-value']) assert.match(css, new RegExp('\\.' + name + '\\{font-size:14px'));
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
              assert.deepEqual(Object.values(measured.type), ['26px', '17px', '17px', '14px', '14px']);
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
