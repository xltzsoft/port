#!/bin/sh
# port 端到端冒烟测试:
#   1) TCP 转发(echo)  2) TCP 转发(HTTP)  3) vhost 路由  4) dashboard
#   5) 客户端被 kill 后重连恢复  6) 服务端重启后客户端自动恢复  7) 端口冲突拒绝
# 用法: ./scripts/smoke_test.sh [port-server] [port-client]
set -eu

SRV="${1:-./port-server}"
CLI="${2:-./port-client}"
TMP=$(mktemp -d /tmp/port-smoke.XXXXXX)
PIDS=""
PASS=0
FAIL=0

cleanup() {
  for p in $PIDS; do kill -9 "$p" 2>/dev/null || true; done
  rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

say()  { printf '\n=== %s ===\n' "$*"; }
ok()   { echo "  PASS: $*"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL: $*"; FAIL=$((FAIL+1)); }
check(){ if [ "$1" -eq 0 ]; then ok "$2"; else bad "$2"; fi; }

# ---- 本地测试服务 ----
mkdir -p "$TMP/web"
echo "port-smoke-ok" > "$TMP/web/index.html"
python3 -m http.server 18080 --bind 127.0.0.1 --directory "$TMP/web" >/dev/null 2>&1 &
PIDS="$PIDS $!"
cat > "$TMP/echo.py" <<'EOF'
import socket, threading
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('127.0.0.1', 18122)); s.listen(16)
while True:
    c, _ = s.accept()
    threading.Thread(target=lambda: (c.sendall(b'hello from local\n'), c.close()), daemon=True).start()
EOF
python3 "$TMP/echo.py" &
PIDS="$PIDS $!"
sleep 0.3

# ---- 服务端 ----
cat > "$TMP/server.yaml" <<EOF
bind_addr: 0.0.0.0
bind_port: 17200
auth: { token: "smoke-token" }
allow_ports: "20000-30000"
vhost_http_port: 17280
vhost_domain: "example.com"
dashboard: { addr: "127.0.0.1:17250", user: "admin", password: "admin123" }
heartbeat: { interval: 2s, timeout: 6s }
EOF
"$SRV" -c "$TMP/server.yaml" >"$TMP/server.log" 2>&1 &
PIDS="$PIDS $!"
sleep 0.5

# ---- 客户端 A ----
cat > "$TMP/client-a.yaml" <<EOF
server_addr: "127.0.0.1:17200"
client_id: "smoke-a"
token: "smoke-token"
tls: { enabled: false }
heartbeat: { interval: 2s, timeout: 6s }
reconnect: { base: 1s, max: 3s }
proxies:
  - { name: echo,  type: tcp,  local: 127.0.0.1:18122, remote_port: 20022 }
  - { name: web,   type: tcp,  local: 127.0.0.1:18080, remote_port: 20080 }
  - { name: web-vhost, type: http, local: 127.0.0.1:18080, subdomain: nas }
EOF
"$CLI" -c "$TMP/client-a.yaml" >"$TMP/client-a.log" 2>&1 &
PIDS="$PIDS $!"
sleep 1

say "1. TCP 转发(echo 20022)"
cat > "$TMP/tcp_client.py" <<'EOF'
import socket, sys
s = socket.create_connection(('127.0.0.1', 20022), timeout=3)
s.sendall(b'ping\n')  # 保持连接等响应(与 SSH/HTTP 客户端一致,不提前半关闭)
s.settimeout(3)
data = s.recv(100)
print(data.decode().strip())
EOF
OUT=$(python3 "$TMP/tcp_client.py" 2>/dev/null || true)
[ "$OUT" = "hello from local" ] && ok "echo 转发正常" || bad "echo 转发失败: [$OUT]"

say "2. TCP 转发(HTTP 20080)"
curl -fsS -m 3 http://127.0.0.1:20080/ | grep -q port-smoke-ok && ok "HTTP 转发正常" || bad "HTTP 转发失败"

say "3. vhost 路由(17280, Host=nas.example.com)"
curl -fsS -m 3 -H "Host: nas.example.com" http://127.0.0.1:17280/ | grep -q port-smoke-ok && ok "vhost 路由正常" || bad "vhost 路由失败"
CODE=$(curl -sS -m 3 -o /dev/null -w "%{http_code}" -H "Host: ghost.example.com" http://127.0.0.1:17280/)
[ "$CODE" = "404" ] && ok "未知域名返回 404" || bad "未知域名返回 $CODE"

say "4. 管理面板(17250)"
curl -fsS -m 3 -u admin:admin123 http://127.0.0.1:17250/api/clients | grep -q "smoke-a" && ok "dashboard 显示在线客户端" || bad "dashboard 未显示客户端"
curl -fsS -m 3 -o /dev/null -w "%{http_code}" http://127.0.0.1:17250/api/clients | grep -q 401 && ok "dashboard 未授权返回 401" || bad "dashboard 鉴权失效"

say "5. 客户端被 kill 后自动重连"
kill -9 $(pgrep -f "client-a.yaml") 2>/dev/null || true
# 重新拉起客户端(模拟进程崩溃后由 systemd 拉起)
"$CLI" -c "$TMP/client-a.yaml" >>"$TMP/client-a.log" 2>&1 &
PIDS="$PIDS $!"
sleep 2
curl -fsS -m 3 http://127.0.0.1:20080/ | grep -q port-smoke-ok && ok "重连后 HTTP 转发恢复(端口不变)" || bad "重连后未恢复"

say "6. 服务端重启后客户端自动恢复"
kill -9 $(pgrep -f "port-server.*server.yaml") 2>/dev/null || true
sleep 0.3
"$SRV" -c "$TMP/server.yaml" >>"$TMP/server.log" 2>&1 &
PIDS="$PIDS $!"
sleep 4
curl -fsS -m 3 http://127.0.0.1:20080/ | grep -q port-smoke-ok && ok "服务端重启后转发恢复" || bad "服务端重启后未恢复"

say "7. 端口冲突:客户端 B 申请已被占用的 20022"
cat > "$TMP/client-b.yaml" <<EOF
server_addr: "127.0.0.1:17200"
client_id: "smoke-b"
token: "smoke-token"
tls: { enabled: false }
heartbeat: { interval: 2s, timeout: 6s }
reconnect: { base: 1s, max: 3s }
proxies:
  - { name: clash, type: tcp, local: 127.0.0.1:18122, remote_port: 20022 }
  - { name: ok,    type: tcp, local: 127.0.0.1:18122, remote_port: 20023 }
EOF
"$CLI" -c "$TMP/client-b.yaml" >"$TMP/client-b.log" 2>&1 &
PIDS="$PIDS $!"
sleep 1.5
grep -q "PORT_IN_USE" "$TMP/client-b.log" && ok "冲突代理被拒绝" || bad "冲突代理未被拒绝"
sed "s/20022/20023/" "$TMP/tcp_client.py" > "$TMP/tcp_client_b.py"
OUT=$(python3 "$TMP/tcp_client_b.py" 2>/dev/null || true)
[ "$OUT" = "hello from local" ] && ok "客户端 B 的非冲突代理正常" || bad "客户端 B 非冲突代理失败: [$OUT]"
curl -fsS -m 3 -u admin:admin123 http://127.0.0.1:17250/api/clients | grep -q '"client_id":"smoke-b"' && ok "dashboard 同时显示两个客户端" || bad "dashboard 未显示客户端 B"

say
echo "结果: $PASS 通过, $FAIL 失败"
[ "$FAIL" -eq 0 ] || exit 1
