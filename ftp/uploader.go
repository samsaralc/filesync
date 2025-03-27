package ftp

import (
	"bytes"
	c "filesync/config"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"

	"github.com/jlaffaye/ftp"
)

// 文件上传器结构体
type Uploader struct {
	config *c.Config
	mu     sync.Mutex
	pool   *Pool
}

// 创建新的文件上传器
func NewUploader(config *c.Config) *Uploader {
	return &Uploader{
		config: config,
		pool:   NewPool(config, 5), // 设置连接池大小为5
	}
}

// 检查文件是否允许上传
func (u *Uploader) isFileAllowed(filePath string) bool {
	fileName := filepath.Base(filePath)
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		slog.Error("获取文件信息失败", "error", err)
		return false
	}

	// 文件大小检查
	if u.config.Upload.MaxFileSizeMB > 0 &&
		fileInfo.Size() > int64(u.config.Upload.MaxFileSizeMB*1024*1024) {
		slog.Warn("文件超过大小限制",
			"文件", filePath,
			"限制", u.config.Upload.MaxFileSizeMB,
			"大小", fileInfo.Size())
		return false
	}

	// 包含和排除模式检查
	if !matchesIncludePatterns(fileName, u.config.FileFilters.IncludePatterns) {
		slog.Warn("文件不符合包含模式", "文件", filePath)
		return false
	}

	if matchesExcludePatterns(fileName, u.config.FileFilters.ExcludePatterns) {
		slog.Warn("文件符合排除模式", "文件", filePath)
		return false
	}

	return true
}

// 匹配包含模式
func matchesIncludePatterns(fileName string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, fileName); matched {
			return true
		}
	}
	return false
}

// 匹配排除模式
func matchesExcludePatterns(fileName string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, fileName); matched {
			return true
		}
	}
	return false
}

// 确保远程目录存在
func (u *Uploader) ensureRemoteDirectories(conn *ftp.ServerConn, path string) error {
	// 如果是根目录，无需创建
	if path == "/" || path == "" {
		return nil
	}

	// 规范化路径
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	path = filepath.ToSlash(path)

	// 缓存已知存在的目录
	existingDirs := make(map[string]bool)

	// 将路径分割成目录组件
	parts := strings.Split(path, "/")
	currentPath := ""

	// 逐个创建目录
	for _, part := range parts {
		if part == "" {
			continue
		}

		// 构建当前路径
		if currentPath == "" {
			currentPath = part
		} else {
			currentPath = filepath.Join(currentPath, part)
		}
		currentPath = filepath.ToSlash(currentPath)

		// 检查目录是否已经确认存在
		if existingDirs[currentPath] {
			continue
		}

		// 尝试进入目录检查是否存在
		if err := conn.ChangeDir(currentPath); err == nil {
			// 目录存在，回到根目录
			existingDirs[currentPath] = true
			if err := conn.ChangeDir("/"); err != nil {
				slog.Warn("无法返回根目录", "error", err)
			}
			continue
		}

		// 尝试创建目录
		err := conn.MakeDir(currentPath)
		if err != nil {
			// 检查是否是"目录已存在"错误
			if strings.Contains(err.Error(), "550") &&
				(strings.Contains(err.Error(), "File exists") ||
					strings.Contains(err.Error(), "Directory already exists") ||
					strings.Contains(err.Error(), "Create directory operation failed")) {
				slog.Debug("远程目录已存在", "path", currentPath)
				existingDirs[currentPath] = true
				continue // 继续处理下一个目录
			}

			return fmt.Errorf("创建远程目录 '%s' 失败: %w", currentPath, err)
		}

		existingDirs[currentPath] = true
		slog.Debug("创建远程目录成功", "path", currentPath)
	}

	return nil
}

