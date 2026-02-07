// Package transport 提供传输层抽象，支持 WebSocket 和未来的 QUIC
package transport

import "io"

// Transport 传输层接口
type Transport interface {
	// Listen 监听指定地址
	Listen(addr string) error
	// Accept 接受新连接
	Accept() (Conn, error)
	// Close 关闭传输层
	Close() error
}

// Conn 连接接口
type Conn interface {
	io.ReadWriteCloser
	// RemoteAddr 返回远程地址
	RemoteAddr() string
	// SetReadDeadline 设置读超时
	SetReadDeadline(t int64) error
	// SetWriteDeadline 设置写超时
	SetWriteDeadline(t int64) error
}
