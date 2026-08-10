// Package server 实现 port 服务端:控制连接管理、端口池、vhost 路由、访客监听与管理面板。
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"port/internal/config"
	"port/internal/proto"
)

// Server 服务端核心。
type Server struct {
	cfg    *config.Server
	ranges []config.PortRange
	log    *slog.Logger
	tlsCfg *tls.Config // 未配置证书时为 nil,退化为明文(仅建议内网调试)

	mu        sync.Mutex
	sessions  map[string]*Session // clientID -> session
	proxies   map[string]*Proxy   // "clientID/name" -> proxy
	byPort    map[int]*Proxy      // 出站端口 -> proxy
	vhost     map[string]*Proxy   // 小写域名 -> proxy
	startedAt time.Time

	vhostLn net.Listener
	dashLn  net.Listener
}

// New 创建服务端:解析端口白名单,加载 TLS 证书。
func New(cfg *config.Server, log *slog.Logger) (*Server, error) {
	ranges, err := config.ParsePortRanges(cfg.AllowPorts)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:       cfg,
		ranges:    ranges,
		log:       log,
		sessions:  make(map[string]*Session),
		proxies:   make(map[string]*Proxy),
		byPort:    make(map[int]*Proxy),
		vhost:     make(map[string]*Proxy),
		startedAt: time.Now(),
	}
	if cfg.TLS.Cert != "" && cfg.TLS.Key != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
		if err != nil {
			return nil, fmt.Errorf("加载 TLS 证书: %w", err)
		}
		s.tlsCfg = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}
	return s, nil
}

// controlAddr 控制入站地址。
func (s *Server) controlAddr() string {
	return net.JoinHostPort(s.cfg.BindAddr, strconv.Itoa(s.cfg.BindPort))
}

// Run 启动全部监听(控制 17200 / vhost / dashboard 17250)并阻塞,
// 直到 ctx 取消后优雅关闭。
func (s *Server) Run(ctx context.Context) error {
	ctrlLn, err := net.Listen("tcp", s.controlAddr())
	if err != nil {
		return fmt.Errorf("监听控制端口 %s: %w", s.controlAddr(), err)
	}
	s.log.Info("port 服务端启动",
		"control", ctrlLn.Addr().String(),
		"allow_ports", s.cfg.AllowPorts,
		"tls", s.tlsCfg != nil,
	)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.acceptLoop(ctrlLn)
	}()

	if s.cfg.VhostHTTPPort > 0 {
		addr := net.JoinHostPort(s.cfg.BindAddr, strconv.Itoa(s.cfg.VhostHTTPPort))
		s.vhostLn, err = net.Listen("tcp", addr)
		if err != nil {
			_ = ctrlLn.Close()
			return fmt.Errorf("监听 vhost 端口 %s: %w", addr, err)
		}
		s.log.Info("vhost http 监听", "addr", s.vhostLn.Addr().String(), "domain", s.cfg.VhostDomain)
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.vhostAcceptLoop(s.vhostLn)
		}()
	}

	if s.cfg.Dashboard.Addr != "" {
		s.dashLn, err = net.Listen("tcp", s.cfg.Dashboard.Addr)
		if err != nil {
			_ = ctrlLn.Close()
			if s.vhostLn != nil {
				_ = s.vhostLn.Close()
			}
			return fmt.Errorf("监听 dashboard %s: %w", s.cfg.Dashboard.Addr, err)
		}
		s.log.Info("dashboard 监听", "addr", s.dashLn.Addr().String(), "auth", s.cfg.Dashboard.User != "")
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveDashboard(s.dashLn)
		}()
	}

	<-ctx.Done()
	s.log.Info("收到退出信号,开始关闭")
	_ = ctrlLn.Close()
	if s.vhostLn != nil {
		_ = s.vhostLn.Close()
	}
	if s.dashLn != nil {
		_ = s.dashLn.Close()
	}
	s.mu.Lock()
	sessions := make([]*Session, 0, len(s.sessions))
	for _, se := range s.sessions {
		sessions = append(sessions, se)
	}
	s.mu.Unlock()
	for _, se := range sessions {
		se.Close()
	}
	wg.Wait()
	return nil
}

// acceptLoop 控制端口连接接入循环。
func (s *Server) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("accept 失败", "err", err)
			continue
		}
		go s.handleControlConn(conn)
	}
}

// vhostAcceptLoop vhost HTTP 端口接入循环。
func (s *Server) vhostAcceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("vhost accept 失败", "err", err)
			continue
		}
		go s.handleVhostConn(conn)
	}
}

// dropSession 移除会话:从 sessions 表摘除并清理其全部代理(释放端口/域名/访客)。
// 调用方需保证 session 已不可再注册新代理(控制循环已结束或被踢)。
func (s *Server) dropSession(se *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[se.clientID] == se {
		delete(s.sessions, se.clientID)
	}
	for _, p := range se.proxies {
		s.removeProxyLocked(p)
	}
}

// removeProxyLocked 移除一条代理并释放资源。调用方需持有 s.mu。
func (s *Server) removeProxyLocked(p *Proxy) {
	delete(s.proxies, proxyKey(p.clientID, p.name))
	delete(p.session.proxies, p.name)
	if p.remotePort != 0 {
		delete(s.byPort, p.remotePort)
	}
	for _, h := range p.hosts {
		delete(s.vhost, h)
	}
	if p.ln != nil {
		_ = p.ln.Close()
	}
	// 关闭排队中、尚未 join 的访客连接
	for {
		select {
		case v := <-p.visitors:
			_ = v.Close()
		default:
			return
		}
	}
}

// lookupVhost 按小写域名查找 vhost 代理。
func (s *Server) lookupVhost(host string) *Proxy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vhost[host]
}

// deliverVisitor 把访客连接送入代理队列并通知客户端开数据流。
// 队列满时直接关闭访客(软限流)。
func (s *Server) deliverVisitor(p *Proxy, conn net.Conn) bool {
	select {
	case p.visitors <- conn:
	default:
		_ = conn.Close()
		return false
	}
	if err := p.session.sendMsg(proto.TypeStartWorkConn, proto.StartWorkConn{ProxyName: p.name}); err != nil {
		// 通知失败(会话已死):取回一个排队访客并关闭
		select {
		case v := <-p.visitors:
			_ = v.Close()
		default:
		}
		_ = conn.Close()
		return false
	}
	return true
}

// proxyAcceptLoop 出站监听端口的访客接入循环。
func (s *Server) proxyAcceptLoop(p *Proxy) {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return // 监听器被关闭(代理下线/会话断开)
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(30 * time.Second)
		}
		s.deliverVisitor(p, conn)
	}
}
