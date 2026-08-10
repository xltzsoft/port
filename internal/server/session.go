package server

import (
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"port/internal/auth"
	"port/internal/mux"
	"port/internal/proto"

	"github.com/xtaci/smux"
)

// Session 一个客户端的控制会话:一条 TCP(+TLS) 连接 + smux 多路复用。
// 控制流由客户端开的第一条 stream 承载,其余 stream 为数据流。
type Session struct {
	server     *Server
	clientID   string
	remoteAddr string
	conn       net.Conn
	sess       *smux.Session
	ctrl       *smux.Stream
	loginAt    time.Time
	log        *slog.Logger

	// proxies 由 server.mu 保护
	proxies map[string]*Proxy

	closeOnce sync.Once
	ctrlMu    sync.Mutex // 控制流写锁(控制循环 Pong / 访客通知并发写)

	bytesIn     atomic.Int64 // 客户端 → 服务端
	bytesOut    atomic.Int64 // 服务端 → 客户端
	activeConns atomic.Int64
}

func newSession(s *Server, clientID string, conn net.Conn, sess *smux.Session, ctrl *smux.Stream, remoteAddr string) *Session {
	return &Session{
		server:     s,
		clientID:   clientID,
		remoteAddr: remoteAddr,
		conn:       conn,
		sess:       sess,
		ctrl:       ctrl,
		loginAt:    time.Now(),
		log:        s.log.With("client", clientID),
		proxies:    make(map[string]*Proxy),
	}
}

// sendMsg 在控制流上发送消息(多 goroutine 安全)。
func (se *Session) sendMsg(typ byte, msg any) error {
	se.ctrlMu.Lock()
	defer se.ctrlMu.Unlock()
	_ = se.ctrl.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return proto.WriteMessage(se.ctrl, typ, msg)
}

// Close 关闭会话:释放端口/域名/访客并断开底层连接。幂等。
func (se *Session) Close() {
	se.closeOnce.Do(func() {
		se.server.dropSession(se)
		_ = se.conn.Close()
		se.log.Info("会话关闭")
	})
}

// handleControlConn 处理一条控制连接: TLS 握手 → smux → 登录 → 控制循环。
func (s *Server) handleControlConn(raw net.Conn) {
	conn := raw
	if s.tlsCfg != nil {
		tc := tls.Server(raw, s.tlsCfg)
		_ = tc.SetDeadline(time.Now().Add(15 * time.Second))
		if err := tc.Handshake(); err != nil {
			s.log.Warn("TLS 握手失败", "remote", raw.RemoteAddr().String(), "err", err)
			_ = raw.Close()
			return
		}
		_ = tc.SetDeadline(time.Time{})
		conn = tc
	}
	if tc, ok := raw.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}
	sess, err := smux.Server(conn, mux.Config())
	if err != nil {
		_ = conn.Close()
		return
	}
	// 约定:客户端开的第一条 stream 是控制流
	ctrl, err := sess.AcceptStream()
	if err != nil {
		_ = conn.Close()
		return
	}
	_ = ctrl.SetReadDeadline(time.Now().Add(15 * time.Second))
	typ, payload, err := proto.ReadFrame(ctrl)
	if err != nil || typ != proto.TypeLogin {
		_ = conn.Close()
		return
	}
	var lg proto.Login
	if err := json.Unmarshal(payload, &lg); err != nil {
		_ = conn.Close()
		return
	}
	se := s.login(conn, sess, ctrl, lg, raw.RemoteAddr().String())
	if se == nil {
		return
	}
	go s.serveDataStreams(se)
	s.controlLoop(se)
}

