// 双向桥接测试:WS 客户端进实例专属房间(C1FDDB54)
// 1) 语音房发 chat → 面板控制台应出现 tellraw(经 /api chat 记录验证)
// 2) 面板 /command say → 游戏聊天行 → 语音房应收到 [游戏] 服务器
const WebSocket = require('ws')

function encodeFrame(ev, args) {
  const header = Buffer.from(JSON.stringify({ ev, args, bins: [] }))
  const head = Buffer.alloc(2); head.writeUInt16BE(header.length)
  return Buffer.concat([head, header])
}
function decodeFrame(buf) {
  const b = Buffer.from(buf)
  const hl = b.readUInt16BE(0)
  return JSON.parse(b.subarray(2, 2 + hl).toString())
}

;(async () => {
  const ws = new WebSocket('ws://127.0.0.1:8145/talk/ws')
  const events = []
  ws.on('message', (d) => events.push(decodeFrame(d)))
  await new Promise((r) => ws.on('open', r))
  await new Promise((r) => setTimeout(r, 300))
  ws.send(encodeFrame('join', [{ room: 'C1FDDB54', name: 'BridgeTester' }]))
  await new Promise((r) => setTimeout(r, 500))

  // 1) 语音房 → 游戏
  ws.send(encodeFrame('chat', [{ text: 'hello from voice room', ts: Date.now() }]))
  await new Promise((r) => setTimeout(r, 1500))
  const chatLog = await (await fetch('http://127.0.0.1:8145/api/instances/c1fddb54b527/chat')).json()
  const bridged = chatLog.find((c) => c.text.includes('BridgeTester') && c.text.includes('hello from voice room'))
  console.log('voice->game bridged into chat log:', !!bridged, bridged ? JSON.stringify(bridged.text) : '')

  // 2) 游戏 → 语音房(用面板喊话 say 模拟游戏内聊天行)
  await fetch('http://127.0.0.1:8145/api/instances/c1fddb54b527/command', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ command: 'say hi voice room' })
  })
  await new Promise((r) => setTimeout(r, 2500))
  const gameMsg = events.find((e) => e.ev === 'chat' && e.args[0]?.name?.startsWith('[游戏]') && e.args[0]?.text?.includes('hi voice room'))
  console.log('game->voice received:', !!gameMsg, gameMsg ? JSON.stringify(gameMsg.args[0]) : '')

  // 3) 回环检查:语音房桥接消息不应再次回到语音房
  const loop = events.filter((e) => e.ev === 'chat' && e.args[0]?.text?.includes('hello from voice room') && e.args[0]?.name?.startsWith('[游戏]'))
  console.log('no loopback:', loop.length === 0)

  ws.close()
  process.exit(0)
})().catch((e) => { console.error('FAIL', e); process.exit(1) })
