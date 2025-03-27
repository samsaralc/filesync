package ftp

import (
	c "filesync/config"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
)

// FTP连接池
type Pool struct {
	config      *c.Config
	connections chan *ftp.ServerConn
	maxSize     int
	mu          sync.Mutex
	closed      bool
}

// 创建新的FTP连接池
func NewPool(config *c.Config, maxSize int) *Pool {
	if maxSize <= 0 {
		maxSize = 5 // 默认连接池大小
	}

	return &Pool{
		config:      config,
		connections: make(chan *ftp.ServerConn, maxSize),
		maxSize:     maxSize,
		closed:      false,
	}
}

// 获取连接
func (p *Pool) Get() (*ftp.ServerConn, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("连接池已关闭")
	}
	p.mu.Unlock()

	select {
	case conn := <-p.connections:
		// 检查连接是否有效
		if err := conn.NoOp(); err != nil {
			slog.Debug("连接已失效，创建新连接", "error", err)
			conn.Quit()
			return p.createConnection()
		}
		slog.Debug("从连接池获取连接成功")
		return conn, nil
	default:
		slog.Debug("连接池为空，创建新连接")
		return p.createConnection()
	}
}

// 归还连接
func (p *Pool) Put(conn *ftp.ServerConn) {
	if conn == nil {
		return
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		conn.Quit()
		return
	}
	p.mu.Unlock()

	// 检查连接是否有效
	if err := conn.NoOp(); err != nil {
		slog.Debug("连接已失效，关闭连接", "error", err)
		conn.Quit()
		return
	}

	select {
	case p.connections <- conn:
		slog.Debug("连接已归还到连接池")
	default:
		slog.Debug("连接池已满，关闭连接")
		conn.Quit()
	}
}

// 创建新的FTP连接
func (p *Pool) createConnection() (*ftp.ServerConn, error) {
	slog.Debug("正在创建新的FTP连接",
		"host", p.config.Server.Host,
		"port", p.config.Server.Port)

	// 设置更合理的超时时间
	timeout := 15 * time.Second

	conn, err := ftp.Dial(
		p.config.Server.Host+":"+p.config.Server.Port,
		ftp.DialWithTimeout(timeout),
	)
	if err != nil {
		slog.Error("FTP连接失败", "error", err)
		return nil, fmt.Errorf("FTP连接失败: %w", err)
	}

	// 先进行登录
	if err = conn.Login(p.config.Server.Username, p.config.Server.Password); err != nil {
		slog.Error("FTP登录失败", "error", err)
		conn.Quit()
		return nil, fmt.Errorf("FTP登录失败: %w", err)
	}

	// 登录成功后设置传输类型
	if err := conn.Type(ftp.TransferTypeBinary); err != nil {
		slog.Error("设置传输类型失败", "error", err)
		conn.Quit()
		return nil, fmt.Errorf("设置传输类型失败: %w", err)
	}

	// 测试连接是否真正可用
	if err := conn.NoOp(); err != nil {
		slog.Error("FTP连接测试失败", "error", err)
		conn.Quit()
		return nil, fmt.Errorf("FTP连接测试失败: %w", err)
	}

	slog.Info("FTP连接创建成功")
	return conn, nil
}

// 关闭连接池
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	p.closed = true
	slog.Info("正在关闭FTP连接池")

	close(p.connections)
	for conn := range p.connections {
		conn.Quit()
	}

	slog.Info("FTP连接池已关闭")
}
