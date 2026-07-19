// 密码 + 进出房 activity 验证
const WebSocket = require('ws')
function enc(ev, args) {
  const h = Buffer.from(JSON.stringify({ ev, args, bins: [] }))
  const b = Buffer.alloc(2); b.writeUInt16BE(h.length)
  return Buffer.concat([b, h])
}
function dec(buf) {
  const b = Buffer.from(buf); const hl = b.readUInt16BE(0)
  return JSON.parse(b.subarray(2, 2 + hl).toString())
}
async function client() {
  const ws = new WebSocket('ws://127.0.0.1:8145/talk/ws')
  const c = { ws, events: [] }
  ws.on('message', (d) => c.events.push(dec(d)))
  c.emit = (ev, ...a) => ws.send(enc(ev, a))
  c.wait = (ev, ms = 3000) => new Promise((res, rej) => {
    const t0 = Date.now()
    const iv = setInterval(() => {
      const f = c.events.find((e) => e.ev === ev)
      if (f) { clearInterval(iv); res(f) }
      else if (Date.now() - t0 > ms) { clearInterval(iv); rej(new Error('timeout ' + ev)) }
    }, 50)
  })
  await new Promise((r) => ws.on('open', r))
  return c
}
;(async () => {
  // 1) 无密码被拒
  const a = await client()
  a.emit('join', { room: 'C1FDDB54', name: 'NoPw' })
  console.log('no-pw rejected:', (await a.wait('auth-required')).args[0].reason)
  // 2) 带密码进入
  a.events = []
  a.emit('join', { room: 'C1FDDB54', name: 'WithPw', password: 'room123' })
  await a.wait('joined')
  console.log('with-pw joined: true')
  await new Promise((r) => setTimeout(r, 800))
  // 3) activity 出现进房动态
  const acts = await (await fetch('http://127.0.0.1:8145/api/activity')).json()
  console.log('activity join:', acts.some((x) => x.text.includes('语音房有人进入')))
  // 4) 离开 → 无人动态
  a.ws.close()
  await new Promise((r) => setTimeout(r, 1000))
  const acts2 = await (await fetch('http://127.0.0.1:8145/api/activity')).json()
  console.log('activity empty:', acts2.some((x) => x.text.includes('语音房已无人')))
  // 5) 清除密码
  await fetch('http://127.0.0.1:8145/api/instances/c1fddb54b527/talkpw', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: '' })
  })
  const b = await client()
  b.emit('join', { room: 'C1FDDB54', name: 'Open' })
  await b.wait('joined')
  console.log('after clear, open join: true')
  b.ws.close()
  process.exit(0)
})().catch((e) => { console.error('FAIL', e.message); process.exit(1) })
