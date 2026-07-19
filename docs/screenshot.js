const puppeteer = require('puppeteer-core');

const BASE = 'http://127.0.0.1:8145';
const OUT = __dirname + '/shots/';
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  const browser = await puppeteer.launch({
    executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
    headless: 'new',
    args: ['--no-sandbox', '--disable-gpu', '--force-device-scale-factor=1.25'],
    defaultViewport: { width: 1440, height: 900 },
  });
  const page = await browser.newPage();
  await page.goto(BASE, { waitUntil: 'networkidle2' });
  await sleep(4000); // 等性能图采样几帧

  const shot = async (name, wait = 1200) => {
    await sleep(wait);
    await page.screenshot({ path: OUT + name + '.png' });
    console.log('shot', name);
  };

  // 主页
  await shot('home', 3000);

  // 实例详情各 tab
  const instId = await page.evaluate(async () => {
    const r = await fetch('/api/instances');
    const d = await r.json();
    return d[0] && d[0].id;
  });
  if (instId) {
    await page.evaluate(id => openConsole(id), instId);
    await shot('inst-console', 2500);
    for (const [tab, name] of [['files','inst-files'], ['mods','inst-mods'], ['stats','inst-stats'], ['net','inst-net'], ['players','inst-players'], ['props','inst-props']]) {
      await page.evaluate(t => {
        const btn = document.querySelector(`#page-inst [data-ctab="${t}"]`);
        if (btn) btn.click();
      }, tab);
      await shot(name, 2000);
    }
    await page.evaluate(() => showPage('home'));
    await sleep(500);
  }

  // 模组市场
  await page.evaluate(() => showPage('mods'));
  await shot('mods', 4000);

  // 备份页
  await page.evaluate(() => showPage('backup'));
  await shot('backup', 1500);

  // 设置页
  await page.evaluate(() => showPage('settings'));
  await shot('settings', 1500);

  // 创建向导弹窗
  await page.evaluate(() => showPage('home'));
  await sleep(300);
  await page.evaluate(() => { if (typeof openCreate === 'function') openCreate(); });
  await shot('create', 2000);

  await browser.close();
})().catch(e => { console.error(e); process.exit(1); });
