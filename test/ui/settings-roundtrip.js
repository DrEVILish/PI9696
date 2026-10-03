// Settings round-trip check: changes every WebUI settings control the way a
// user does (pick a value / type one, press Enter), then requires the field
// to show the new value immediately, after two telemetry pushes, and after a
// reload (server truth); then restores it. Exits non-zero on any mismatch or
// page error. Catches the htmx settle bug that showed the OLD value in a
// field saved with Enter.
//
// Run from a dev box with Playwright, against a SIM instance only (it
// changes settings, including the device name):
//   PI9696_SIM=1 PI9696_REMOTE_PORT=18080 ./pi9696 &   # note the access code
//   PI9696_URL=http://127.0.0.1:18080 PI9696_TOKEN=XXXXXXXX node test/ui/settings-roundtrip.js
// WiFi fields and the tint colour picker are skipped (WiFi needs all fields
// at once; the tint has its own check).
const { chromium } = require('playwright');
const fs = require('fs');
const BASE = process.env.PI9696_URL || 'http://127.0.0.1:18080';
const SKIP = new Set(['wifiSsid', 'wifiPass', 'wifiEnabled', 'tintcolor']);
(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const errors = [];
  page.on('pageerror', e => errors.push(String(e)));
  page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
  const token = (process.env.PI9696_TOKEN || fs.readFileSync(process.env.PI9696_TOKEN_FILE || '/dev/stdin', 'utf8')).replace(/\s/g, '');
  await page.request.post(BASE + '/login', { form: { token }, maxRedirects: 0 });
  const posts = [];
  page.on('request', r => { if (r.method() === 'POST') posts.push(r.url().replace(BASE, '') + ' ' + (r.postData() || '')); });
  const open = async (pane) => {
    if (!(await page.locator('#settingsBtn').isVisible())) return;
    if (!(await page.locator('#tab-' + pane).isVisible())) { await page.click('#settingsBtn'); await page.waitForTimeout(300); }
    await page.click('#tab-' + pane); await page.waitForTimeout(200);
  };
  const panes = ['device','audio','metering','metadata','transport','display','demo','logging'];
  await page.goto(BASE + '/');
  // Inventory controls per pane.
  const controls = [];
  for (const pane of panes) {
    const ids = await page.$$eval(`#pane-${pane} select, #pane-${pane} input`, els => els.map(e => ({
      id: e.id, tag: e.tagName, type: e.type || '', name: e.name,
      sel: e.id ? '#' + e.id : `${e.tagName.toLowerCase()}[name="${e.name}"]`,
      container: (e.closest('[id]:not(form)') || {}).id || ''
    })));
    for (const c of ids) if (c.type !== 'hidden' && c.type !== 'checkbox' && c.type !== 'range' && c.type !== 'color' && !SKIP.has(c.id)) controls.push({ pane, ...c });
  }
  console.log('controls:', controls.map(c => `${c.pane}/${c.id || c.container + '>' + c.name}`).join(' '));
  const valueOf = async (c) => page.evaluate(({ pane, container, name, id }) => {
    const root = document.getElementById('pane-' + pane);
    const el = id ? document.getElementById(id) : (document.getElementById(container) || root).querySelector(`[name="${name}"]`);
    if (!el) return null;
    return el.tagName === 'SELECT' ? el.options[el.selectedIndex].text : el.value;
  }, c);
  const results = [];
  for (const c of controls) {
    await page.goto(BASE + '/'); await page.waitForTimeout(500);
    await open(c.pane);
    const loc = c.id ? page.locator('#' + c.id) : page.locator(`#${c.container} [name="${c.name}"]`).first();
    const before = await valueOf(c);
    let target;
    posts.length = 0;
    if (c.tag === 'SELECT') {
      const opts = await loc.evaluate(el => [...el.options].map(o => o.text));
      target = opts.find(o => o !== before);
      await loc.focus();
      await loc.selectOption({ label: target });
      await page.keyboard.press('Enter');
    } else if (c.type === 'number') {
      target = String(Number(before) === 2 ? 4 : 2);
      await loc.fill(target); await loc.press('Enter');
    } else {
      target = before === 'Probe' ? 'Probe2' : 'Probe';
      await loc.fill(target); await loc.press('Enter');
    }
    await page.waitForTimeout(400);
    const now = await valueOf(c);
    await page.waitForTimeout(4500);   // past two telemetry pushes
    const later = await valueOf(c);
    await page.goto(BASE + '/'); await page.waitForTimeout(500); await open(c.pane);
    const server = await valueOf(c);
    const bad = now !== target || later !== target || server !== target;
    results.push({ ctl: `${c.pane}/${c.id || c.container + '>' + c.name}`, before, target, now, later, server, posts: posts.slice(), bad });
    console.log((bad ? 'MISMATCH ' : 'ok       ') + JSON.stringify(results[results.length - 1]));
    // restore
    await open(c.pane);
    if (c.tag === 'SELECT') { await loc.selectOption({ label: before }); }
    else { await loc.fill(before); await loc.press('Enter'); }
    await page.waitForTimeout(800);
  }
  console.log('errors:', JSON.stringify(errors));
  await browser.close();
  const bad = results.filter(r => r.bad).length;
  console.log(`${results.length} controls, ${bad} mismatched, ${errors.length} page errors`);
  process.exit(bad || errors.length ? 1 : 0);
})();
