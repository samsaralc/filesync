package config

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/natefinch/lumberjack"
	"gopkg.in/yaml.v3"
)

var logger *slog.Logger

// 自定义日志处理器
type customHandler struct {
	writer io.Writer
	level  slog.Level
}

func (h *customHandler) Handle(ctx context.Context, r slog.Record) error {
	// 格式化时间
	timeStr := r.Time.Format(time.DateTime)

	// 构建日志消息
	msg := fmt.Sprintf("[%s] [%s] %s", timeStr, r.Level, r.Message)

	// 添加属性
	r.Attrs(func(a slog.Attr) bool {
		msg += fmt.Sprintf(" %s=%v", a.Key, a.Value.Any())
		return true
	})

	// 添加换行符
	msg += "\n"

	// 写入日志
	_, err := h.writer.Write([]byte(msg))
	return err
}

// 获取日志级别的中文描述
func getLevelString(level slog.Level) string {
	switch level {
	case slog.LevelDebug:
		return "调试"
	case slog.LevelInfo:
		return "信息"
	case slog.LevelWarn:
		return "警告"
	case slog.LevelError:
		return "错误"
	default:
		return "未知"
	}
}

func (h *customHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *customHandler) WithGroup(name string) slog.Handler {
	return h
}

func (h *customHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level
}

