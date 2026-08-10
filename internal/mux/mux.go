// Package mux 封装 smux 多路复用参数与双向透传。
package mux

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

// Config 返回统一的多路复用配置。
// keepalive 是第二道探活保险,应用层心跳(15s/90s)负责主探活。
func Config() *smux.Config {
	cfg := smux.DefaultConfig()
	cfg.KeepAliveInterval = 10 * time.Second
	cfg.KeepAliveTimeout = 30 * time.Second
	return cfg
}

// Join 双向透传 a↔b。任一端结束(EOF/错误)即关闭两端,
// 避免半开连接悬挂。onA2B / onB2A 分别回调 A→B、B→A 方向的总字节数。
func Join(a, b net.Conn, onA2B, onB2A func(n int64)) {
	var wg sync.WaitGroup
	wg.Add(2)
	closeBoth := func() { _ = a.Close(); _ = b.Close() }
	go func() {
		defer wg.Done()
		n, _ := io.Copy(b, a) // a→b
		if onA2B != nil {
			onA2B(n)
		}
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(a, b) // b→a
		if onB2A != nil {
			onB2A(n)
		}
		closeBoth()
	}()
	wg.Wait()
}