// 转换编码（GBK/GB2312 -> UTF-8）
func convertToUTF8(str string) string {
	// 如果字符串已经是有效的UTF-8，则不需要转换
	if utf8.ValidString(str) {
		return str
	}

	// 尝试从GBK转换到UTF-8
	reader := transform.NewReader(bytes.NewReader([]byte(str)), simplifiedchinese.GBK.NewDecoder())
	data, err := io.ReadAll(reader)
	if err != nil {
		// 尝试从GB2312转换
		reader = transform.NewReader(bytes.NewReader([]byte(str)), simplifiedchinese.GB18030.NewDecoder())
		data, err = io.ReadAll(reader)
		if err != nil {
			slog.Warn("编码转换失败", "error", err)
			return str // 转换失败则返回原字符串
		}
	}

	return string(data)
}

// 上传文件
func (u *Uploader) UploadFile(filePath string) error {
	u.mu.Lock() // 防止同时多次上传同一文件
	defer u.mu.Unlock()

	// 检查文件是否存在及类型
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("文件不存在: %s", filePath)
		}
		return fmt.Errorf("获取文件信息失败: %w", err)
	}

	// 如果是目录，跳过上传但不算错误
	if fileInfo.IsDir() {
		slog.Debug("跳过目录上传", "路径", filePath)
		return nil
	}

	if !u.isFileAllowed(filePath) {
		return nil // 不符合上传条件，但不算错误
	}

	var lastErr error
	for attempt := 0; attempt <= u.config.Upload.RetryAttempts; attempt++ {
		// 检查文件是否仍然存在
		if _, err = os.Stat(filePath); os.IsNotExist(err) {
			slog.Error("文件不存在", "file", filePath)
			return fmt.Errorf("文件不存在: %s", filePath)
		}

		// 尝试上传文件
		if err = u.tryUpload(filePath); err != nil {
			lastErr = err
			slog.Warn("上传尝试失败",
				"尝试", attempt,
				"总尝试次数", u.config.Upload.RetryAttempts,
				"error", err)

			// 最后一次尝试失败，直接返回错误
			if attempt == u.config.Upload.RetryAttempts {
				return lastErr
			}

			// 等待指定时间后重试
			time.Sleep(time.Duration(u.config.Upload.RetryDelaySeconds) * time.Second)
			continue
		}

		// 上传成功，返回nil
		return nil
	}

	return lastErr // 理论上不会执行到这里
}

// 尝试上传单个文件
func (u *Uploader) tryUpload(filePath string) error {
	// 从连接池获取连接
	conn, err := u.pool.Get()
	if err != nil {
		return fmt.Errorf("获取FTP连接失败: %w", err)
	}

	// 使用defer确保连接归还到连接池
	defer u.pool.Put(conn)

	// 打开文件
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	// 计算相对路径
	relPath, err := filepath.Rel(u.config.Directories.LocalWatch, filePath)
	if err != nil {
		return fmt.Errorf("计算相对路径失败: %w", err)
	}

	// 处理目录路径
	dirPath := filepath.Dir(relPath)
	dirPath = filepath.ToSlash(dirPath)
	// 转换目录路径编码
	dirPath = convertToUTF8(dirPath)
	remotePath := filepath.Join(u.config.Directories.RemoteDestination, dirPath)
	remotePath = filepath.ToSlash(remotePath)

	// 创建远程目录结构
	if err := u.ensureRemoteDirectories(conn, remotePath); err != nil {
		return fmt.Errorf("创建远程目录失败: %w", err)
	}

	// 处理文件名
	fileName := filepath.Base(filePath)
	// 转换文件名编码
	fileName = convertToUTF8(fileName)
	fileName = filepath.ToSlash(fileName)
	remoteFilePath := filepath.Join(remotePath, fileName)
	remoteFilePath = filepath.ToSlash(remoteFilePath)

	slog.Debug("开始上传文件",
		"本地文件", filePath,
		"远程文件", remoteFilePath)

	// 上传文件
	if err := conn.Stor(remoteFilePath, file); err != nil {
		return fmt.Errorf("文件传输失败: %w", err)
	}

	slog.Info("上传成功",
		"本地文件", filePath,
		"远程文件", remoteFilePath)

	return nil
}
