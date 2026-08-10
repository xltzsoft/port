package server

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"
)

// vhostConn 包装已带缓冲读取的连接,并把解析时消费过的请求头先重放出去,
// 保证 io.Copy 不丢任何字节。
type vhostConn struct {
	net.Conn
	r    *bufio.Reader
	head []byte // 解析 Host 时消费掉的请求头原文(含结尾空行)
}

func (c *vhostConn) Read(p []byte) (int, error) {
	if len(c.head) > 0 {
		n := copy(p, c.head)
		c.head = c.head[n:]
		return n, nil
	}
	return c.r.Read(p)
}

// handleVhostConn 处理 vhost 连接:读请求头提取 Host → 查路由表 → 转交对应代理。
// 首版按 TCP 透传处理 HTTP,不破坏 WebSocket/长连接。
func (s *Server) handleVhostConn(conn net.Conn) {
	br := bufio.NewReaderSize(conn, 64*1024)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	var head []byte
	line, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return
	}
	head = append(head, line...)
	fields := strings.Fields(line)
	if len(fields) < 2 {
		s.vhostReply(conn, 400, "Bad Request")
		return
	}
	var host string
	for {
		hl, err := br.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			return
		}
		head = append(head, hl...)
		if hl == "\r\n" || hl == "\n" {
			break // 请求头结束
		}
		if len(head) > 16*1024 {
			_ = conn.Close()
			return
		}
		if strings.HasPrefix(strings.ToLower(hl), "host:") {
			host = strings.TrimSpace(hl[len("host:"):])
		}
	}
	if host == "" {
		s.vhostReply(conn, 400, "Bad Request")
		return
	}
	h := strings.ToLower(host)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i] // 去掉端口
	}
	p := s.lookupVhost(h)
	if p == nil {
		s.log.Warn("vhost 未匹配", "host", h, "remote", conn.RemoteAddr().String())
		s.vhostReply(conn, 404, "Not Found")
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	// 转交后由 join 负责关闭连接
	if !s.deliverVisitor(p, &vhostConn{Conn: conn, r: br, head: head}) {
		_ = conn.Close()
	}
}

// vhostReply 返回简单的 HTTP 错误响应。
func (s *Server) vhostReply(conn net.Conn, code int, text string) {
	body := fmt.Sprintf("%d %s", code, text)
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", code, text, len(body), body)
	_ = conn.Close()
}

// normalizeHost 小写化、去空白与尾部点号。
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	return strings.TrimSuffix(h, ".")
}
