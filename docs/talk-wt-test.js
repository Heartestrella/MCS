const puppeteer = require('puppeteer-core')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
;(async () => {
  const browser = await puppeteer.launch({
    executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
    headless: 'new',
    args: ['--no-sandbox', '--ignore-certificate-errors',
      '--ignore-certificate-errors-spki-list',
      '--origin-to-force-quic-on=192.168.1.8:4433',
      '--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream',
      '--auto-select-desktop-capture-source=Entire screen',
      '--autoplay-policy=no-user-gesture-required'],
    defaultViewport: { width: 1280, height: 800 },
  })
  const wtLogs = { p1: [], p2: [] }
  const p1 = await browser.newPage()
  p1.on('console', (m) => { const t = m.text(); if (t.includes('[wt]')) { wtLogs.p1.push(t); } })
  await p1.goto('https://192.168.1.8:8446/talk/#/room/WTE2E', { waitUntil: 'networkidle2' })
  const p2 = await browser.newPage()
  p2.on('console', (m) => { const t = m.text(); if (t.includes('[wt]')) { wtLogs.p2.push(t); } })
  await p2.goto('https://192.168.1.8:8446/talk/#/room/WTE2E', { waitUntil: 'networkidle2' })
  await sleep(2000)

  // 双方都切 UDP(点 TCP chip)
  for (const p of [p1, p2]) {
    await p.evaluate(() => {
      const chip = [...document.querySelectorAll('button, .chip, [class*=chip]')].find((x) => /TCP/.test(x.textContent))
      chip?.click()
    })
  }
  await sleep(4000)
  console.log('P1 wt logs:', JSON.stringify(wtLogs.p1.slice(0, 5)))
  console.log('P2 wt logs:', JSON.stringify(wtLogs.p2.slice(0, 5)))

  // P1 开始共享屏幕
  await p1.evaluate(() => [...document.querySelectorAll('button')].find((x) => /共享屏幕/.test(x.textContent))?.click())
  await sleep(800)
  await p1.evaluate(() => [...document.querySelectorAll('button')].find((x) => /开始共享/.test(x.textContent))?.click())
  await sleep(8000)

  console.log('P1 wt logs after share:', JSON.stringify(wtLogs.p1.slice(-4)))
  console.log('P2 wt logs after share:', JSON.stringify(wtLogs.p2.slice(-4)))

  // P2 收到画面?走的什么通道?
  const p2res = await p2.evaluate(async () => {
    const c = [...document.querySelectorAll('canvas')].sort((a, b) => b.width * b.height - a.width * a.height)[0]
    const banner = document.body.innerText.match(/UDP[^\n]*/)?.[0] || ''
    if (!c) return { canvas: 'none', banner }
    const a = c.toDataURL().length
    await new Promise((r) => setTimeout(r, 1500))
    return { canvas: `${c.width}x${c.height}`, drawing: a !== c.toDataURL().length || a > 5000, banner }
  })
  console.log('P2 view:', JSON.stringify(p2res))
  await p2.screenshot({ path: 'shots/talk-wt-e2e.png' })
  await browser.close()
})().catch((e) => { console.error('FAIL', e.message); process.exit(1) })
