# 内网穿透工具（port）设计方案

> 目标：客户端位于内网（无公网 IP），服务端部署在公网服务器；
> 多个客户端连接服务端的**同一个入站端口**；每个客户端对外暴露**不同的出站端口**；
> 支持转发 SSH / HTTP 等服务；断线自动重连，长期运行稳定。

---

## 1. 需求分析与关键决策

| 需求 | 对应设计决策 |
|---|---|
| 客户端在内网 | 客户端主动**出站**连接服务端（反向隧道），无需 NAT 打洞 |
| 多客户端共用一个入站端口 | 单一**控制端口**（如 7000），按 clientID 区分连接 |
| 每个客户端出站端口不同 | 服务端按客户端配置动态监听 `remotePort`，端口池管理 + 冲突检测 |
| 转发 SSH | TCP 四层透明转发 |
| 转发 HTTP | TCP 转发 + **vhost 模式**（多个客户端可共享 80/443，按域名路由） |
| 断线重连 | 心跳检测 + 指数退避重连 + 代理状态自动重建 |
| 稳定性 | 连接多路复用、连接池、TCP keepalive、systemd 守护、优雅降级 |

**结论：采用 frp 式的"控制通道 + 多路复用数据通道"架构。** 下文先对比现有方案，再给出完整设计。

---

## 2. 现有方案调研对比

| 项目 | 语言 | 架构特点 | 多客户端单端口 | 端口复用 | 加密 | 备注 |
|---|---|---|---|---|---|---|
| **frp** (fatedier) | Go | 控制连接 + 工作连接，支持 tcpMux | ✅ bindPort 单端口 | vhost 80/443、tcpmux(httpconnect) | TLS + token/oidc | 生态最大，支持 tcp/udp/http/https/stcp/xtcp，KCP/QUIC 传输 |
| **rathole** (rapiz1) | Rust | 单连接多路复用，按服务名配对 | ✅ 单控制端口 | 无 vhost | Noise / TLS（强制 token） | 内存占用低、二进制 ~500KiB、支持热重载 |
| **nps** (ehang-io) | Go | 桥接端口 8024 + Web 管理面板 | ✅ bridge 单端口 | 域名(host)模式 80/443 | 自研加密 | 带 Web UI、多用户、socks5；项目维护趋缓 |
| **chisel** | Go | SSH 隧道 over HTTP/WebSocket | ✅ 单端口 | 无 | SSH 加密 | 主打单端口 socks5 反向代理 |
| **gost** | Go | 通用代理链 | 视配置 | 插件化 | 多种 | 功能广但非专为多客户端穿透设计 |

**共同范式**（值得借鉴）：
1. 客户端主动连接服务端单一控制端口，鉴权后注册代理服务；
2. 控制通道负责信令（注册/心跳/新建数据流通知），数据通道承载真实流量；
3. 数据通道要么在控制连接上做多路复用（smux/yamux），要么由客户端回拨建立独立工作连接；
4. HTTP 类服务通过 Host 头/子域名在共享 80 端口上路由（vhost），避免每客户端占用独立端口。

**自研 vs 直接用 frp**：如果目标是快速落地生产，直接用 frp 即可满足全部需求；
自研的价值在于可控、轻量、学习。本方案按自研设计，协议上大幅简化 frp。

---

## 3. 总体架构

```
                 公网服务器 (server, 1.2.3.4)
 ┌──────────────────────────────────────────────────┐
 │  :7000  控制端口（所有客户端连这里，TLS）            │
 │  :10022 用户A的 SSH  ←┐                            │
 │  :10023 用户B的 SSH  ←┤ 出站监听端口（每客户端独立）  │
 │  :80    vhost HTTP   ←┘ （a.example.com→A,        │
 │                          b.example.com→B）         │
 └───────▲───────────────────────────▲──────────────┘
         │ 一条 TCP+TLS 连接          │ 一条 TCP+TLS 连接
         │ (mux: 控制流+数据流)        │ (mux: 控制流+数据流)
   ┌─────┴──────┐              ┌──────┴─────┐
   │ client A    │              │ client B    │
   │ (内网1)     │              │ (内网2)     │
   └─────┬──────┘              └──────┬─────┘
         │ 127.0.0.1:22 (sshd)        │ 127.0.0.1:22 (sshd)
         │ 127.0.0.1:8080 (web)       │ 127.0.0.1:3000 (web)
```

