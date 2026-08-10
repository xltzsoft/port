// Package proto 定义 port 的控制流帧格式与消息。
//
// 控制流(control stream)使用自定义帧: [1B 类型][4B 大端长度][payload JSON]。
// 数据流(work stream)第一个帧为 StreamMeta(同样帧格式),之后为纯字节透传。
package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

const (
	// FrameHeaderSize 帧头大小: 1B 类型 + 4B 长度
	FrameHeaderSize = 5
	// MaxFrameSize 单帧 payload 上限,防止异常帧撑爆内存
	MaxFrameSize = 1 << 20
	// Version 协议版本
	Version = "1.0"
)

// 控制流消息类型
const (
	TypeLogin         byte = 1  // C→S 登录
	TypeLoginResp     byte = 2  // S→C 登录应答
	TypeNewProxy      byte = 3  // C→S 注册代理
	TypeNewProxyResp  byte = 4  // S→C 注册结果
	TypeCloseProxy    byte = 5  // C→S 下线代理
	TypeStartWorkConn byte = 6  // S→C 通知客户端有访客进入,请开数据流
	TypePing          byte = 7  // C→S 心跳
	TypePong          byte = 8  // S→C 心跳应答
	TypeError         byte = 9  // S→C 错误提示
	TypeStreamMeta    byte = 10 // 仅数据流首帧:声明所属代理
)

// Login 客户端登录请求。token 不落网络,只传 HMAC。
type Login struct {
	Version  string `json:"version"`
	ClientID string `json:"client_id"`
	Ts       int64  `json:"ts"`
	Hmac     string `json:"hmac"`
}

// LoginResp 登录应答。
type LoginResp struct {
	OK         bool   `json:"ok"`
	ServerTime int64  `json:"server_time"`
	Error      string `json:"error,omitempty"`
}

// NewProxy 代理注册请求。
type NewProxy struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"` // tcp | http
	Local         string   `json:"local"` // 内网地址 ip:port
	RemotePort    int      `json:"remote_port"`
	Subdomain     string   `json:"subdomain,omitempty"`
	CustomDomains []string `json:"custom_domains,omitempty"`
	ReqID         int      `json:"req_id,omitempty"` // 客户端请求 ID,应答时原样带回
}

// NewProxyResp 代理注册应答。
type NewProxyResp struct {
	ReqID      int    `json:"req_id,omitempty"`
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	RemotePort int    `json:"remote_port"`
	Error      string `json:"error,omitempty"`
}

// CloseProxy 下线代理。
type CloseProxy struct {
	Name string `json:"name"`
}

// StartWorkConn 服务端通知客户端: 有新访客连接某代理。
type StartWorkConn struct {
	ProxyName string `json:"proxy_name"`
}

// Ping / Pong 心跳(含时间戳,可测 RTT)。
type Ping struct {
	Ts int64 `json:"ts"`
}

// Pong 心跳应答。
type Pong struct {
	Ts int64 `json:"ts"`
}

// StreamMeta 数据流首帧,声明该流属于哪个代理。
type StreamMeta struct {
	ProxyName string `json:"proxy_name"`
}

// ErrorMsg 服务端错误提示。
type ErrorMsg struct {
	Error string `json:"error"`
}

// WriteFrame 写一个帧: [1B type][4B 长度][payload]。
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	hdr := make([]byte, FrameHeaderSize)
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame 读一个完整帧,返回类型与 payload。
func ReadFrame(r io.Reader) (byte, []byte, error) {
	hdr := make([]byte, FrameHeaderSize)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	typ := hdr[0]
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrameSize {
		return 0, nil, fmt.Errorf("frame too large: %d bytes", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return typ, payload, nil
}

// WriteMessage 序列化消息并写帧。
func WriteMessage(w io.Writer, typ byte, msg any) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return WriteFrame(w, typ, payload)
}
