const puppeteer = require('puppeteer-core')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
;(async () => {
  const browser = await puppeteer.launch({
    executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
    headless: 'new', args: ['--no-sandbox'],
    defaultViewport: { width: 1440, height: 900 },
  })
  const page = await browser.newPage()
  await page.goto('http://127.0.0.1:8145/', { waitUntil: 'networkidle2' })
  await sleep(2000)
  const instId = await page.evaluate(async () => (await (await fetch('/api/instances')).json())[0]?.id)
  console.log('inst:', instId)
  await page.evaluate((id) => openConsole(id, 'net'), instId)
  await sleep(2500)
  await page.screenshot({ path: 'shots/inst-net.png' })
  // 点「进入语音房」→ 新标签
  const [popup] = await Promise.all([
    new Promise((res) => browser.once('targetcreated', (t) => res(t.page()))),
    page.evaluate(() => talkOpen()),
  ])
  const talkPage = await popup
  await sleep(3000)
  const url = talkPage.url()
  console.log('talk url:', url)
  const roomOk = await talkPage.evaluate(() => document.body.innerText.includes('已连接'))
  console.log('talk connected:', roomOk)
  await talkPage.screenshot({ path: 'shots/talk-from-panel.png' })
  await browser.close()
})().catch((e) => { console.error('FAIL', e.message); process.exit(1) })
