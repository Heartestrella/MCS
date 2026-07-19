const puppeteer = require('puppeteer-core')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
;(async () => {
  const browser = await puppeteer.launch({
    executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
    headless: 'new', args: ['--no-sandbox', '--ignore-certificate-errors'],
    defaultViewport: { width: 1440, height: 900 },
  })
  // 先拿实例 id,算房间号
  const page = await browser.newPage()
  await page.goto('http://127.0.0.1:8145/', { waitUntil: 'networkidle2' })
  const instId = await page.evaluate(async () => (await (await fetch('/api/instances')).json())[0]?.id)
  const room = instId.slice(0, 8).toUpperCase()
  console.log('inst:', instId, 'room:', room)

  // 两个客户端进入该实例专属房间
  const c1 = await browser.newPage()
  await c1.goto(`http://127.0.0.1:8145/talk/#/room/${room}`, { waitUntil: 'networkidle2' })
  const c2 = await browser.newPage()
  await c2.goto(`http://127.0.0.1:8145/talk/#/room/${room}`, { waitUntil: 'networkidle2' })
  await sleep(2000)

  const rooms = await page.evaluate(async () => await (await fetch('/api/talk/rooms')).json())
  console.log('rooms api:', JSON.stringify(rooms))

  // 主页卡片刷新后应显示「语音房 2 人」
  await page.bringToFront()
  await sleep(4500) // 等 loadAll 周期
  const cardHasTag = await page.evaluate(() => document.body.innerText.includes('语音房 2 人'))
  console.log('home card tag:', cardHasTag)
  await page.screenshot({ path: 'shots/talk-count-home.png' })

  // 联机 tab 徽标
  await page.evaluate((id) => openConsole(id, 'net'), instId)
  await sleep(2000)
  const netBadge = await page.evaluate(() => {
    const el = document.getElementById('talkCount')
    return el && el.style.display !== 'none' ? el.textContent : 'hidden'
  })
  console.log('net tab badge:', netBadge)
  await page.screenshot({ path: 'shots/talk-count-net.png' })
  await browser.close()
})().catch((e) => { console.error('FAIL', e.message); process.exit(1) })
