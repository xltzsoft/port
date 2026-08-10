package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"
)

// dashProxy / dashClient 管理面板展示用的快照结构。
type dashProxy struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Local       string `json:"local"`
	RemotePort  int    `json:"remote_port"`
	Subdomain   string `json:"subdomain"`
	ActiveConns int64  `json:"active_conns"`
}

type dashClient struct {
	ClientID    string      `json:"client_id"`
	RemoteAddr  string      `json:"remote_addr"`
	LoginAt     string      `json:"login_at"`
	Uptime      string      `json:"uptime"`
	BytesIn     int64       `json:"bytes_in"`
	BytesOut    int64       `json:"bytes_out"`
	ActiveConns int64       `json:"active_conns"`
	Proxies     []dashProxy `json:"proxies"`
}

// snapshot 汇总在线客户端与代理状态。
func (s *Server) snapshot() []dashClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]dashClient, 0, len(s.sessions))
	for _, se := range s.sessions {
		dc := dashClient{
			ClientID:    se.clientID,
			RemoteAddr:  se.remoteAddr,
			LoginAt:     se.loginAt.Format(time.RFC3339),
			Uptime:      time.Since(se.loginAt).Round(time.Second).String(),
			BytesIn:     se.bytesIn.Load(),
			BytesOut:    se.bytesOut.Load(),
			ActiveConns: se.activeConns.Load(),
		}
		for _, p := range se.proxies {
			dc.Proxies = append(dc.Proxies, dashProxy{
				Name:        p.name,
				Type:        p.typ,
				Local:       p.local,
				RemotePort:  p.remotePort,
				Subdomain:   p.subdomain,
				ActiveConns: p.active.Load(),
			})
		}
		out = append(out, dc)
	}
	return out
}

// serveDashboard 管理面板 HTTP 服务(端口 17250)。
func (s *Server) serveDashboard(ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.dashIndex)
	mux.HandleFunc("/api/clients", s.dashClients)
	mux.HandleFunc("/api/health", s.dashHealth)
	handler := http.Handler(mux)
	if s.cfg.Dashboard.User != "" {
		handler = s.basicAuth(handler)
	}
	if err := http.Serve(ln, handler); err != nil && !errors.Is(err, net.ErrClosed) {
		s.log.Warn("dashboard 服务退出", "err", err)
	}
}

// basicAuth 管理面板 Basic Auth。
func (s *Server) basicAuth(next http.Handler) http.Handler {
	user := s.cfg.Dashboard.User
	pass := s.cfg.Dashboard.Password
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="port"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) dashIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashIndexHTML))
}

func (s *Server) dashClients(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"server": map[string]any{
			"started_at":  s.startedAt.Format(time.RFC3339),
			"uptime":      time.Since(s.startedAt).Round(time.Second).String(),
			"control":     s.controlAddr(),
			"allow_ports": s.cfg.AllowPorts,
		},
		"clients": s.snapshot(),
	})
}

func (s *Server) dashHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

const dashIndexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>port 管理面板</title>
<style>
  body{font-family:-apple-system,"PingFang SC",sans-serif;margin:2rem;background:#fafafa;color:#222}
  h1{font-size:1.4rem}
  table{border-collapse:collapse;width:100%;background:#fff;box-shadow:0 1px 3px rgba(0,0,0,.1)}
  th,td{border:1px solid #e5e5e5;padding:8px 12px;text-align:left;font-size:.9rem;vertical-align:top}
  th{background:#f2f2f2}
  .muted{color:#888}
</style>
</head>
<body>
<h1>port 内网穿透 · 管理面板</h1>
<p class="muted" id="meta">加载中…</p>
<table>
<thead><tr>
  <th>client_id</th><th>来源地址</th><th>登录时间</th><th>在线时长</th>
  <th>下行(B)</th><th>上行(B)</th><th>活动连接</th><th>代理</th>
</tr></thead>
<tbody id="rows"><tr><td colspan="8" class="muted">加载中…</td></tr></tbody>
</table>
<script>
async function refresh(){
  try{
    const r=await fetch('/api/clients');
    if(r.status===401){document.getElementById('meta').textContent='认证失败,请检查服务端 dashboard.user/password';return;}
    const d=await r.json();
    document.getElementById('meta').textContent='服务端 '+d.server.control+' 已运行 '+d.server.uptime+' · 在线客户端 '+d.clients.length+' 个 · 端口池 '+d.server.allow_ports;
    const tbody=document.getElementById('rows');tbody.innerHTML='';
    if(d.clients.length===0){tbody.innerHTML='<tr><td colspan="8" class="muted">暂无在线客户端</td></tr>';return;}
    for(const c of d.clients){
      const tr=document.createElement('tr');
      const proxies=c.proxies.map(p=>{
        const port=p.remote_port?(':'+p.remote_port):(p.subdomain?('vhost:'+p.subdomain):'');
        return p.name+' <span class="muted">'+p.type+'→'+p.local+port+' ×'+p.active_conns+'</span>';
      }).join('<br>');
      tr.innerHTML='<td>'+c.client_id+'</td><td>'+c.remote_addr+'</td><td>'+c.login_at+'</td><td>'+c.uptime+'</td><td>'+c.bytes_in+'</td><td>'+c.bytes_out+'</td><td>'+c.active_conns+'</td><td>'+proxies+'</td>';
      tbody.appendChild(tr);
    }
  }catch(e){document.getElementById('meta').textContent='加载失败: '+e;}
}
refresh();setInterval(refresh,2000);
</script>
</body></html>`
