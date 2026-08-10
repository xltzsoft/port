// port-client: 内网穿透客户端。
// 主动连接服务端控制端口(17200),注册代理;SIGHUP 触发配置热重载。
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"port/internal/client"
	"port/internal/config"
	"port/internal/logger"
)

// Version 构建时经 -ldflags "-X main.Version=..." 注入。
var Version = "dev"

func main() {
	cfgPath := flag.String("c", "configs/client.yaml", "配置文件路径")
	level := flag.String("log-level", "info", "日志级别: debug|info|warn|error")
	flag.Parse()

	log := logger.New(*level)
	log.Info("port-client", "version", Version)

	cfg, err := config.LoadClient(*cfgPath)
	if err != nil {
		log.Error("加载配置失败", "err", err)
		os.Exit(1)
	}
	cl, err := client.New(cfg, log)
	if err != nil {
		log.Error("初始化客户端失败", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 配置热重载: kill -HUP <pid>
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			cl.Reload(*cfgPath)
		}
	}()

	if err := cl.Run(ctx); err != nil {
		log.Error("客户端退出", "err", err)
		os.Exit(1)
	}
	log.Info("客户端已退出")
}
