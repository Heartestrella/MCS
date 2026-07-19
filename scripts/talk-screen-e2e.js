const puppeteer = require('puppeteer-core')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
;(async () => {
  const browser = await puppeteer.launch({
    executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
    headless: 'new',
    args: ['--no-sandbox', '--ignore-certificate-errors',
      '--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream',
      '--auto-select-desktop-capture-source=Entire screen',
      '--autoplay-policy=no-user-gesture-required'],
    defaultViewport: { width: 1280, height: 800 },
  })
  const p1 = await browser.newPage()
  p1.on('console', (m) => { if (m.type() === 'error') console.log('P1:', m.text().slice(0, 120)) })
  await p1.goto('https://192.168.1.8:8446/talk/#/room/SCRE2E', { waitUntil: 'networkidle2' })
  const p2 = await browser.newPage()
  p2.on('console', (m) => { if (m.type() === 'error') console.log('P2:', m.text().slice(0, 120)) })
  await p2.goto('https://192.168.1.8:8446/talk/#/room/SCRE2E', { waitUntil: 'networkidle2' })
  await sleep(2000)

  // P1 打开共享菜单并开始共享
  await p1.evaluate(() => [...document.querySelectorAll('button')].find((x) => /共享屏幕/.test(x.textContent))?.click())
  await sleep(800)
  const started = await p1.evaluate(() => {
    const b = [...document.querySelectorAll('button')].find((x) => /开始共享/.test(x.textContent))
    if (b) { b.click(); return 'clicked' }
    return 'no-start-btn'
  })
  console.log('P1 start share:', started)
  await sleep(6000)

  const p1state = await p1.evaluate(() => document.body.innerText.match(/共享中|停止共享|↑\s*[\d.]+\s*[KM]B/)?.[0] || 'not-sharing')
  console.log('P1 state:', p1state)

  // P2:画面 canvas 是否持续更新
  const p2res = await p2.evaluate(async () => {
    const canvases = [...document.querySelectorAll('canvas')]
    const big = canvases.sort((a, b) => b.width * b.height - a.width * a.height)[0]
    if (!big) return { canvas: 'none' }
    const a = big.toDataURL().length
    await new Promise((r) => setTimeout(r, 1500))
    const b = big.toDataURL().length
    return { canvas: `${big.width}x${big.height}`, drawing: a !== b || a > 5000, bytes: [a, b] }
  })
  console.log('P2 screen view:', JSON.stringify(p2res))
  await p2.screenshot({ path: 'shots/talk-screen-e2e.png' })
  await browser.close()
})().catch((e) => { console.error('FAIL', e.message); process.exit(1) })
