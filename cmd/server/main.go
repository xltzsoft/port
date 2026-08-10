// port-server: 内网穿透服务端。
// 控制入站 17200,出站端口池 20000-30000,vhost HTTP 17280,管理面板 17250。
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"port/internal/config"
	"port/internal/logger"
	"port/internal/server"
)

// Version 构建时经 -ldflags "-X main.Version=..." 注入。
var Version = "dev"

func main() {
	cfgPath := flag.String("c", "configs/server.yaml", "配置文件路径")
	level := flag.String("log-level", "info", "日志级别: debug|info|warn|error")
	flag.Parse()

	log := logger.New(*level)
	log.Info("port-server", "version", Version)

	cfg, err := config.LoadServer(*cfgPath)
	if err != nil {
		log.Error("加载配置失败", "err", err)
		os.Exit(1)
	}
	srv, err := server.New(cfg, log)
	if err != nil {
		log.Error("初始化服务端失败", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		log.Error("服务端退出", "err", err)
		os.Exit(1)
	}
	log.Info("服务端已退出")
}