// login 校验登录、踢掉同 clientID 旧连接并登记会话。
func (s *Server) login(conn net.Conn, sess *smux.Session, ctrl *smux.Stream, lg proto.Login, remoteAddr string) *Session {
	se := newSession(s, lg.ClientID, conn, sess, ctrl, remoteAddr)
	fail := func(msg string) *Session {
		_ = se.sendMsg(proto.TypeLoginResp, proto.LoginResp{OK: false, Error: msg})
		_ = conn.Close()
		return nil
	}
	if lg.ClientID == "" {
		return fail("empty client_id")
	}
	if !auth.VerifyLogin(s.cfg.Auth.Token, lg.ClientID, lg.Ts, lg.Hmac, 5*time.Minute) {
		s.log.Warn("登录鉴权失败", "client", lg.ClientID, "remote", remoteAddr)
		return fail("unauthorized")
	}
	if lg.Version != proto.Version {
		s.log.Warn("协议版本不匹配", "client", lg.ClientID, "got", lg.Version, "want", proto.Version)
	}
	s.mu.Lock()
	old := s.sessions[lg.ClientID]
	if old != nil {
		delete(s.sessions, lg.ClientID)
	}
	s.sessions[lg.ClientID] = se
	s.mu.Unlock()
	if old != nil {
		// 踢掉旧连接(同步清理端口,避免新连接注册撞端口);NAT 环境僵尸连接由此回收
		old.Close()
		s.log.Info("替换旧连接", "client", lg.ClientID)
	}
	if err := se.sendMsg(proto.TypeLoginResp, proto.LoginResp{OK: true, ServerTime: time.Now().Unix()}); err != nil {
		se.Close()
		return nil
	}
	s.log.Info("客户端登录", "client", lg.ClientID, "remote", remoteAddr)
	return se
}

// controlLoop 读取控制流消息,直到连接断开或心跳超时(超时按 cfg.Heartbeat.Timeout)。
func (s *Server) controlLoop(se *Session) {
	defer se.Close()
	timeout := time.Duration(s.cfg.Heartbeat.Timeout)
	ctrl := se.ctrl
	for {
		_ = ctrl.SetReadDeadline(time.Now().Add(timeout))
		typ, payload, err := proto.ReadFrame(ctrl)
		if err != nil {
			se.log.Info("控制流结束", "err", err)
			return
		}
		switch typ {
		case proto.TypeNewProxy:
			var np proto.NewProxy
			if err := json.Unmarshal(payload, &np); err != nil {
				se.log.Warn("NewProxy 解析失败", "err", err)
				continue
			}
			resp := s.registerProxy(se, np)
			if err := se.sendMsg(proto.TypeNewProxyResp, resp); err != nil {
				return
			}
		case proto.TypeCloseProxy:
			var cp proto.CloseProxy
			if err := json.Unmarshal(payload, &cp); err != nil {
				continue
			}
			s.closeProxy(se, cp.Name)
		case proto.TypePing:
			var pg proto.Ping
			if err := json.Unmarshal(payload, &pg); err != nil {
				continue
			}
			if err := se.sendMsg(proto.TypePong, proto.Pong{Ts: pg.Ts}); err != nil {
				return
			}
		default:
			se.log.Warn("未知控制消息", "type", typ)
		}
	}
}

// serveDataStreams 接受客户端打开的数据流(每条对应一个访客连接)。
func (s *Server) serveDataStreams(se *Session) {
	for {
		stream, err := se.sess.AcceptStream()
		if err != nil {
			return // 会话关闭
		}
		go s.handleDataStream(se, stream)
	}
}

// handleDataStream 读取数据流首帧(StreamMeta)找到代理,与排队访客 join。
func (s *Server) handleDataStream(se *Session, stream *smux.Stream) {
	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, payload, err := proto.ReadFrame(stream)
	if err != nil {
		_ = stream.Close()
		return
	}
	if typ != proto.TypeStreamMeta {
		_ = stream.Close()
		return
	}
	var meta proto.StreamMeta
	if err := json.Unmarshal(payload, &meta); err != nil || meta.ProxyName == "" {
		_ = stream.Close()
		return
	}
	s.mu.Lock()
	p := se.proxies[meta.ProxyName]
	s.mu.Unlock()
	if p == nil {
		se.log.Warn("数据流指向未知代理", "name", meta.ProxyName)
		_ = stream.Close()
		return
	}
	// 等访客:客户端先拨内网服务成功才开流,失败则不出现
	var visitor net.Conn
	select {
	case visitor = <-p.visitors:
	case <-time.After(15 * time.Second):
		_ = stream.Close()
		return
	}
	_ = stream.SetDeadline(time.Time{})
	se.log.Debug("数据流 join", "proxy", meta.ProxyName, "visitor", visitor.RemoteAddr().String())

	se.activeConns.Add(1)
	p.active.Add(1)
	defer func() {
		se.activeConns.Add(-1)
		p.active.Add(-1)
	}()
	// visitor→stream 为发往客户端,计入 bytesOut;反向计入 bytesIn
	mux.Join(visitor, stream,
		func(n int64) { se.bytesOut.Add(n) },
		func(n int64) { se.bytesIn.Add(n) },
	)
}
