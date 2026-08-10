// Package config 加载服务端/客户端 YAML 配置,并做基础校验。
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 支持 "15s" 形式的 YAML 时长。
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// SameStrings 无序比较两个字符串集合。
func SameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ma := make(map[string]int, len(a))
	for _, s := range a {
		ma[s]++
	}
	for _, s := range b {
		if ma[s] == 0 {
			return false
		}
		ma[s]--
	}
	return true
}

// ---- 服务端 ----

// Server 服务端配置。
type Server struct {
	BindAddr string    `yaml:"bind_addr"`
	BindPort int       `yaml:"bind_port"`
	TLS      ServerTLS `yaml:"tls"`
	Auth     Auth      `yaml:"auth"`
	// AllowPorts 出站端口白名单,如 "20000-30000" 或 "20000,20050-20060"
	AllowPorts    string    `yaml:"allow_ports"`
	VhostHTTPPort int       `yaml:"vhost_http_port"`
	VhostDomain   string    `yaml:"vhost_domain"`
	Dashboard     Dashboard `yaml:"dashboard"`
	Heartbeat     Heartbeat `yaml:"heartbeat"`
}

// ServerTLS 服务端 TLS 配置(cert/key 留空则纯文本,仅建议内网调试)。
type ServerTLS struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// Auth 预共享 token。
type Auth struct {
	Token string `yaml:"token"`
}

// Dashboard 管理面板。
type Dashboard struct {
	Addr     string `yaml:"addr"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

// Heartbeat 心跳参数。
type Heartbeat struct {
	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`
}

func defaultServer() *Server {
	return &Server{
		BindAddr:   "0.0.0.0",
		BindPort:   17200,
		AllowPorts: "20000-30000",
		Heartbeat:  Heartbeat{Interval: Duration(15 * time.Second), Timeout: Duration(90 * time.Second)},
	}
}

// LoadServer 读取并校验服务端配置。
func LoadServer(path string) (*Server, error) {
	s := defaultServer()
	if err := loadYAML(path, s); err != nil {
		return nil, err
	}
	if s.BindPort <= 0 || s.BindPort > 65535 {
		return nil, errors.New("bind_port 无效")
	}
	if s.Auth.Token == "" {
		return nil, errors.New("auth.token 不能为空")
	}
	if _, err := ParsePortRanges(s.AllowPorts); err != nil {
		return nil, fmt.Errorf("allow_ports: %w", err)
	}
	if t := time.Duration(s.Heartbeat.Timeout); t <= 0 || time.Duration(s.Heartbeat.Interval) <= 0 {
		return nil, errors.New("heartbeat 必须为正")
	}
	if (s.TLS.Cert == "") != (s.TLS.Key == "") {
		return nil, errors.New("tls.cert 与 tls.key 必须同时配置或同时留空")
	}
	return s, nil
}

func loadYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// ---- 端口范围 ----

// PortRange 一个闭区间端口段。
type PortRange struct {
	Min, Max int
}

// Contains 端口是否在该段内。
func (r PortRange) Contains(p int) bool { return p >= r.Min && p <= r.Max }

// ParsePortRanges 解析 "20000-30000"、"20000,20050-20060" 形式的端口白名单。
func ParsePortRanges(s string) ([]PortRange, error) {
	var out []PortRange
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "-") {
			segs := strings.SplitN(part, "-", 2)
			min, err1 := strconv.Atoi(strings.TrimSpace(segs[0]))
			max, err2 := strconv.Atoi(strings.TrimSpace(segs[1]))
			if err1 != nil || err2 != nil || min <= 0 || max < min || max > 65535 {
				return nil, fmt.Errorf("非法端口段 %q", part)
			}
			out = append(out, PortRange{Min: min, Max: max})
		} else {
			p, err := strconv.Atoi(part)
			if err != nil || p <= 0 || p > 65535 {
				return nil, fmt.Errorf("非法端口 %q", part)
			}
			out = append(out, PortRange{Min: p, Max: p})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("端口白名单为空")
	}
	return out, nil
}

// ---- 客户端 ----

// Client 客户端配置。
type Client struct {
	ServerAddr string    `yaml:"server_addr"`
	ClientID   string    `yaml:"client_id"`
	Token      string    `yaml:"token"`
	TLS        ClientTLS `yaml:"tls"`
	Heartbeat  Heartbeat `yaml:"heartbeat"`
	Reconnect  Reconnect `yaml:"reconnect"`
	Proxies    []Proxy   `yaml:"proxies"`
}

// ClientTLS 客户端 TLS 配置。
type ClientTLS struct {
	Enabled           bool   `yaml:"enabled"`
	SkipVerify        bool   `yaml:"skip_verify"`
	ServerFingerprint string `yaml:"server_fingerprint"` // 例 "SHA256:abcd...";启用后按指纹 pinning
}

// Reconnect 重连退避参数。
type Reconnect struct {
	Base Duration `yaml:"base"`
	Max  Duration `yaml:"max"`
}

// Proxy 一条代理配置。
type Proxy struct {
	Name          string   `yaml:"name"`
	Type          string   `yaml:"type"` // tcp | http
	Local         string   `yaml:"local"` // 内网地址 ip:port
	RemotePort    int      `yaml:"remote_port"`
	Subdomain     string   `yaml:"subdomain"`
	CustomDomains []string `yaml:"custom_domains"`
}

func defaultClient() *Client {
	return &Client{
		TLS:       ClientTLS{Enabled: true},
		Heartbeat: Heartbeat{Interval: Duration(15 * time.Second), Timeout: Duration(90 * time.Second)},
		Reconnect: Reconnect{Base: Duration(time.Second), Max: Duration(60 * time.Second)},
	}
}

// LoadClient 读取并校验客户端配置。
func LoadClient(path string) (*Client, error) {
	c := defaultClient()
	if err := loadYAML(path, c); err != nil {
		return nil, err
	}
	if c.ServerAddr == "" {
		return nil, errors.New("server_addr 不能为空")
	}
	if c.ClientID == "" {
		return nil, errors.New("client_id 不能为空")
	}
	if c.Token == "" {
		return nil, errors.New("token 不能为空")
	}
	if t := time.Duration(c.Heartbeat.Timeout); t <= 0 || time.Duration(c.Heartbeat.Interval) <= 0 {
		return nil, errors.New("heartbeat 必须为正")
	}
	if b := time.Duration(c.Reconnect.Base); b <= 0 || time.Duration(c.Reconnect.Max) <= 0 || time.Duration(c.Reconnect.Max) < b {
		return nil, errors.New("reconnect 配置无效")
	}
	seen := map[string]bool{}
	for _, p := range c.Proxies {
		if p.Name == "" {
			return nil, errors.New("proxy name 不能为空")
		}
		if p.Type != "tcp" && p.Type != "http" {
			return nil, fmt.Errorf("proxy %q: 不支持的 type %q", p.Name, p.Type)
		}
		if _, _, err := net.SplitHostPort(p.Local); err != nil {
			return nil, fmt.Errorf("proxy %q: 非法 local 地址 %q", p.Name, p.Local)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("proxy name 重复: %q", p.Name)
		}
		seen[p.Name] = true
	}
	return c, nil
}
