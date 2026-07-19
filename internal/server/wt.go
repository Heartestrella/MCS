package server

// wt.go — WebTransport (HTTP/3 / QUIC) 中转,Go 重写原 server/webtransport.js。
// 屏幕视频/屏幕音频块在双方都有健康 WT 会话时走 QUIC uni-stream(UDP 低延迟),
// 语音/控制/聊天仍走 WebSocket。
//
// uni-stream 线格式(与前端 useTransport.js 一致,一流一块 FIN 结尾):
//   [1B kind]      0x01=video 0x02=screen-audio
//   [8B ts BE]     微秒时间戳
//   [2B metaLen]   JSON 元数据长度
//   [meta JSON][payload]
// datagram 仅心跳: [1B kind 0xF0=ping/0xF1=pong][4B seq]

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

const (
	wtKindVideo       = 0x01
	wtKindScreenAudio = 0x02
	wtKindPing        = 0xF0
	wtKindPong        = 0xF1
	wtTokenTTL        = 30 * time.Second
	wtMaxChunk        = 20 * 1024 * 1024
)

type wtToken struct {
	peerID  string
	room    string
	expires time.Time
}

type WTRelay struct {
	talk     *TalkServer
	server   *webtransport.Server
	mu       sync.Mutex
	tokens   map[string]*wtToken
	sessions map[string]*webtransport.Session // peerID -> session
	port     int

	certMu   sync.Mutex
	cert     *tls.Certificate
	certHash string // base64(SHA-256(DER)),浏览器 serverCertificateHashes 用
	certExp  time.Time
}

// wtCert returns a short-lived ECDSA cert. Chrome 的 WebTransport 只信任
// serverCertificateHashes 机制下有效期 ≤14 天的 ECDSA 证书(自签也行),
// 所以这里内存自签 13 天,过半自动轮换,哈希随 token 下发。
func (r *WTRelay) wtCert() (*tls.Certificate, string) {
	r.certMu.Lock()
	defer r.certMu.Unlock()
	if r.cert != nil && time.Now().Before(r.certExp.Add(-7*24*time.Hour)) {
		return r.cert, r.certHash
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return r.cert, r.certHash
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "MCS Talk WT"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(13 * 24 * time.Hour), // ≤14 天是硬性要求
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	if lan := lanIP(); lan != "" {
		if ip := net.ParseIP(lan); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return r.cert, r.certHash
	}
	sum := sha256.Sum256(der)
	r.cert = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	r.certHash = base64.StdEncoding.EncodeToString(sum[:])
	r.certExp = tmpl.NotAfter
	log.Printf("[wt] 短期证书已签发(13天), hash=%s", r.certHash[:12])
	return r.cert, r.certHash
}

// NewWTRelay starts the QUIC listener on udp/port. 用 ≤14 天短期自签证书 +
// serverCertificateHashes(哈希随 token 下发),浏览器无需系统信任即可连。
// Returns nil when startup fails (talk falls back to pure WS).
func NewWTRelay(talk *TalkServer, dataDir string, port int) *WTRelay {
	r := &WTRelay{
		talk:     talk,
		tokens:   map[string]*wtToken{},
		sessions: map[string]*webtransport.Session{},
		port:     port,
	}
	r.wtCert() // 预热,启动即签发
	mux := http.NewServeMux()
	r.server = &webtransport.Server{
		// 页面在 8446/HTTPS,WT 在 4433 — 跨端口 Origin,默认校验会拒
		CheckOrigin: func(*http.Request) bool { return true },
		H3: &http3.Server{
			Addr:            fmt.Sprintf("0.0.0.0:%d", port),
			EnableDatagrams: true, // WebTransport 握手要求 H3_DATAGRAM
			TLSConfig: &tls.Config{
				MinVersion: tls.VersionTLS13,
				NextProtos: []string{http3.NextProtoH3},
				GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
					cert, _ := r.wtCert()
					return cert, nil
				},
			},
			Handler: mux,
		},
	}
	mux.HandleFunc("/wt", func(w http.ResponseWriter, req *http.Request) {
		sess, err := r.server.Upgrade(w, req)
		if err != nil {
			log.Printf("[wt] upgrade 被拒: %v (origin=%s)", err, req.Header.Get("Origin"))
			return
		}
		go r.handleSession(sess)
	})
	// 必须显式调用:在 H3 SETTINGS 里宣告 WebTransport 支持(含 Safari 要求的
	// WT_MAX_SESSIONS),否则客户端握手直接失败 "server didn't enable WebTransport"
	webtransport.ConfigureHTTP3Server(r.server.H3)
	go func() {
		log.Printf("[wt] WebTransport listening on udp/%d", port)
		if err := r.server.ListenAndServe(); err != nil {
			log.Printf("[wt] 监听失败(前端自动回落 WS): %v", err)
		}
	}()
	go r.reapTokens()
	return r
}

