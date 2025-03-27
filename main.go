package main

import (
	"filesync/config"
	"filesync/watcher"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// 加载配置
	cfg, err := config.LoadConfig()
	if err != nil {
		slog.Error("加载配置失败", "error", err)
		os.Exit(1)
	}

	// 配置日志
	config.ConfigureLogging(cfg)

	// 创建文件监控器
	fileWatcher, err := watcher.NewFileWatcher(cfg)
	if err != nil {
		slog.Error("创建文件监控器失败", "error", err)
		os.Exit(1)
	}

	// 启动文件监控
	if err := fileWatcher.Start(); err != nil {
		slog.Error("启动文件监控失败", "error", err)
		os.Exit(1)
	}

	slog.Info("文件监控服务已启动")

	// 等待中断信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	// 停止文件监控
	fileWatcher.Stop()
	slog.Info("文件监控服务已停止")
}