**核心思路**：客户端与服务端之间只维持**一条长连接**（TCP+TLS），
其上运行 **smux** 多路复用：
- **stream 0**（或约定 ID）：控制流，传 JSON/msgpack 信令；
- 其余 stream：每条用户连接对应一个数据流（`io.Copy` 双向转发）。

> 备选方案（frp 模式）：客户端收到"新连接"信令后回拨服务端建立独立工作连接。
> 优点：单用户连接故障不影响整体、可做连接池预热降低首包延迟；
> 缺点：连接数多、NAT/防火墙回拨偶发失败。**本设计采用 mux 单连接方案**（rathole 同构），
> 实现更简单、穿透性最好（只有一条出站连接）。

---

## 4. 端口模型

### 4.1 服务端监听端口

| 端口 | 用途 | 说明 |
|---|---|---|
| 7000 | 控制入站（唯一必需） | 所有客户端连此端口，TLS |
| 10000–20000 | 动态出站端口池 | 按客户端配置申请，如 A 的 SSH=10022 |
| 80 | vhost HTTP | 多客户端共享，按 Host 路由（可选） |
| 7500 | 管理 API / Dashboard（可选） | 查看在线客户端、流量统计 |

### 4.2 出站端口分配规则

- 客户端在配置中声明想要的 `remote_port`；
- 服务端校验：是否在允许范围（`allow_ports: 10000-20000`）、是否已被占用；
- 冲突时拒绝该代理并返回明确错误（`PORT_IN_USE`），不影响该客户端其它代理；
- 客户端断线即释放端口；重连后重新申请，**配置不变则端口不变**，SSH 地址对使用者稳定；
- 同一客户端的多个代理共享其 mux 连接，按 stream 区分。

### 4.3 HTTP 的两种暴露方式

1. **TCP 模式**：和普通 TCP 一样占独立端口（如 `:18080`），零解析、零修改，最稳；
2. **vhost 模式**：客户端声明 `subdomain: api`（或自定义域名），服务端在 `:80` 上按 Host 头路由到对应客户端的流。多客户端共享 80 端口。可选配合 ACME 自动签发 HTTPS。

**建议 SSH 用 TCP 模式，Web 服务优先 vhost 模式。**

---

## 5. 协议设计

### 5.1 传输与帧格式