func (r *WTRelay) reapTokens() {
	for range time.Tick(15 * time.Second) {
		now := time.Now()
		r.mu.Lock()
		for t, v := range r.tokens {
			if v.expires.Before(now) {
				delete(r.tokens, t)
			}
		}
		r.mu.Unlock()
	}
}

// IssueToken registers a short-lived token binding peerID+room and returns
// the token, dial URL, and the current cert hash for serverCertificateHashes.
func (r *WTRelay) IssueToken(peerID, room string) (token, url, certHash string) {
	b := make([]byte, 18)
	rand.Read(b)
	token = base64.RawURLEncoding.EncodeToString(b)
	r.mu.Lock()
	r.tokens[token] = &wtToken{peerID: peerID, room: room, expires: time.Now().Add(wtTokenTTL)}
	r.mu.Unlock()
	host := lanIP()
	if host == "" {
		host = "127.0.0.1"
	}
	_, hash := r.wtCert()
	return token, fmt.Sprintf("https://%s:%d/wt", host, r.port), hash
}

// DropPeer closes and forgets the session of a disconnecting WS peer.
func (r *WTRelay) DropPeer(peerID string) {
	r.mu.Lock()
	sess := r.sessions[peerID]
	delete(r.sessions, peerID)
	r.mu.Unlock()
	if sess != nil {
		sess.CloseWithError(0, "peer left")
	}
}

// ---------- session ----------

func (r *WTRelay) handleSession(sess *webtransport.Session) {
	// hello:第一条 bidi 流,JSON {token, socketId} → ack {ok:true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	stream, err := sess.AcceptStream(ctx)
	cancel()
	if err != nil {
		sess.CloseWithError(0, "hello timeout")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(stream, 4096))
	if err != nil {
		sess.CloseWithError(0, "hello read")
		return
	}
	var hello struct {
		Token    string `json:"token"`
		SocketID string `json:"socketId"`
	}
	if json.Unmarshal(raw, &hello) != nil || hello.Token == "" {
		sess.CloseWithError(0, "bad hello")
		return
	}
	r.mu.Lock()
	tk := r.tokens[hello.Token]
	if tk != nil {
		delete(r.tokens, hello.Token)
	}
	r.mu.Unlock()
	if tk == nil || tk.peerID != hello.SocketID {
		sess.CloseWithError(0, "bad token")
		return
	}
	stream.Write([]byte(`{"ok":true}`))
	stream.Close()

	peerID, room := tk.peerID, tk.room
	r.mu.Lock()
	if prev := r.sessions[peerID]; prev != nil {
		prev.CloseWithError(0, "replaced")
	}
	r.sessions[peerID] = sess
	r.mu.Unlock()
	log.Printf("[wt] hello ok peer=%s room=%s", peerID, room)

	go r.echoDatagrams(sess)
	r.readUniStreams(sess, peerID, room) // 阻塞直到会话结束

	r.mu.Lock()
	if r.sessions[peerID] == sess {
		delete(r.sessions, peerID)
	}
	r.mu.Unlock()
	log.Printf("[wt] session closed peer=%s", peerID)
}

func (r *WTRelay) echoDatagrams(sess *webtransport.Session) {
	for {
		b, err := sess.ReceiveDatagram(context.Background())
		if err != nil {
			return
		}
		if len(b) >= 1 && b[0] == wtKindPing {
			pong := make([]byte, len(b))
			copy(pong, b)
			pong[0] = wtKindPong
			sess.SendDatagram(pong)
		}
	}
}

func (r *WTRelay) readUniStreams(sess *webtransport.Session, fromPeer, room string) {
	for {
		uni, err := sess.AcceptUniStream(context.Background())
		if err != nil {
			return
		}
		go func() {
			buf, err := io.ReadAll(io.LimitReader(uni, wtMaxChunk+1))
			if err != nil || len(buf) < 11 || len(buf) > wtMaxChunk {
				return
			}
			r.fanOut(fromPeer, room, buf)
		}()
	}
}

