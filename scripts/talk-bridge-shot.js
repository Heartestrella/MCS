const puppeteer = require('puppeteer-core')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
;(async () => {
  const browser = await puppeteer.launch({
    executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
    headless: 'new', args: ['--no-sandbox', '--ignore-certificate-errors'],
    defaultViewport: { width: 1280, height: 800 },
  })
  const p = await browser.newPage()
  await p.goto('http://127.0.0.1:8145/talk/#/room/C1FDDB54', { waitUntil: 'networkidle2' })
  await sleep(2000)
  // 从语音房发一条,再触发游戏 say 一条
  await p.evaluate(() => {
    const el = [...document.querySelectorAll('input')].find((i) => (i.placeholder || '').includes('输入消息'))
    el.focus()
  })
  await p.keyboard.type('打游戏了打游戏了')
  await p.keyboard.press('Enter')
  await fetch('http://127.0.0.1:8145/api/instances/c1fddb54b527/command', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ command: 'say 服务器晚上 8 点重启' })
  })
  await sleep(2500)
  const chatText = await p.evaluate(() => document.body.innerText.includes('[游戏] 服务器'))
  console.log('voice room sees game msg:', chatText)
  await p.screenshot({ path: 'shots/talk-bridge.png' })
  await browser.close()
})().catch((e) => { console.error('FAIL', e.message); process.exit(1) })
