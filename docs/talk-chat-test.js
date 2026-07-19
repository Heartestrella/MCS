const puppeteer = require('puppeteer-core')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
;(async () => {
  const browser = await puppeteer.launch({
    executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
    headless: 'new', args: ['--no-sandbox'],
    defaultViewport: { width: 1280, height: 800 },
  })
  const p1 = await browser.newPage()
  await p1.goto('http://127.0.0.1:8145/talk/#/room/GOTEST3', { waitUntil: 'networkidle2' })
  const p2 = await browser.newPage()
  await p2.goto('http://127.0.0.1:8145/talk/#/room/GOTEST3', { waitUntil: 'networkidle2' })
  await sleep(2000)

  await p1.bringToFront()
  // 直接对准聊天输入框(placeholder 含 "输入消息")
  const ok = await p1.evaluate(() => {
    const el = [...document.querySelectorAll('input')].find((i) => (i.placeholder || '').includes('输入消息'))
    if (!el) return false
    el.focus()
    return true
  })
  console.log('input focused:', ok)
  await p1.keyboard.type('go relay works!')
  await p1.keyboard.press('Enter')
  await sleep(1500)
  const got = await p2.evaluate(() => document.body.innerText.includes('go relay works'))
  console.log('P2 received chat:', got)
  await p2.screenshot({ path: 'shots/talk-chat.png' })
  await browser.close()
})().catch((e) => { console.error('FAIL', e.message); process.exit(1) })
