package watcher

import (
	"context"
	"filesync/config"
	"filesync/ftp"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/panjf2000/ants/v2"
)

// 文件监控器结构体
type FileWatcher struct {
	config     *config.Config
	uploader   *ftp.Uploader
	watcher    *fsnotify.Watcher
	lastFile   string
	lastUpload time.Time
	mu         sync.RWMutex
	pool       *ants.Pool
	retryQueue []string
	retryMu    sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

// 创建新的文件监控器
func NewFileWatcher(config *config.Config) (*FileWatcher, error) {
	// 创建工作池
	pool, err := ants.NewPool(10, ants.WithPreAlloc(true))
	if err != nil {
		return nil, fmt.Errorf("创建工作池失败: %w", err)
	}

	// 创建文件监视器
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		pool.Release()
		return nil, fmt.Errorf("创建文件监视器失败: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	w := &FileWatcher{
		config:     config,
		uploader:   ftp.NewUploader(config),
		watcher:    watcher,
		pool:       pool,
		retryQueue: make([]string, 0),
		ctx:        ctx,
		cancel:     cancel,
	}

	// 启动重试处理器
	w.wg.Add(1)
	go w.retryFailedUploads()

	return w, nil
}

// 启动文件监控
func (w *FileWatcher) Start() error {
	// 确保监控目录存在
	if _, err := os.Stat(w.config.Directories.LocalWatch); os.IsNotExist(err) {
		return fmt.Errorf("监控目录不存在: %s", w.config.Directories.LocalWatch)
	}

	// 启动事件处理
	w.wg.Add(1)
	go w.handleEvents()

	// 递归添加目录监控
	if err := w.addRecursiveWatch(w.config.Directories.LocalWatch); err != nil {
		return fmt.Errorf("添加目录监控失败: %w", err)
	}

	slog.Info("开始监听目录", "目录", w.config.Directories.LocalWatch)
	return nil
}

// 停止文件监控
func (w *FileWatcher) Stop() {
	// 发送停止信号
	w.cancel()

	// 等待所有goroutine完成
	w.wg.Wait()

	// 清理资源
	w.pool.Release()
	w.watcher.Close()

	slog.Info("文件监控器已停止")
}

// 处理文件事件
func (w *FileWatcher) handleEvents() {
	defer w.wg.Done()

	for {
		select {
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			w.processFileEvent(event)
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			slog.Error("文件监听错误", "error", err)
		case <-w.ctx.Done():
			slog.Info("事件处理器收到停止信号")
			return
		}
	}
}

// 处理Windows长路径
func cleanWindowsPath(path string) string {
	if runtime.GOOS == "windows" && strings.HasPrefix(path, "\\\\?\\") {
		return path[4:]
	}
	return path
}

// 处理文件事件
func (w *FileWatcher) processFileEvent(event fsnotify.Event) {
	// 记录文件事件
	eventType := getEventType(event)
	filePath := cleanWindowsPath(event.Name)
	fileInfo := getFileInfo(event.Name)

	slog.Info("文件变动",
		"类型", eventType,
		"路径", filePath,
		"对象", fileInfo)

	// 检查文件是否存在
	info, err := os.Stat(filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("获取文件信息失败", "error", err)
		}
		return
	}

	// 处理新建目录
	if info.IsDir() && event.Has(fsnotify.Create) {
		if err := addDirectoryWatch(w.watcher, filePath); err != nil {
			slog.Error("添加新目录监控失败", "目录", filePath, "error", err)
		}
		return
	}

	// 处理文件写入事件
	if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
		w.handleWriteEvent(filePath)
	}
}

// 处理写入事件
func (w *FileWatcher) handleWriteEvent(filePath string) {
	// 防止短时间内重复处理同一文件
	w.mu.RLock()
	if filePath == w.lastFile && time.Since(w.lastUpload) < 500*time.Millisecond {
		w.mu.RUnlock()
		return
	}
	w.mu.RUnlock()

	// 检查是否为目录
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("获取文件信息失败", "error", err)
		}
		return
	}

	// 如果是目录，则跳过上传
	if fileInfo.IsDir() {
		slog.Debug("跳过目录上传", "路径", filePath)
		return
	}

	// 尝试提交任务到工作池
	err = w.pool.Submit(func() {
		// 等待文件写入完成
		if !w.waitForFileStable(filePath) {
			return
		}

		if err := w.uploader.UploadFile(filePath); err != nil {
			slog.Error("上传文件失败，加入重试队列", "文件", filePath, "error", err)
			w.addToRetryQueue(filePath)
		} else {
			w.updateLastFile(filePath)
			slog.Info("文件上传成功", "文件", filePath)
		}
	})

	if err != nil {
		slog.Error("提交上传任务失败，加入重试队列", "error", err)
		w.addToRetryQueue(filePath)
	}
}

