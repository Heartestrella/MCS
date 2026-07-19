const puppeteer = require('puppeteer-core')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
;(async () => {
  const browser = await puppeteer.launch({
    executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
    headless: 'new',
    args: ['--no-sandbox', '--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream'],
    defaultViewport: { width: 1280, height: 800 },
  })
  const p1 = await browser.newPage()
  p1.on('console', (m) => { if (m.type() === 'error') console.log('P1 err:', m.text().slice(0, 150)) })
  await p1.goto('http://127.0.0.1:8145/talk/#/room/GOTEST', { waitUntil: 'networkidle2' })
  await sleep(2500)

  const p2 = await browser.newPage()
  await p2.goto('http://127.0.0.1:8145/talk/#/room/GOTEST', { waitUntil: 'networkidle2' })
  await sleep(2500)

  // 检查双方是否互相看见(participant 卡片数量)
  const count1 = await p1.evaluate(() => document.body.innerText)
  console.log('P1 sees peer名单包含2人:', /2\s*人|2 members/.test(count1) || count1.includes('YOU'))
  const cards1 = await p1.$$eval('[class*=participant], [class*=avatar], [class*=member]', (els) => els.length).catch(() => 0)
  console.log('P1 participant elements:', cards1)

  // 从 P1 发聊天,P2 收
  const sent = await p1.evaluate(() => {
    const input = document.querySelector('textarea, input[placeholder*="消息"], input[type=text]')
    if (!input) return 'no-input'
    input.value = 'go relay works!'
    input.dispatchEvent(new Event('input', { bubbles: true }))
    const btn = [...document.querySelectorAll('button')].find((b) => /发送|send/i.test(b.textContent))
    if (btn) { btn.click(); return 'clicked' }
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }))
    return 'entered'
  })
  console.log('P1 chat send:', sent)
  await sleep(1500)
  const got = await p2.evaluate(() => document.body.innerText.includes('go relay works'))
  console.log('P2 received chat:', got)

  await p1.screenshot({ path: 'shots/talk-room.png' })
  await browser.close()
  console.log('DONE')
})().catch((e) => { console.error('FAIL', e); process.exit(1) })