// fanOut rewrites the chunk header to include meta.from, then delivers to every
// room member: WT session if live, otherwise mirrors over WebSocket.
func (r *WTRelay) fanOut(fromPeer, room string, chunk []byte) {
	kind := chunk[0]
	if kind != wtKindVideo && kind != wtKindScreenAudio {
		return
	}
	ts := int64(binary.BigEndian.Uint64(chunk[1:9]))
	metaLen := int(binary.BigEndian.Uint16(chunk[9:11]))
	if 11+metaLen > len(chunk) {
		return
	}
	meta := map[string]any{}
	if metaLen > 0 {
		if json.Unmarshal(chunk[11:11+metaLen], &meta) != nil {
			return
		}
	}
	payload := chunk[11+metaLen:]

	// 注入 from 重编码(WT 会话是点对点的,接收端无法从连接推断发送者)
	meta["from"] = fromPeer
	newMeta, _ := json.Marshal(meta)
	rewritten := make([]byte, 0, 11+len(newMeta)+len(payload))
	rewritten = append(rewritten, kind)
	rewritten = binary.BigEndian.AppendUint64(rewritten, uint64(ts))
	rewritten = binary.BigEndian.AppendUint16(rewritten, uint16(len(newMeta)))
	rewritten = append(rewritten, newMeta...)
	rewritten = append(rewritten, payload...)

	// 收集房间成员(避免长时间持有 talk 锁:先拷贝引用)
	r.talk.mu.Lock()
	var wsTargets []*talkPeer
	var wtTargets []*webtransport.Session
	if tr := r.talk.rooms[room]; tr != nil {
		for id, p := range tr.peers {
			if id == fromPeer {
				continue
			}
			r.mu.Lock()
			sess := r.sessions[id]
			r.mu.Unlock()
			if sess != nil {
				wtTargets = append(wtTargets, sess)
			} else {
				wsTargets = append(wsTargets, p)
			}
		}
	}
	r.talk.mu.Unlock()

	for _, sess := range wtTargets {
		go func(s *webtransport.Session) {
			uni, err := s.OpenUniStream()
			if err != nil {
				return
			}
			uni.Write(rewritten)
			uni.Close()
		}(sess)
	}

	if len(wsTargets) > 0 {
		frame := buildWSMediaFrame(fromPeer, kind, ts, meta, payload)
		for _, p := range wsTargets {
			p.push(frame)
		}
	}
}

// buildWSMediaFrame converts a WT chunk into the equivalent WS 'video' /
// 'screen-audio' frame (msg 对象里 data/description 用 $bin 占位)。
func buildWSMediaFrame(fromPeer string, kind byte, ts int64, meta map[string]any, payload []byte) []byte {
	ev := "video"
	if kind == wtKindScreenAudio {
		ev = "screen-audio"
	}
	msg := map[string]any{"ts": ts, "data": map[string]any{"$bin": 0}}
	bins := [][]byte{payload}
	if t, _ := meta["type"].(string); t != "" {
		msg["type"] = t
	} else if kind == wtKindVideo {
		msg["type"] = "delta"
	} else {
		msg["type"] = "key"
	}
	if kind == wtKindVideo {
		if cfg, ok := meta["config"].(map[string]any); ok {
			outCfg := map[string]any{}
			for k, v := range cfg {
				if k == "description" {
					if s, ok := v.(string); ok && s != "" {
						if desc, err := base64.StdEncoding.DecodeString(s); err == nil {
							bins = append(bins, desc)
							outCfg["description"] = map[string]any{"$bin": len(bins) - 1}
						}
					}
					continue
				}
				outCfg[k] = v
			}
			msg["config"] = outCfg
		}
	} else {
		if v, ok := meta["sampleRate"]; ok {
			msg["sampleRate"] = v
		} else {
			msg["sampleRate"] = 48000
		}
		if v, ok := meta["channels"]; ok {
			msg["channels"] = v
		} else {
			msg["channels"] = 2
		}
		if s, ok := meta["description"].(string); ok && s != "" {
			if desc, err := base64.StdEncoding.DecodeString(s); err == nil {
				bins = append(bins, desc)
				msg["description"] = map[string]any{"$bin": len(bins) - 1}
			}
		}
	}
	return buildTalkFrame(ev, []json.RawMessage{jraw(fromPeer), jraw(msg)}, bins)
}
