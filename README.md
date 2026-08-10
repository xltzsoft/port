# port — 内网穿透工具

参考 [docs/design.md](docs/design.md) 实现。客户端位于内网(无公网 IP),服务端部署在公网服务器;
多个客户端连接服务端的**同一控制端口**,每个客户端对外暴露**不同的出站端口**;
支持 TCP(SSH 等)与 vhost HTTP 转发;断线自动重连,长期运行稳定。

| 端口 | 用途 |
|---|---|
| **17200** | 控制入站端口(所有客户端连这里,TLS 可选) |
| **20000–30000** | 出站转发端口池(每客户端独立) |
| **17280** | vhost HTTP 共享端口(可选,多客户端按域名路由) |
| **17250** | 管理面板 / API(可选) |

## 快速开始

### 1. 构建

```sh
make build          # 生成 ./port-server 与 ./port-client
```

### 2. 配置

修改 `configs/server.yaml` 与 `configs/client.yaml`:

```sh
# 双端必须一致
auth.token: "改成强随机串"

# 服务端: 部署到公网机器
./port-server -c configs/server.yaml

# 客户端: 运行在内网机器, 每个客户端一个唯一 client_id
./port-client -c configs/client.yaml
```

### 3. 验证

```sh
# TCP 模式(如 SSH, 每客户端不同端口)
ssh -p 20022 user@SERVER_IP

# TCP 模式转发内网 Web
curl http://SERVER_IP:20080/

# vhost 模式(多客户端共享 17280, 按 Host 路由)
curl -H "Host: nas.example.com" http://SERVER_IP:17280/

# 管理面板(admin/admin123, 见服务端配置)
curl -u admin:admin123 http://SERVER_IP:17250/api/clients
```

## 架构

```
                公网服务器 (server)
 ┌─────────────────────────────────────────────┐
 │ :17200  控制端口(所有客户端连这里, TLS 可选)   │
 │ :20022  用户A的 SSH  ←┐                      │
 │ :20023  用户B的 SSH  ←┤ 出站端口池 20000-30000│
 │ :17280  vhost HTTP   ←┘ (nas.example.com→A)  │
 │ :17250  管理面板                              │
 └──────▲──────────────────────────▲───────────┘
        │ 一条 TCP+TLS 连接          │ 一条 TCP+TLS 连接
        │ (smux 多路复用)            │ (smux 多路复用)
  ┌─────┴──────┐             ┌──────┴─────┐
  │ client A   │             │ client B   │
  │ 127.0.0.1:22             │ 127.0.0.1:22
```

- 客户端与服务端之间只维持**一条长连接**(TCP+TLS),其上运行 smux 多路复用;
- 第一条 stream 为控制流(JSON 信令),其余 stream 为数据流(`io.Copy` 双向透传);
- 访客到达服务端出站端口 → 服务端通知客户端 → 客户端拨内网服务成功后开数据流 → join 透传;
  内网服务拨不通则不开流,访客超时收到连接重置,不影响其它代理。

## 断线重连

- 应用层心跳: 客户端每 **15s** Ping,服务端 **90s** 无任何消息判死(可配置);
- smux 层 keepalive 与 TCP keepalive(30s)作为第二、三道保险;
- 重连采用**指数退避 + 抖动**(base 1s, max 60s),连续稳定运行 5 分钟后重置;
- 重连后重新 Login → 重新注册全部代理(幂等),**配置不变则对外端口不变**,SSH 地址稳定;
- 同 clientID 新连接会踢掉旧连接,回收 NAT 环境下僵尸连接占用的端口。

## 配置热重载

客户端支持 `kill -HUP <pid>` 热重载: 新增/变更的代理即时注册,删除的立即下线,**不重启进程、不断现有连接**。

## TLS 与安全

```sh
# 生成自签证书(生产建议用受信任 CA 或 ACME)
sh deploy/gen_cert.sh /etc/port

# 服务端 configs/server.yaml
tls:
  cert: /etc/port/server.crt
  key:  /etc/port/server.key

# 客户端 configs/client.yaml: 三种方式任选
tls:
  enabled: true
  skip_verify: true                  # ① 跳过证书链校验(仅内网/测试)
  # server_fingerprint: "SHA256:FC:4A:..."  # ② 指纹 pinning(推荐, 防中间人)
  # ③ 不填则走系统信任链(证书需由受信任 CA 签发)
```

- 登录鉴权: 预共享 token + HMAC-SHA256(`clientID|ts`),token 永不上网,±5min 时间窗防重放;
- 出站端口白名单: 客户端只能申请 `allow_ports` 内的端口;
- 管理面板独立端口 + Basic Auth。

## systemd 部署

```sh
sudo cp port-server port-client /usr/local/bin/
sudo mkdir -p /etc/port && sudo cp configs/*.yaml deploy/gen_cert.sh /etc/port/
sudo cp deploy/port-server.service deploy/port-client.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now port-server
# 内网机器上:
sudo systemctl enable --now port-client
journalctl -u port-server -f
```

## 目录结构

```
cmd/server, cmd/client     # 入口
internal/proto             # 帧格式、消息、编解码
internal/auth              # token + HMAC 握手
internal/mux               # smux 封装 + 双向透传
internal/config            # YAML 配置加载与校验、端口白名单解析
internal/server            # 控制连接/会话、端口池、代理注册、vhost 路由、管理面板
internal/client            # 登录、重连循环、代理注册、数据流转发、热重载
configs/                   # 示例配置
deploy/                    # systemd unit 模板 + 证书生成脚本
scripts/smoke_test.sh      # 端到端冒烟测试
```

## 测试

```sh
./scripts/smoke_test.sh
# 覆盖: TCP 转发 / vhost 路由 / dashboard / 客户端断线重连 /
#       服务端重启恢复 / 端口冲突拒绝 / 多客户端并存
```

## 已知限制与路线图

- 数据流 join 采用"任一端 EOF 即关闭两端"(与 frp 一致): 对 SSH/HTTP 等真实客户端无影响,
  但**不支持半关闭语义**(如 `printf x | nc` 这类发送后立刻半关闭的客户端会截断回包);
- 尚未实现: Prometheus 指标、连接池预热、QUIC 传输、HTTPS 终止 + ACME(见 design.md M6/M7);
- 单连接存在 TCP 队头阻塞,极端高并发可开多条 mux 连接分片(roadmap)。
