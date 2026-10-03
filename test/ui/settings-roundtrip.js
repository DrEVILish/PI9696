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
// Switches are toggled by click. WiFi fields, the brightness slider and the
// tint colour picker are skipped (WiFi needs all fields at once; the other
// two have no Enter and their own checks). A change the server refuses must
// leave the field showing the server's value; one action must never send
// conflicting saves. A rapid-spin case checks number boxes keep the last value.
const { chromium } = require('playwright');
const fs = require('fs');
const BASE = process.env.PI9696_URL || 'http://127.0.0.1:18080';
const SKIP = new Set(['wifiSsid', 'wifiPass', 'wifiEnabled', 'tintcolor', 'brightnessRange']);
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
    for (const c of ids) if (c.type !== 'hidden' && c.type !== 'range' && c.type !== 'color' && !SKIP.has(c.id)) controls.push({ pane, ...c });
  }
  console.log('controls:', controls.map(c => `${c.pane}/${c.id || c.container + '>' + c.name}`).join(' '));
  const valueOf = async (c) => page.evaluate(({ pane, container, name, id }) => {
    const root = document.getElementById('pane-' + pane);
    const el = id ? document.getElementById(id) : (document.getElementById(container) || root).querySelector(`[name="${name}"]`);
    if (!el) return null;
    if (el.type === 'checkbox') return el.checked ? 'on' : 'off';
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
    if (c.type === 'checkbox') {
      target = before === 'on' ? 'off' : 'on';
      await page.locator(`label.switch[for="${c.id}"]`).click(); // the input itself is visually hidden
    } else if (c.tag === 'SELECT') {
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
    // The field must always agree with what the server stored (a refused
    // change, e.g. monitoring with no source, legitimately stays put), and a
    // single user action must never send conflicting saves (an Enter in a
    // number box may send the same save twice; the server ignores repeats).
    const refused = server !== target;
    const bad = now !== server || later !== server || posts.length === 0 || !posts.every(p => p === posts[0]);
    results.push({ ctl: `${c.pane}/${c.id || c.container + '>' + c.name}`, before, target, now, later, server, refused, posts: posts.slice(), bad });
    console.log((bad ? 'MISMATCH ' : 'ok       ') + JSON.stringify(results[results.length - 1]));
    // restore
    await open(c.pane);
    if (!refused) {
      if (c.type === 'checkbox') { await page.locator(`label.switch[for="${c.id}"]`).click(); }
      else if (c.tag === 'SELECT') { await loc.selectOption({ label: before }); }
      else { await loc.fill(before); await loc.press('Enter'); }
    }
    await page.waitForTimeout(800);
  }
  // Rapid spinner clicks on a number box: changes made while a save is in
  // flight must not be lost - the field and the server end on the last one.
  {
    await page.goto(BASE + '/'); await page.waitForTimeout(500);
    await open('audio');
    const loc = page.locator('#channelsInput');
    const start = Number(await loc.inputValue());
    posts.length = 0;
    await loc.focus();
    for (let i = 0; i < 3; i++) await page.keyboard.press('ArrowUp');
    await page.waitForTimeout(2500);
    const shown = Number(await loc.inputValue());
    await page.goto(BASE + '/'); await page.waitForTimeout(500); await open('audio');
    const server = Number(await page.locator('#channelsInput').inputValue());
    const bad = shown !== start + 3 || server !== start + 3;
    results.push({ ctl: 'audio/channelsInput rapid x3', before: start, target: start + 3, now: shown, server, posts: posts.slice(), bad });
    console.log((bad ? 'MISMATCH ' : 'ok       ') + JSON.stringify(results[results.length - 1]));
    await page.locator('#channelsInput').fill(String(start)); await page.locator('#channelsInput').press('Enter');
    await page.waitForTimeout(800);
  }
  // A change made elsewhere (front panel, another browser, HyperDeck) must
  // show when the settings sheet is next opened, without a reload, and in
  // the main window's Status panel.
  {
    await page.goto(BASE + '/'); await page.waitForTimeout(500);
    await open('audio');
    const start = Number(await page.locator('#channelsInput').inputValue());
    await page.click('#settingsClose'); await page.waitForTimeout(200);
    const other = start === 3 ? 6 : 3;
    await page.request.post(BASE + '/api/settings/channels', { form: { count: String(other) } });
    await page.waitForTimeout(4500);   // let the telemetry push refresh the main window
    await page.click('#settingsBtn'); await page.waitForTimeout(800);
    await page.click('#tab-audio'); await page.waitForTimeout(200);
    const shown = Number(await page.locator('#channelsInput').inputValue());
    const mainText = await page.locator('#config').innerText();
    const mainOK = new RegExp('\\b' + other + '\\s*ch|Channels\\W+' + other + '\\b', 'i').test(mainText);
    const bad = shown !== other || !mainOK;
    results.push({ ctl: 'audio/channelsInput changed elsewhere', before: start, target: other, now: shown, mainWindow: mainOK, bad });
    console.log((bad ? 'MISMATCH ' : 'ok       ') + JSON.stringify(results[results.length - 1]));
    if (!mainOK) console.log('main window text:', JSON.stringify(mainText.slice(0, 300)));
    await page.request.post(BASE + '/api/settings/channels', { form: { count: String(start) } });
  }
  console.log('errors:', JSON.stringify(errors));
  await browser.close();
  const bad = results.filter(r => r.bad).length;
  console.log(`${results.length} controls, ${bad} mismatched, ${errors.length} page errors`);
  process.exit(bad || errors.length ? 1 : 0);
})();
