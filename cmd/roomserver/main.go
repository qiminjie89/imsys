package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/qiminjie89/imsys/internal/roomserver"
	"github.com/qiminjie89/imsys/pkg/config"
	"github.com/qiminjie89/imsys/pkg/logger"
	"go.uber.org/zap"
)

func main() {
	// 解析命令行参数
	configPath := flag.String("config", "configs/roomserver.yaml", "config file path")
	flag.Parse()

	// 加载配置
	cfg, err := config.LoadRoomserverConfig(*configPath)
	if err != nil {
		panic("load config failed: " + err.Error())
	}

	// 初始化日志
	if err := logger.Init(logger.Config{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
		Output: cfg.Log.Output,
	}); err != nil {
		panic("init logger failed: " + err.Error())
	}
	defer logger.Sync()

	logger.Info("starting roomserver",
		zap.String("config", *configPath),
	)

	// 创建并启动服务
	server := roomserver.NewServer(cfg)
	if err := server.Start(); err != nil {
		logger.Error("start server failed", zap.Error(err))
		os.Exit(1)
	}

	// 等待退出信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("received shutdown signal")
	server.Stop()
}
