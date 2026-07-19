const puppeteer = require('puppeteer-core')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
;(async () => {
  const browser = await puppeteer.launch({
    executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
    headless: 'new', args: ['--no-sandbox'],
    defaultViewport: { width: 1440, height: 900 },
  })
  const p = await browser.newPage()
  p.on('dialog', async (d) => { await d.accept('uipass') })
  await p.goto('http://127.0.0.1:8145/', { waitUntil: 'networkidle2' })
  await sleep(1500)
  await p.evaluate(() => openConsole('c1fddb54b527', 'net'))
  await sleep(2000)
  const before = await p.evaluate(() => document.getElementById('talkPwBtn')?.textContent)
  await p.evaluate(() => talkPwToggle())
  await sleep(1500)
  const after = await p.evaluate(() => document.getElementById('talkPwBtn')?.textContent)
  console.log('btn before:', before, '| after set:', after)
  await p.screenshot({ path: 'shots/talk-pw-ui.png' })
  // 清掉,别把测试密码留下
  await fetch('http://127.0.0.1:8145/api/instances/c1fddb54b527/talkpw', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: '' })
  })
  await browser.close()
})().catch((e) => { console.error('FAIL', e.message); process.exit(1) })
