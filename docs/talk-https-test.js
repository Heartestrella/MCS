const puppeteer = require('puppeteer-core')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
;(async () => {
  const browser = await puppeteer.launch({
    executablePath: 'C:/Program Files/Google/Chrome/Application/chrome.exe',
    headless: 'new',
    args: ['--no-sandbox', '--ignore-certificate-errors',
      '--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream'],
    defaultViewport: { width: 1280, height: 800 },
  })
  const page = await browser.newPage()
  page.on('console', (m) => { if (m.type() === 'error') console.log('err:', m.text().slice(0, 120)) })
  // 用局域网 IP 走 HTTPS —— 模拟朋友的电脑
  await page.goto('https://192.168.1.8:8446/talk/#/room/HTTPSTEST', { waitUntil: 'networkidle2' })
  await sleep(2500)

  const env = await page.evaluate(() => ({
    secure: window.isSecureContext,
    mediaDevices: !!navigator.mediaDevices,
    videoEncoder: typeof VideoEncoder !== 'undefined',
    connected: document.body.innerText.includes('已连接'),
  }))
  console.log('env:', JSON.stringify(env))

  // 点开麦克风,看有没有报错/状态变化
  const micBtn = await page.evaluate(() => {
    const b = [...document.querySelectorAll('button')].find((x) => /麦克风/.test(x.textContent))
    if (!b) return 'not-found'
    b.click()
    return b.textContent.trim()
  })
  await sleep(2500)
  const after = await page.evaluate(() => {
    const b = [...document.querySelectorAll('button')].find((x) => /麦克风/.test(x.textContent))
    return { btn: b?.textContent.trim(), err: document.body.innerText.includes('无法访问') }
  })
  console.log('mic before:', micBtn, '| after:', JSON.stringify(after))
  await page.screenshot({ path: 'shots/talk-https-mic.png' })
  await browser.close()
})().catch((e) => { console.error('FAIL', e.message); process.exit(1) })
