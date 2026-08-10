// Package client 实现 port 客户端:登录、代理注册、心跳、断线重连、数据流转发与配置热重载。
//
// 连接模型:一条 TCP(+TLS) 连接上跑 smux,第一条 stream 为控制流。
// 控制流由一个读取 goroutine 独占,所有写入经 writer goroutine 串行;
// 注册类请求带 ReqID,应答由读取 goroutine 路由回等待方,实现并发安全的 RPC。
package client

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"port/internal/auth"
	"port/internal/config"
	"port/internal/mux"
	"port/internal/proto"

	"github.com/xtaci/smux"
)

// writeReq 发往 writer goroutine 的写请求。
type writeReq struct {
	typ   byte
	msg   any
	reqID int
}

// Client 客户端核心。
type Client struct {
	log *slog.Logger

	mu      sync.Mutex
	cfg     *config.Client // Reload 时整体替换
	proxies map[string]*config.Proxy
	session *smux.Session
	writeCh chan writeReq

	pendingMu sync.Mutex
	pending   map[int]chan proto.NewProxyResp // ReqID -> 应答通道
	reqSeq    int
}

// New 创建客户端。
func New(cfg *config.Client, log *slog.Logger) (*Client, error) {
	return &Client{
		log:     log,
		cfg:     cfg,
		proxies: buildProxyMap(cfg.Proxies),
		pending: make(map[int]chan proto.NewProxyResp),
	}, nil
}

func buildProxyMap(ps []config.Proxy) map[string]*config.Proxy {
	m := make(map[string]*config.Proxy, len(ps))
	for i := range ps {
		m[ps[i].Name] = &ps[i]
	}
	return m
}

// Run 主循环:连接 → 登录 → 注册代理 → 维持连接;断线按指数退避重连。
// 连续稳定运行超过 5 分钟后退避重置,重连后按原配置重新注册,对外地址不变。
func (c *Client) Run(ctx context.Context) error {
	base := time.Duration(c.cfg.Reconnect.Base)
	maxD := time.Duration(c.cfg.Reconnect.Max)
	backoff := base
	stableAt := time.Now()
	for {
		if ctx.Err() != nil {
			return nil
		}
		c.mu.Lock()
		cfg := c.cfg
		c.mu.Unlock()
		err := c.runOnce(ctx, cfg)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(stableAt) > 5*time.Minute {
			backoff = base // 稳定运行 5 分钟后重置退避
		}
		jitter := time.Duration(rand.Float64()*0.4-0.2) * backoff
		wait := backoff + jitter
		if wait < 0 {
			wait = 0
		}
		c.log.Warn("连接断开,准备重连", "err", err, "wait", wait.Round(time.Millisecond))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil
		}
		backoff *= 2
		if backoff > maxD {
			backoff = maxD
		}
	}
}

// runOnce 单次连接的生命周期。
func (c *Client) runOnce(ctx context.Context, cfg *config.Client) error {
	c.clearSession()

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	raw, err := dialer.Dial("tcp", cfg.ServerAddr)
	if err != nil {
		return fmt.Errorf("连接服务端失败: %w", err)
	}
	conn := raw
	if cfg.TLS.Enabled {
		conn, err = c.wrapTLS(raw, cfg)
		if err != nil {
			_ = raw.Close()
			return fmt.Errorf("TLS 握手失败: %w", err)
		}
	}
	sess, err := smux.Client(conn, mux.Config())
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("建立多路复用失败: %w", err)
	}
	// 第一条 stream 作为控制流
	ctrl, err := sess.OpenStream()
	if err != nil {
		_ = conn.Close()
		return err
	}

	// 登录
	_ = ctrl.SetDeadline(time.Now().Add(10 * time.Second))
	ts := time.Now().Unix()
	if err := proto.WriteMessage(ctrl, proto.TypeLogin, proto.Login{
		Version:  proto.Version,
		ClientID: cfg.ClientID,
		Ts:       ts,
		Hmac:     auth.HmacLogin(cfg.Token, cfg.ClientID, ts),
	}); err != nil {
		_ = conn.Close()
		return fmt.Errorf("发送登录请求失败: %w", err)
	}
	typ, payload, err := proto.ReadFrame(ctrl)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("读取登录应答失败: %w", err)
	}
	if typ != proto.TypeLoginResp {
		_ = conn.Close()
		return fmt.Errorf("意外的登录应答类型 %d", typ)
	}
	var lr proto.LoginResp
	if err := json.Unmarshal(payload, &lr); err != nil {
		_ = conn.Close()
		return err
	}
	if !lr.OK {
		_ = conn.Close()
		return fmt.Errorf("登录被拒绝: %s", lr.Error)
	}
	_ = ctrl.SetDeadline(time.Time{})

	writeCh := make(chan writeReq, 64)
	c.setSession(sess, writeCh)
	defer c.clearSession()

	stopPing := make(chan struct{})
	defer close(stopPing)
	go c.writerLoop(ctrl, writeCh)
	go c.pingLoop(cfg, writeCh, stopPing)
	ctrlErr := make(chan error, 1)
	go func() { ctrlErr <- c.controlLoop(cfg, ctrl) }()

	// 注册全部代理(应答经控制循环路由回来)
	for _, p := range c.proxyList() {
		if err := c.registerProxy(p); err != nil {
			c.log.Error("代理注册失败", "name", p.Name, "err", err)
		}
	}
	c.log.Info("客户端就绪", "client_id", cfg.ClientID, "proxies", len(c.proxyList()))

	err = <-ctrlErr
	close(writeCh)
	if err != nil {
		return fmt.Errorf("控制流结束: %w", err)
	}
	return errors.New("控制流结束")
}

