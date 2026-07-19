const puppeteer = require('puppeteer-core')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
;(async () => {
  const browser = await puppeteer.launch({
    executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
    headless: 'new', args: ['--no-sandbox', '--ignore-certificate-errors'],
    defaultViewport: { width: 900, height: 900 },
  })
  // 先放一个客户端进语音房,让按钮显示人数
  const c1 = await browser.newPage()
  await c1.goto('http://127.0.0.1:8145/talk/#/room/C1FDDB54', { waitUntil: 'networkidle2' })
  await sleep(1500)

  const p = await browser.newPage()
  await p.goto('http://127.0.0.1:8145/s/fd4ea2f6d6e3cb8c', { waitUntil: 'networkidle2' })
  await sleep(2000)
  const btn = await p.evaluate(() => {
    const b = document.getElementById('talkBtn')
    return { visible: b && getComputedStyle(b).display !== 'none', text: b?.textContent }
  })
  console.log('talk button:', JSON.stringify(btn))

  // 点击按钮 → 新标签应打开语音房
  const [popup] = await Promise.all([
    new Promise((res) => browser.once('targetcreated', (t) => res(t.page()))),
    p.click('#talkBtn'),
  ])
  const talkPage = await popup
  await sleep(2500)
  console.log('opened:', talkPage.url())
  const joined = await talkPage.evaluate(() => document.body.innerText.includes('已连接'))
  console.log('room connected:', joined)
  await p.screenshot({ path: 'shots/status-talk.png' })
  await browser.close()
})().catch((e) => { console.error('FAIL', e.message); process.exit(1) })