// 等待文件稳定 (不再被写入)
func (w *FileWatcher) waitForFileStable(filePath string) bool {
	const maxWaitTime = 10 * time.Second
	const checkInterval = 100 * time.Millisecond
	const stableChecks = 3

	stableCount := 0
	lastSize := int64(-1)
	startTime := time.Now()

	for time.Since(startTime) < maxWaitTime {
		// 检查是否已收到停止信号
		select {
		case <-w.ctx.Done():
			return false
		default:
			// 继续处理
		}

		// 获取文件信息
		info, err := os.Stat(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				slog.Warn("等待文件稳定过程中文件已不存在", "文件", filePath)
				return false
			}
			slog.Error("获取文件信息失败", "文件", filePath, "error", err)
			return false
		}

		currentSize := info.Size()

		// 检查文件大小是否变化
		if currentSize == lastSize {
			stableCount++
			// 连续多次检测到大小不变，认为文件已稳定
			if stableCount >= stableChecks {
				slog.Debug("文件已稳定", "文件", filePath, "大小", currentSize)
				return true
			}
		} else {
			stableCount = 0
			lastSize = currentSize
		}

		time.Sleep(checkInterval)
	}

	slog.Warn("等待文件稳定超时", "文件", filePath)
	return true // 超时后仍然尝试上传
}

// 更新最后处理的文件信息
func (w *FileWatcher) updateLastFile(filePath string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastFile = filePath
	w.lastUpload = time.Now()
}

// 添加到重试队列
func (w *FileWatcher) addToRetryQueue(filePath string) {
	w.retryMu.Lock()
	defer w.retryMu.Unlock()
	w.retryQueue = append(w.retryQueue, filePath)
}

// 处理重试上传逻辑
func (w *FileWatcher) retryFailedUploads() {
	defer w.wg.Done()

	duration, err := time.ParseDuration(w.config.Upload.RetryQueueDelay)
	if err != nil {
		slog.Error("时间解析错误", "error", err)
		return
	}
	ticker := time.NewTicker(duration)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.processRetryQueue()
		case <-w.ctx.Done():
			slog.Info("重试上传处理器收到停止信号")
			return
		}
	}
}

// 处理重试队列
func (w *FileWatcher) processRetryQueue() {
	w.retryMu.Lock()

	if len(w.retryQueue) == 0 {
		w.retryMu.Unlock()
		return
	}

	// 拷贝当前重试队列并清空
	failedFiles := make([]string, len(w.retryQueue))
	copy(failedFiles, w.retryQueue)
	w.retryQueue = make([]string, 0, cap(w.retryQueue))

	w.retryMu.Unlock()

	slog.Info("尝试重新上传失败的文件", "数量", len(failedFiles))

	// 批量上传处理
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 5) // 限制并发数

	for _, filePath := range failedFiles {
		// 检查是否已收到停止信号
		select {
		case <-w.ctx.Done():
			return
		default:
			// 继续处理
		}

		// 检查文件是否存在及类型
		fileInfo, err := os.Stat(filePath)
		if err != nil {
			if os.IsNotExist(err) {
				slog.Warn("文件已不存在，跳过重试", "文件", filePath)
				continue
			}
			slog.Error("获取文件信息失败", "文件", filePath, "error", err)
			continue
		}

		// 如果是目录，跳过上传
		if fileInfo.IsDir() {
			slog.Debug("跳过目录上传", "路径", filePath)
			continue
		}

		wg.Add(1)
		semaphore <- struct{}{} // 获取令牌

		go func(path string) {
			defer func() {
				<-semaphore // 释放令牌
				wg.Done()
			}()

			if err := w.uploader.UploadFile(path); err != nil {
				slog.Error("重试上传失败，重新加入队列", "文件", path, "error", err)
				w.addToRetryQueue(path)
			} else {
				slog.Info("文件重新上传成功", "文件", path)
			}
		}(filePath)
	}

	wg.Wait()
}

// 递归添加监控
func (w *FileWatcher) addRecursiveWatch(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			slog.Error("遍历目录错误", "error", err)
			return nil // 继续遍历其他文件
		}

		if info.IsDir() {
			if err := addDirectoryWatch(w.watcher, path); err != nil {
				slog.Error("无法监听目录", "目录", path, "error", err)
				// 继续遍历，不要中断整个过程
			}
		}
		return nil
	})
}

// 获取事件类型
func getEventType(event fsnotify.Event) string {
	switch {
	case event.Has(fsnotify.Create):
		return "创建"
	case event.Has(fsnotify.Write):
		return "写入"
	case event.Has(fsnotify.Remove):
		return "删除"
	case event.Has(fsnotify.Rename):
		return "重命名"
	case event.Has(fsnotify.Chmod):
		return "权限变更"
	default:
		return "未知"
	}
}

// 获取文件信息
func getFileInfo(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "文件不存在"
		}
		return fmt.Sprintf("无法获取信息: %v", err)
	}
	if info.IsDir() {
		return "目录"
	}
	return fmt.Sprintf("文件 (大小: %d 字节)", info.Size())
}

// 跨平台目录监控
func addDirectoryWatch(watcher *fsnotify.Watcher, dir string) error {
	if runtime.GOOS == "windows" {
		// Windows需要特殊处理长路径
		dir = filepath.Clean(dir)
		if !strings.HasPrefix(dir, "\\\\?\\") {
			dir = "\\\\?\\" + dir
		}
	}
	return watcher.Add(dir)
}
