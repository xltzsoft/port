package server

import (
	"fmt"
	"net"
	"strconv"
	"sync/atomic"

	"port/internal/config"
	"port/internal/proto"
)

// Proxy 一条已注册的代理(归属某个客户端会话)。
// tcp 类型占用独立出站端口;http 类型注册 vhost 域名,共享 vhost 端口。
type Proxy struct {
	name          string
	typ           string
	clientID      string
	local         string
	remotePort    int
	subdomain     string
	customDomains []string
	hosts         []string // vhost 域名(小写)
	ln            net.Listener
	visitors      chan net.Conn
	session       *Session
	active        atomic.Int64 // 当前活动连接数
}

func proxyKey(clientID, name string) string {
	return clientID + "/" + name
}

// registerProxy 按类型分发代理注册。
func (s *Server) registerProxy(se *Session, np proto.NewProxy) proto.NewProxyResp {
	switch np.Type {
	case "tcp":
		return s.registerTCPProxy(se, np)
	case "http":
		return s.registerHTTPProxy(se, np)
	default:
		return proto.NewProxyResp{ReqID: np.ReqID, Name: np.Name, Error: fmt.Sprintf("不支持的代理类型 %q", np.Type)}
	}
}

// registerTCPProxy 注册 TCP 代理:校验端口白名单/占用,绑定出站监听。
// 同名代理配置不变时幂等成功(重连后重新注册,对外端口不变)。
func (s *Server) registerTCPProxy(se *Session, np proto.NewProxy) proto.NewProxyResp {
	resp := proto.NewProxyResp{ReqID: np.ReqID, Name: np.Name}
	if _, _, err := net.SplitHostPort(np.Local); err != nil {
		resp.Error = "invalid local address"
		return resp
	}
	if np.RemotePort != 0 && !s.portAllowed(np.RemotePort) {
		resp.Error = fmt.Sprintf("INVALID_PORT: %d 不在白名单 %s 内", np.RemotePort, s.cfg.AllowPorts)
		return resp
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := proxyKey(se.clientID, np.Name)
	if old, ok := s.proxies[key]; ok {
		if old.sameConfig(np) {
			return proto.NewProxyResp{ReqID: np.ReqID, Name: np.Name, OK: true, RemotePort: old.remotePort}
		}
		resp.Error = "NAME_EXISTS: 同名代理配置不一致,请先 CloseProxy"
		return resp
	}

	port := np.RemotePort
	if port == 0 {
		port = s.allocPortLocked()
		if port == 0 {
			resp.Error = "PORT_POOL_FULL: 端口池已满"
			return resp
		}
	}
	if _, used := s.byPort[port]; used {
		resp.Error = fmt.Sprintf("PORT_IN_USE: %d 已被占用", port)
		return resp
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.BindAddr, strconv.Itoa(port)))
	if err != nil {
		resp.Error = fmt.Sprintf("BIND_FAILED: %v", err)
		return resp
	}

	p := &Proxy{
		name:       np.Name,
		typ:        np.Type,
		clientID:   se.clientID,
		local:      np.Local,
		remotePort: port,
		ln:         ln,
		visitors:   make(chan net.Conn, 32),
		session:    se,
	}
	s.proxies[key] = p
	s.byPort[port] = p
	se.proxies[np.Name] = p

	resp.OK = true
	resp.RemotePort = port
	s.log.Info("代理注册", "client", se.clientID, "name", np.Name, "type", np.Type, "remote_port", port, "local", np.Local)
	go s.proxyAcceptLoop(p)
	return resp
}

// registerHTTPProxy 注册 vhost HTTP 代理:subdomain + vhost_domain 或自定义域名。
func (s *Server) registerHTTPProxy(se *Session, np proto.NewProxy) proto.NewProxyResp {
	resp := proto.NewProxyResp{ReqID: np.ReqID, Name: np.Name}
	if s.cfg.VhostHTTPPort == 0 {
		resp.Error = "VHOST_DISABLED: 服务端未开启 vhost"
		return resp
	}
	if np.Subdomain == "" && len(np.CustomDomains) == 0 {
		resp.Error = "VHOST_REQUIRED: http 代理需配置 subdomain 或 custom_domains"
		return resp
	}
	if np.Subdomain != "" && s.cfg.VhostDomain == "" {
		resp.Error = "VHOST_NEED_DOMAIN: 服务端未配置 vhost_domain"
		return resp
	}
	if _, _, err := net.SplitHostPort(np.Local); err != nil {
		resp.Error = "invalid local address"
		return resp
	}

	var hosts []string
	for _, d := range np.CustomDomains {
		hosts = append(hosts, normalizeHost(d))
	}
	if np.Subdomain != "" {
		hosts = append(hosts, normalizeHost(np.Subdomain)+"."+normalizeHost(s.cfg.VhostDomain))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := proxyKey(se.clientID, np.Name)
	if old, ok := s.proxies[key]; ok {
		if old.sameConfig(np) {
			return proto.NewProxyResp{ReqID: np.ReqID, Name: np.Name, OK: true}
		}
		resp.Error = "NAME_EXISTS: 同名代理配置不一致,请先 CloseProxy"
		return resp
	}
	for _, h := range hosts {
		if _, taken := s.vhost[h]; taken {
			resp.Error = fmt.Sprintf("VHOST_CONFLICT: 域名 %s 已被占用", h)
			return resp
		}
	}

	p := &Proxy{
		name:          np.Name,
		typ:           np.Type,
		clientID:      se.clientID,
		local:         np.Local,
		subdomain:     np.Subdomain,
		customDomains: np.CustomDomains,
		hosts:         hosts,
		visitors:      make(chan net.Conn, 32),
		session:       se,
	}
	s.proxies[key] = p
	se.proxies[np.Name] = p
	for _, h := range hosts {
		s.vhost[h] = p
	}

	resp.OK = true
	s.log.Info("vhost 代理注册", "client", se.clientID, "name", np.Name, "hosts", hosts, "local", np.Local)
	return resp
}

// sameConfig 代理配置是否一致(用于幂等判断)。
func (p *Proxy) sameConfig(np proto.NewProxy) bool {
	if p.typ != np.Type || p.local != np.Local {
		return false
	}
	if p.typ == "tcp" {
		return p.remotePort == np.RemotePort
	}
	return p.subdomain == np.Subdomain && config.SameStrings(p.customDomains, np.CustomDomains)
}

// closeProxy 客户端主动下线某代理。
func (s *Server) closeProxy(se *Session, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := se.proxies[name]
	if !ok {
		return
	}
	s.removeProxyLocked(p)
	s.log.Info("代理下线", "client", se.clientID, "name", name)
}

// allocPortLocked 从端口池取第一个空闲端口。调用方需持有 s.mu。
func (s *Server) allocPortLocked() int {
	for _, r := range s.ranges {
		for p := r.Min; p <= r.Max; p++ {
			if _, used := s.byPort[p]; !used {
				return p
			}
		}
	}
	return 0
}

// portAllowed 端口是否在白名单内。
func (s *Server) portAllowed(port int) bool {
	for _, r := range s.ranges {
		if r.Contains(port) {
			return true
		}
	}
	return false
}