// 配置结构体
type Config struct {
	Server struct {
		Host     string `yaml:"host"`
		Port     string `yaml:"port"`
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"server"`

	Directories struct {
		LocalWatch        string `yaml:"local_watch"`
		RemoteDestination string `yaml:"remote_destination"`
	} `yaml:"directories"`

	Logging struct {
		Level      string `yaml:"level"`
		File       string `yaml:"file"`
		MaxSize    int    `yaml:"max_size_mb"`  // 单个日志文件最大大小（MB）
		MaxBackups int    `yaml:"max_backups"`  // 保留的旧日志文件数量
		MaxAge     int    `yaml:"max_age_days"` // 日志文件保留天数
		Compress   bool   `yaml:"compress"`     // 是否压缩旧日志
	} `yaml:"logging"`

	Upload struct {
		RetryAttempts     int    `yaml:"retry_attempts"`
		RetryDelaySeconds int    `yaml:"retry_delay_seconds"`
		MaxFileSizeMB     int    `yaml:"max_file_size_mb"`
		RetryQueueDelay   string `yaml:"retry_queue_delay"`
	} `yaml:"upload"`

	FileFilters struct {
		IncludePatterns []string `yaml:"include_patterns"`
		ExcludePatterns []string `yaml:"exclude_patterns"`
	} `yaml:"file_filters"`
}

// 加载配置文件
func LoadConfig() (*Config, error) {
	// 尝试从多个位置读取配置文件
	configPaths := []string{
		"config.yaml",
		"config.yml",
		"/etc/file-uploader/config.yaml",
		"~/.config/file-uploader/config.yaml",
	}

	var config Config
	var loadedFromPath string

	for _, path := range configPaths {
		// 展开可能的用户主目录
		expandedPath := os.ExpandEnv(path)

		configData, err := os.ReadFile(expandedPath)
		if err == nil {
			if err := yaml.Unmarshal(configData, &config); err != nil {
				return nil, fmt.Errorf("解析配置文件 %s 失败: %w", expandedPath, err)
			}
			loadedFromPath = expandedPath
			break
		}
	}

	if loadedFromPath == "" {
		return nil, fmt.Errorf("未找到配置文件，请在以下位置之一创建配置文件: %v", configPaths)
	}

	// 验证配置
	if err := validateConfig(&config); err != nil {
		return nil, fmt.Errorf("配置验证失败: %w", err)
	}

	fmt.Printf("已从 %s 加载配置\n", loadedFromPath)
	return &config, nil
}

// 验证配置文件
func validateConfig(config *Config) error {
	// 验证服务器配置
	if config.Server.Host == "" {
		return fmt.Errorf("服务器主机不能为空")
	}
	if config.Server.Port == "" {
		return fmt.Errorf("服务器端口不能为空")
	}
	if config.Server.Username == "" {
		return fmt.Errorf("服务器用户名不能为空")
	}

	// 验证目录配置
	if config.Directories.LocalWatch == "" {
		return fmt.Errorf("本地监控目录不能为空")
	}
	if config.Directories.RemoteDestination == "" {
		return fmt.Errorf("远程目标目录不能为空")
	}

	// 验证上传配置
	if config.Upload.RetryAttempts < 0 {
		return fmt.Errorf("重试次数不能为负数")
	}
	if config.Upload.RetryDelaySeconds < 0 {
		return fmt.Errorf("重试延迟不能为负数")
	}

	return nil
}

// 配置日志
func ConfigureLogging(config *Config) {
	// 创建日志目录
	logDir := filepath.Dir(config.Logging.File)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		slog.Error("创建日志目录失败", "error", err)
		return
	}

	// 创建不同级别的日志文件
	baseLogFile := config.Logging.File
	debugLogFile := strings.TrimSuffix(baseLogFile, filepath.Ext(baseLogFile)) + "_debug.log"
	infoLogFile := strings.TrimSuffix(baseLogFile, filepath.Ext(baseLogFile)) + "_info.log"
	warnLogFile := strings.TrimSuffix(baseLogFile, filepath.Ext(baseLogFile)) + "_warn.log"
	errorLogFile := strings.TrimSuffix(baseLogFile, filepath.Ext(baseLogFile)) + "_error.log"

	// 创建日志轮转器
	debugWriter := &lumberjack.Logger{
		Filename:   debugLogFile,
		MaxSize:    config.Logging.MaxSize,    // 每个文件最大尺寸，单位MB
		MaxBackups: config.Logging.MaxBackups, // 保留的旧文件数量
		MaxAge:     config.Logging.MaxAge,     // 保留天数
		Compress:   config.Logging.Compress,   // 是否压缩
	}

	infoWriter := &lumberjack.Logger{
		Filename:   infoLogFile,
		MaxSize:    config.Logging.MaxSize,
		MaxBackups: config.Logging.MaxBackups,
		MaxAge:     config.Logging.MaxAge,
		Compress:   config.Logging.Compress,
	}

	warnWriter := &lumberjack.Logger{
		Filename:   warnLogFile,
		MaxSize:    config.Logging.MaxSize,
		MaxBackups: config.Logging.MaxBackups,
		MaxAge:     config.Logging.MaxAge,
		Compress:   config.Logging.Compress,
	}

	errorWriter := &lumberjack.Logger{
		Filename:   errorLogFile,
		MaxSize:    config.Logging.MaxSize,
		MaxBackups: config.Logging.MaxBackups,
		MaxAge:     config.Logging.MaxAge,
		Compress:   config.Logging.Compress,
	}

	// 创建自定义日志处理器
	debugHandler := &customHandler{
		writer: debugWriter,
		level:  slog.LevelDebug,
	}

	infoHandler := &customHandler{
		writer: infoWriter,
		level:  slog.LevelInfo,
	}

	warnHandler := &customHandler{
		writer: warnWriter,
		level:  slog.LevelWarn,
	}

	errorHandler := &customHandler{
		writer: errorWriter,
		level:  slog.LevelError,
	}

	consoleHandler := &customHandler{
		writer: os.Stdout,
		level:  slog.LevelDebug,
	}

	// 创建多处理器
	multiHandler := &MultiHandler{
		handlers: []slog.Handler{
			consoleHandler, // 控制台处理器放在最前面
			debugHandler,
			infoHandler,
			warnHandler,
			errorHandler,
		},
	}

	// 设置全局日志处理器
	slog.SetDefault(slog.New(multiHandler))
}

// MultiHandler 实现多处理器
type MultiHandler struct {
	handlers []slog.Handler
}

func (h *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	// 首先处理控制台输出
	if err := h.handlers[0].Handle(ctx, r); err != nil {
		return err
	}

	// 然后根据日志级别写入到对应的文件
	switch r.Level {
	case slog.LevelDebug:
		return h.handlers[1].Handle(ctx, r) // debugHandler
	case slog.LevelInfo:
		return h.handlers[2].Handle(ctx, r) // infoHandler
	case slog.LevelWarn:
		return h.handlers[3].Handle(ctx, r) // warnHandler
	case slog.LevelError:
		return h.handlers[4].Handle(ctx, r) // errorHandler
	default:
		return nil
	}
}

func (h *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		handlers[i] = handler.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: handlers}
}

func (h *MultiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		handlers[i] = handler.WithGroup(name)
	}
	return &MultiHandler{handlers: handlers}
}

func (h *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}