// controlLoop 独占读取控制流:心跳应答、访客通知、注册应答路由。
// 读超时(Heartbeat.Timeout)即判定连接死亡,返回后触发重连。
func (c *Client) controlLoop(cfg *config.Client, ctrl *smux.Stream) error {
	timeout := time.Duration(cfg.Heartbeat.Timeout)
	for {
		_ = ctrl.SetReadDeadline(time.Now().Add(timeout))
		typ, payload, err := proto.ReadFrame(ctrl)
		if err != nil {
			return err
		}
		switch typ {
		case proto.TypePong:
			// 心跳应答:任何消息都会刷新读超时,无需额外处理
		case proto.TypeStartWorkConn:
			var m proto.StartWorkConn
			if err := json.Unmarshal(payload, &m); err != nil || m.ProxyName == "" {
				continue
			}
			go c.handleWorkConn(m.ProxyName)
		case proto.TypeNewProxyResp:
			var resp proto.NewProxyResp
			if err := json.Unmarshal(payload, &resp); err != nil {
				continue
			}
			c.deliverPending(resp)
		case proto.TypeError:
			var em proto.ErrorMsg
			_ = json.Unmarshal(payload, &em)
			c.log.Warn("服务端错误", "error", em.Error)
		default:
			c.log.Warn("未知控制消息", "type", typ)
		}
	}
}

// writerLoop 串行写入控制流。写失败说明连接已死,退出即可(读侧会触发重连)。
func (c *Client) writerLoop(ctrl *smux.Stream, writeCh <-chan writeReq) {
	for req := range writeCh {
		_ = ctrl.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := proto.WriteMessage(ctrl, req.typ, req.msg); err != nil {
			c.log.Warn("控制流写入失败", "err", err)
			return
		}
	}
}

// pingLoop 应用层心跳。
func (c *Client) pingLoop(cfg *config.Client, writeCh chan<- writeReq, stop <-chan struct{}) {
	iv := time.Duration(cfg.Heartbeat.Interval)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			select {
			case writeCh <- writeReq{typ: proto.TypePing, msg: proto.Ping{Ts: time.Now().Unix()}}:
			default:
			}
		case <-stop:
			return
		}
	}
}

// registerProxy 注册代理并等待服务端应答(经控制循环路由)。
func (c *Client) registerProxy(p *config.Proxy) error {
	c.mu.Lock()
	writeCh := c.writeCh
	c.mu.Unlock()
	if writeCh == nil {
		return errors.New("未连接")
	}
	reqID := c.nextReqID()
	ch := make(chan proto.NewProxyResp, 1)
	c.pendingMu.Lock()
	c.pending[reqID] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, reqID)
		c.pendingMu.Unlock()
	}()

	msg := proto.NewProxy{
		Name:          p.Name,
		Type:          p.Type,
		Local:         p.Local,
		RemotePort:    p.RemotePort,
		Subdomain:     p.Subdomain,
		CustomDomains: p.CustomDomains,
		ReqID:         reqID,
	}
	select {
	case writeCh <- writeReq{typ: proto.TypeNewProxy, msg: msg, reqID: reqID}:
	case <-time.After(5 * time.Second):
		return errors.New("发送注册请求超时(连接已断开?)")
	}
	select {
	case resp := <-ch:
		if !resp.OK {
			return fmt.Errorf("服务端拒绝: %s", resp.Error)
		}
		c.log.Info("代理注册成功", "name", p.Name, "type", p.Type, "remote_port", resp.RemotePort, "local", p.Local)
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("等待注册应答超时")
	}
}

// closeProxyRPC 通知服务端下线代理(不等待应答)。
func (c *Client) closeProxyRPC(name string) {
	c.mu.Lock()
	writeCh := c.writeCh
	c.mu.Unlock()
	if writeCh == nil {
		return
	}
	select {
	case writeCh <- writeReq{typ: proto.TypeCloseProxy, msg: proto.CloseProxy{Name: name}}:
		c.log.Info("代理已下线", "name", name)
	default:
		c.log.Warn("下线通知发送失败(连接繁忙)", "name", name)
	}
}

func (c *Client) deliverPending(resp proto.NewProxyResp) {
	c.pendingMu.Lock()
	ch := c.pending[resp.ReqID]
	c.pendingMu.Unlock()
	if ch != nil {
		ch <- resp
	}
}

