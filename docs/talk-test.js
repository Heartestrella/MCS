// 端到端测试 talk.go 中转:两个客户端进同一房间,验证 join/peers/chat/voice/密码
const WebSocket = require('ws') ?? globalThis.WebSocket

function encodeFrame(ev, args) {
  const bins = []
  const jsonArgs = args.map((a) => {
    if (a instanceof ArrayBuffer) { bins.push(Buffer.from(a)); return { $bin: bins.length - 1 } }
    if (Buffer.isBuffer(a)) { bins.push(a); return { $bin: bins.length - 1 } }
    return a
  })
  const header = Buffer.from(JSON.stringify({ ev, args: jsonArgs, bins: bins.map((b) => b.length) }))
  const head = Buffer.alloc(2)
  head.writeUInt16BE(header.length)
  return Buffer.concat([head, header, ...bins])
}
function decodeFrame(buf) {
  const b = Buffer.from(buf)
  const hl = b.readUInt16BE(0)
  const header = JSON.parse(b.subarray(2, 2 + hl).toString())
  const bins = []
  let off = 2 + hl
  for (const bl of header.bins || []) { bins.push(b.subarray(off, off + bl)); off += bl }
  const revive = (n) => {
    if (n && typeof n === 'object') {
      if (typeof n.$bin === 'number') return bins[n.$bin]
      if (Array.isArray(n)) return n.map(revive)
      const o = {}; for (const k of Object.keys(n)) o[k] = revive(n[k]); return o
    }
    return n
  }
  return { ev: header.ev, args: (header.args || []).map(revive) }
}

function client(name) {
  const ws = new (require('ws'))('ws://127.0.0.1:8145/talk/ws')
  ws.binaryType = 'arraybuffer'
  const c = { ws, name, id: null, events: [], waiters: [] }
  ws.on('message', (data) => {
    const f = decodeFrame(data)
    if (f.ev === 'connect') c.id = f.args[0].id
    c.events.push(f)
    c.waiters = c.waiters.filter((w) => !w(f))
  })
  c.emit = (ev, ...args) => ws.send(encodeFrame(ev, args))
  c.wait = (ev, timeout = 3000) => new Promise((res, rej) => {
    const hit = c.events.find((f) => f.ev === ev)
    if (hit) return res(hit)
    const t = setTimeout(() => rej(new Error(`${name}: timeout waiting ${ev}`)), timeout)
    c.waiters.push((f) => { if (f.ev === ev) { clearTimeout(t); res(f); return true } return false })
  })
  return new Promise((res) => ws.on('open', () => res(c)))
}

;(async () => {
  const a = await client('A')
  const b = await client('B')
  await a.wait('connect'); await b.wait('connect')
  console.log('ids:', a.id, b.id)

  // A 创建带密码房间
  a.emit('join', { room: 'TEST01', name: 'Alice', setPassword: 'pw123', password: 'pw123' })
  await a.wait('joined')
  console.log('A joined ok')

  // B 无密码进 → 应被拒
  b.emit('join', { room: 'TEST01', name: 'Bob' })
  const auth = await b.wait('auth-required')
  console.log('B rejected:', auth.args[0].reason)

  // B 带对密码进
  b.events = []
  b.emit('join', { room: 'TEST01', name: 'Bob', password: 'pw123' })
  await b.wait('joined')
  const peers = await b.wait('peers')
  console.log('B peers:', JSON.stringify(peers.args[0].list))
  const pj = await a.wait('peer-joined')
  console.log('A saw peer-joined:', pj.args[0].name)

  // chat 广播(双方都收到)
  a.emit('chat', { text: 'hello from A', ts: Date.now() })
  const chatB = await b.wait('chat')
  console.log('B got chat:', chatB.args[0].name, chatB.args[0].text)

  // voice 二进制中转:A 发 640 字节 PCM → B 收到且带发送者 id
  const pcm = Buffer.alloc(640, 7)
  a.emit('voice', pcm)
  const v = await b.wait('voice')
  console.log('B got voice from', v.args[0], 'bytes:', v.args[1].length, 'byte0:', v.args[1][0])

  // video 带嵌套二进制(data 在 msg 对象里)——模拟 shim 的对象内 $bin
  const vmsg = encodeFrame('video', [{ type: 'key', ts: 1, data: { $bin: 0 }, config: { codec: 'vp8' } }])
  // 手工拼:header 已声明 bins
  const hdr = Buffer.from(JSON.stringify({ ev: 'video', args: [{ type: 'key', ts: 1, data: { $bin: 0 }, config: { codec: 'vp8' } }], bins: [4] }))
  const hb = Buffer.alloc(2); hb.writeUInt16BE(hdr.length)
  a.ws.send(Buffer.concat([hb, hdr, Buffer.from([9, 9, 9, 9])]))
  const vid = await b.wait('video')
  console.log('B got video from', vid.args[0], 'type:', vid.args[1].type, 'data bytes:', vid.args[1].data.length)

  // state 广播
  a.emit('state', { micOn: true, screenOn: false })
  const st = await b.wait('peer-state')
  console.log('B saw state:', JSON.stringify(st.args[0]))

  // rename
  a.emit('rename', { name: 'Alice2' })
  const rn = await b.wait('peer-renamed')
  console.log('B saw rename:', rn.args[0].name)

  // A 断开 → B 收 peer-left
  a.ws.close()
  const pl = await b.wait('peer-left')
  console.log('B saw peer-left:', pl.args[0].id === a.id ? 'OK' : 'MISMATCH')

  b.ws.close()
  console.log('ALL PASS')
  process.exit(0)
})().catch((e) => { console.error('FAIL:', e.message); process.exit(1) })
