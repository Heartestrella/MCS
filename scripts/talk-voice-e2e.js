const puppeteer = require('puppeteer-core')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
;(async () => {
  const browser = await puppeteer.launch({
    executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
    headless: 'new',
    args: ['--no-sandbox', '--ignore-certificate-errors',
      '--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream',
      '--autoplay-policy=no-user-gesture-required'],
    defaultViewport: { width: 1280, height: 800 },
  })
  const p1 = await browser.newPage()
  await p1.goto('https://192.168.1.8:8446/talk/#/room/VOICEE2E', { waitUntil: 'networkidle2' })
  const p2 = await browser.newPage()
  await p2.goto('https://192.168.1.8:8446/talk/#/room/VOICEE2E', { waitUntil: 'networkidle2' })
  await sleep(2000)

  // P1 开麦(fake device 输出音调)
  await p1.evaluate(() => [...document.querySelectorAll('button')].find((x) => /麦克风/.test(x.textContent))?.click())
  await sleep(4000)

  // P2 检查:P1 的成员卡是否显示 micOn / 波形活动
  const p2state = await p2.evaluate(() => {
    const t = document.body.innerText
    return { sawMicPeer: /开麦|speaking|ON AIR/i.test(t) || !!document.querySelector('[class*=speaking], [class*=level], canvas'), text: t.slice(0, 60) }
  })
  console.log('P2 state:', JSON.stringify(p2state))
  // 更硬核:直接查 P2 收到的语音字节数(peers reactive 不易取,查波形 canvas 是否在画)
  const activity = await p2.evaluate(async () => {
    const c = document.querySelector('canvas')
    if (!c) return 'no-canvas'
    const a = c.toDataURL()
    await new Promise((r) => setTimeout(r, 1200))
    const b = c.toDataURL()
    return a === b ? 'static' : 'animating'
  })
  console.log('P2 waveform:', activity)
  await p2.screenshot({ path: 'shots/talk-voice-e2e.png' })
  await browser.close()
})().catch((e) => { console.error('FAIL', e.message); process.exit(1) })