func (c *Client) nextReqID() int {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	c.reqSeq++
	return c.reqSeq
}

// handleWorkConn 收到访客通知后:拨内网服务 → 开数据流 → 写元数据 → 双向透传。
// 内网服务拨不通则不开流,服务端侧访客会在超时后收到连接重置。
func (c *Client) handleWorkConn(name string) {
	sess := c.getSession()
	if sess == nil {
		return
	}
	c.mu.Lock()
	p, ok := c.proxies[name]
	c.mu.Unlock()
	if !ok {
		c.log.Warn("收到未知代理的访客通知", "name", name)
		return
	}
	local, err := net.DialTimeout("tcp", p.Local, 5*time.Second)
	if err != nil {
		c.log.Warn("连接本地服务失败", "proxy", name, "local", p.Local, "err", err)
		return
	}
	stream, err := sess.OpenStream()
	if err != nil {
		_ = local.Close()
		return
	}
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
	if err := proto.WriteMessage(stream, proto.TypeStreamMeta, proto.StreamMeta{ProxyName: name}); err != nil {
		_ = stream.Close()
		_ = local.Close()
		return
	}
	_ = stream.SetDeadline(time.Time{})
	c.log.Debug("数据流已建立", "proxy", name, "local", p.Local)
	mux.Join(stream, local, nil, nil)
}

// wrapTLS 包装 TLS:支持跳过校验与证书指纹 pinning。
func (c *Client) wrapTLS(conn net.Conn, cfg *config.Client) (net.Conn, error) {
	host, _, err := net.SplitHostPort(cfg.ServerAddr)
	if err != nil {
		return nil, err
	}
	tcfg := &tls.Config{
		ServerName:         host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.TLS.SkipVerify || cfg.TLS.ServerFingerprint != "",
	}
	tc := tls.Client(conn, tcfg)
	_ = tc.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tc.Handshake(); err != nil {
		return nil, err
	}
	_ = tc.SetDeadline(time.Time{})
	if fp := cfg.TLS.ServerFingerprint; fp != "" {
		if err := verifyFingerprint(tc, fp); err != nil {
			_ = tc.Close()
			return nil, err
		}
	}
	return tc, nil
}

// verifyFingerprint 校验服务端证书 SHA256 指纹。
// 兼容 "SHA256:xx" 前缀、冒号分隔(openssl -fingerprint 输出)与裸 hex 三种写法。
func verifyFingerprint(tc *tls.Conn, want string) error {
	want = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(want)), "sha256:")
	want = strings.ReplaceAll(want, ":", "")
	if len(want) != 64 {
		return fmt.Errorf("指纹格式无效: %q", want)
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return errors.New("服务端未提供证书")
	}
	sum := sha256.Sum256(certs[0].Raw)
	got := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return fmt.Errorf("证书指纹不匹配,实际为 SHA256:%s", got)
	}
	return nil
}

// Reload 热重载配置(SIGHUP):新增/变更的代理即时注册,删除的立即下线。
// 断线状态时只更新内存配置,重连后按新配置注册。
func (c *Client) Reload(path string) {
	newCfg, err := config.LoadClient(path)
	if err != nil {
		c.log.Error("重载配置失败", "err", err)
		return
	}
	c.mu.Lock()
	oldM := c.proxies
	newM := buildProxyMap(newCfg.Proxies)
	c.cfg = newCfg
	c.proxies = newM
	c.mu.Unlock()
	c.log.Info("配置已重载", "proxies", len(newCfg.Proxies))

	if c.getSession() == nil {
		return // 断线:重连时会按新配置注册
	}
	for name, np := range newM {
		op, exists := oldM[name]
		if exists && proxyEqual(op, np) {
			continue
		}
		if exists {
			c.closeProxyRPC(name) // 配置变更:先下线再重新注册
		}
		if err := c.registerProxy(np); err != nil {
			c.log.Error("代理注册失败", "name", name, "err", err)
		}
	}
	for name := range oldM {
		if _, ok := newM[name]; !ok {
			c.closeProxyRPC(name)
		}
	}
}

func proxyEqual(a, b *config.Proxy) bool {
	return a.Name == b.Name && a.Type == b.Type && a.Local == b.Local &&
		a.RemotePort == b.RemotePort && a.Subdomain == b.Subdomain &&
		config.SameStrings(a.CustomDomains, b.CustomDomains)
}

// ---- 会话状态访问(Reload 与 runOnce 并发) ----

func (c *Client) setSession(sess *smux.Session, writeCh chan writeReq) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = sess
	c.writeCh = writeCh
}

func (c *Client) clearSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = nil
	c.writeCh = nil
}

func (c *Client) getSession() *smux.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

func (c *Client) proxyList() []*config.Proxy {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*config.Proxy, 0, len(c.proxies))
	for _, p := range c.proxies {
		out = append(out, p)
	}
	return out
}