- 底层：TCP + TLS 1.3（服务端证书，客户端可配置跳过验证或 pinning 指纹）；
- 多路复用：[smux](https://github.com/xtaci/smux)（Go 实现成熟，自带 stream 流控、keepalive）；
- 控制流消息格式：`[1B 类型][4B 长度][payload]`，payload 用 JSON（调试友好）或 msgpack（紧凑）。首版建议 JSON。

### 5.2 消息类型（控制流）

| Type | 方向 | 说明 |
|---|---|---|
| `Login` | C→S | `{version, client_id, token, ts, hmac}`，hmac=HMAC(token, client_id+ts) 防重放 |
| `LoginResp` | S→C | `{ok, server_time, error}` |
| `NewProxy` | C→S | `{name, type(tcp/http), remote_port, subdomain, local_ip, local_port}` |
| `NewProxyResp` | S→C | `{name, ok, remote_port, error}` |
| `Ping` / `Pong` | 双向 | 心跳，含双方时间戳，可测 RTT |
| `CloseProxy` | C→S | 客户端主动下线某个代理 |
| `StartWorkConn` | S→C | 通知客户端有新用户连接进入某代理（触发客户端开新 stream） |

数据流打开时，客户端在新 stream 上先发一行元数据 `{proxy_name}`，服务端据此完成拼接。

### 5.3 TCP 代理转发时序

```
访客              服务端 :10022          控制流(mux s0)           客户端
 │  dial 1.2.3.4:10022      │                    │                  │
 │─────────────────────────▶│                    │                  │
 │                          │── StartWorkConn ──▶│                  │
 │                          │                    │──── 开 mux stream s5，写 {proxy:"ssh"} ────▶│
 │                          │◀───────────────────────────────────────────────────────────────│
 │                          │  join(访客conn, s5) │                  │── dial 127.0.0.1:22
 │◀═══════════════ io.Copy 双向透传 ══════════════▶│◀══════════ io.Copy ═════════▶ sshd
```

### 5.4 HTTP vhost 路由

服务端 `:80` 收到请求后读 Host 头 → 查路由表 `host → (client, proxy)` →
同样通过 `StartWorkConn` 建立流，把整个 HTTP 连接透传（首版按 TCP 透传处理 HTTP，
不做七层解析，简单且不会破坏 WebSocket/长连接）。后续可扩展：
- Host 重写、`X-Forwarded-For` 注入；
- HTTPS 终止（服务端持证书）+ 回源明文。

---

## 6. 断线重连与稳定性设计（重点）

### 6.1 心跳与探活

- 控制流应用层心跳：客户端每 **15s** 发 Ping，服务端 **90s** 未收到任何消息判死，关闭连接；
- smux 层自带 keepalive 作为第二道保险；
- 系统层开启 TCP keepalive（`30s/3次`），尽早发现半开连接（NAT 会话老化典型 60–300s）；
- 客户端侧同样检测：心跳超时 → 主动关闭，进入重连流程。

### 6.2 重连策略

```
失败 → 等待 backoff = min(base * 2^n + jitter(±20%), 60s) → 重连
base = 1s，连续成功运行 >5min 后重置 n
```

- 重连成功后：重新 Login → **重新注册全部代理**（服务端端口按配置重新申请，对外地址不变）；
- 服务端容忍"旧连接尚未判死、新连接已来"的情况：以新连接替换旧连接（按 clientID 踢旧），
  避免 NAT 环境下僵尸连接占住端口；
- 代理注册是**幂等**的：重复 NewProxy 同名代理直接返回成功。

### 6.3 数据面容错

- 单个数据流出错（内网服务拒绝、流中断）：只关闭该 stream 和对应访客连接，**不影响控制通道和其它代理**；
- mux 连接整体断开时：服务端立即关闭该客户端所有访客连接（让访客收到 RST 而非挂起），释放端口；
- 内网服务短暂不可用：客户端 dial 本地失败 → 向访客返回连接重置；SSH 客户端重试即可；
- 大流量背压：smux 每 stream 自带滑动窗口流控，慢访客不会撑爆内存；另设单连接写缓冲上限兜底。

### 6.4 进程级稳定性

- 双端均以 **systemd**（或 launchd）守护：`Restart=always, RestartSec=3`；
- 配置热重载（监听 SIGHUP 或 fsnotify）：新增代理不重启进程、不断现有连接（借鉴 rathole）；
- 结构化日志 + 可选 Prometheus 指标（在线客户端数、每代理连接数、字节数、重连次数）；
- 服务端对单客户端做连接数/带宽软限制，防止异常打满。

### 6.5 可选增强（二期）

- **连接池**：客户端预先开 N 条空闲 mux 连接，控制连接断开瞬间数据面可秒级切换；
- **QUIC 传输**：0-RTT 重连、抗丢包、连接迁移（客户端切换 WiFi/4G 不断线）——frp 已支持，可后期引入 `quic-go`；
- **多端容灾**：客户端配置多个服务端地址轮询。

---

## 7. 安全设计

| 层 | 措施 |
|---|---|
| 传输 | 全程 TLS 1.3；服务端证书 + 客户端可选指纹 pinning |
| 鉴权 | 预共享 token，Login 用 HMAC( token, clientID+时间戳 )，时间戳窗口 ±5min 防重放；token 不上网 |
| 授权 | 服务端按客户端配置 `allow_ports` 白名单，客户端只能申请指定端口段 |
| 防滥用 | 出站监听默认绑 `0.0.0.0`（可配置）；服务端限新建连接速率；可选访客 IP 白名单 |
| 管理面 | Dashboard/管理 API 单独端口 + 独立口令，不对外或绑 127.0.0.1 |
| 审计 | 记录登录、代理注册、端口分配、访客来源 IP |

---

## 8. 技术选型

**推荐 Go**（frp/nps 同语言，net 库与 smux 生态成熟，单二进制交叉编译方便部署到路由器/NAS）：

| 组件 | 选择 | 理由 |
|---|---|---|
| 多路复用 | `github.com/xtaci/smux` | 轻量、带流控与 keepalive；备选 hashicorp/yamux |
| TLS | 标准库 `crypto/tls` | 支持证书指纹 pinning |
| 配置 | YAML（`gopkg.in/yaml.v3`） | 人读友好；热重载用 `fsnotify` |
| 信令编码 | JSON 起步，预留 msgpack | 调试优先 |
| 日志 | `log/slog` | 标准库结构化日志 |
| 指标（可选） | `prometheus/client_golang` | 运维观测 |
| 进程守护 | systemd unit 模板 | 随仓库提供 |

Rust 备选（rathole 路线）：内存更省、二进制更小，但开发效率与生态上手成本高于 Go。首版建议 Go。

---

## 9. 配置示例

**服务端 `server.yaml`**
```yaml
bind_addr: 0.0.0.0
bind_port: 7000
tls:
  cert: /etc/port/server.crt
  key:  /etc/port/server.key
auth:
  token: "change-me-strong-token"
allow_ports: "10000-20000"        # 出站端口白名单
vhost_http_port: 80               # 可选
dashboard:                        # 可选
  addr: 127.0.0.1:7500
  user: admin
  password: admin
heartbeat: { interval: 15s, timeout: 90s }
```

**客户端 A `client.yaml`**
```yaml
server_addr: 1.2.3.4:7000
client_id: home-nas
token: "change-me-strong-token"
tls:
  server_fingerprint: "SHA256:xxxx"   # pinning，可选
reconnect: { base: 1s, max: 60s }

proxies:
  - name: ssh
    type: tcp
    local: 127.0.0.1:22
    remote_port: 10022            # 出站端口（每客户端不同）
  - name: web
    type: http
    local: 127.0.0.1:8080
    subdomain: nas                # → nas.example.com:80
```

**客户端 B**：`client_id: office`，`remote_port: 10023`，`subdomain: office`。
两个客户端都连 `1.2.3.4:7000`。

使用方式：`ssh -p 10022 user@1.2.3.4`、`curl http://nas.example.com`。

---

## 10. 项目结构与里程碑

```
port/
├── cmd/
│   ├── server/main.go
│   └── client/main.go
├── internal/
│   ├── proto/        # 帧格式、消息定义、编解码
│   ├── mux/          # smux 封装
│   ├── auth/         # token + HMAC 握手
│   ├── server/       # 控制连接管理、端口池、vhost 路由、访客监听
│   ├── client/       # 登录、重连循环、代理注册、本地转发
│   └── config/       # 配置加载与热重载
├── configs/          # 示例配置
└── deploy/           # systemd unit 模板
```

| 里程碑 | 内容 | 验收 |
|---|---|---|
| M1 最小可用 | TCP 转发：登录鉴权 + 单代理 + mux 数据流 | `ssh -p 10022` 通 |
| M2 多客户端 | 端口池、冲突处理、多代理并发 | 两客户端同时在线互不干扰 |
| M3 稳定性 | 心跳、指数退避重连、断线清理、状态重建 | 拔网线 60s 恢复后 SSH 可用原端口重连 |
| M4 HTTP | vhost 路由、subdomain、Host 透传 | 两客户端共享 80 端口按域名访问 |
| M5 安全加固 | TLS pinning、allow_ports、限流、审计日志 | 渗透自查通过 |
| M6 运维 | 热重载、Dashboard、Prometheus、systemd 模板 | 配置热更新不断连 |
| M7 增强（可选） | QUIC 传输、连接池、HTTPS 终止 + ACME | 弱网/切网场景验证 |

---

## 11. 风险与注意事项

1. **NAT 会话老化**：长连接必须用心跳顶住（15s 足够覆盖常见 60s+ 老化时间）；
2. **运营商封锁/防火墙**：控制端口可被深度检测，必要时走 WebSocket/HTTPS 伪装（备选 transport）；
3. **单连接瓶颈**：所有流量挤一条 TCP 存在队头阻塞，极端高并发场景可开 2–4 条 mux 连接分片；
4. **合规**：服务端暴露在公网，务必改默认口令、限制端口范围、及时更新 TLS 配置；
5. **端口规划**：建议制定规范，如 `10000+客户端编号*10+服务序号`，避免人肉管理冲突。
