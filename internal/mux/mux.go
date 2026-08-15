// Package mux 封装 smux 多路复用参数与双向透传。
package mux

import (
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
// 避免半开连接悬挂。onA2B / onB2A 在数据流动时按块增量回调
// (不是连接结束时一次性回调),便于实时统计流量。
func Join(a, b net.Conn, onA2B, onB2A func(n int64)) {
	var wg sync.WaitGroup
	wg.Add(2)
	closeBoth := func() { _ = a.Close(); _ = b.Close() }
	copyLoop := func(dst, src net.Conn, cb func(int64)) {
		buf := make([]byte, 32*1024)
		for {
			nr, er := src.Read(buf)
			if nr > 0 {
				nw, ew := dst.Write(buf[:nr])
				if nw > 0 && cb != nil {
					cb(int64(nw))
				}
				if ew != nil || nr != nw {
					break
				}
			}
			if er != nil {
				break
			}
		}
	}
	go func() {
		defer wg.Done()
		copyLoop(b, a, onA2B) // a→b
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		copyLoop(a, b, onB2A) // b→a
		closeBoth()
	}()
	wg.Wait()
}
