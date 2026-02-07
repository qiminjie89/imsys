package transport

import (
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocketTransport WebSocket 传输层实现
type WebSocketTransport struct {
	addr     string
	upgrader websocket.Upgrader
	server   *http.Server
	connCh   chan *WebSocketConn
	doneCh   chan struct{}
}

// WebSocketConfig WebSocket 配置
type WebSocketConfig struct {
	ReadBufferSize   int
	WriteBufferSize  int
	HandshakeTimeout time.Duration
}

// NewWebSocketTransport 创建 WebSocket 传输层
func NewWebSocketTransport(cfg WebSocketConfig) *WebSocketTransport {
	return &WebSocketTransport{
		upgrader: websocket.Upgrader{
			ReadBufferSize:   cfg.ReadBufferSize,
			WriteBufferSize:  cfg.WriteBufferSize,
			HandshakeTimeout: cfg.HandshakeTimeout,
			CheckOrigin: func(r *http.Request) bool {
				return true // 生产环境应检查 Origin
			},
		},
		connCh: make(chan *WebSocketConn, 1000),
		doneCh: make(chan struct{}),
	}
}

// Listen 监听地址
func (t *WebSocketTransport) Listen(addr string) error {
	t.addr = addr

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", t.handleWebSocket)

	t.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go t.server.ListenAndServe()
	return nil
}

func (t *WebSocketTransport) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := t.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	wsConn := &WebSocketConn{
		conn:       conn,
		remoteAddr: r.RemoteAddr,
	}

	select {
	case t.connCh <- wsConn:
	case <-t.doneCh:
		conn.Close()
	}
}

// Accept 接受新连接
func (t *WebSocketTransport) Accept() (Conn, error) {
	select {
	case conn := <-t.connCh:
		return conn, nil
	case <-t.doneCh:
		return nil, http.ErrServerClosed
	}
}

// Close 关闭传输层
func (t *WebSocketTransport) Close() error {
	close(t.doneCh)
	if t.server != nil {
		return t.server.Close()
	}
	return nil
}

// WebSocketConn WebSocket 连接实现
type WebSocketConn struct {
	conn       *websocket.Conn
	remoteAddr string
}

// Read 读取数据
func (c *WebSocketConn) Read(p []byte) (int, error) {
	_, data, err := c.conn.ReadMessage()
	if err != nil {
		return 0, err
	}
	n := copy(p, data)
	return n, nil
}

// Write 写入数据
func (c *WebSocketConn) Write(p []byte) (int, error) {
	err := c.conn.WriteMessage(websocket.BinaryMessage, p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close 关闭连接
func (c *WebSocketConn) Close() error {
	return c.conn.Close()
}

// RemoteAddr 返回远程地址
func (c *WebSocketConn) RemoteAddr() string {
	return c.remoteAddr
}

// SetReadDeadline 设置读超时
func (c *WebSocketConn) SetReadDeadline(t int64) error {
	return c.conn.SetReadDeadline(time.Unix(0, t))
}

// SetWriteDeadline 设置写超时
func (c *WebSocketConn) SetWriteDeadline(t int64) error {
	return c.conn.SetWriteDeadline(time.Unix(0, t))
}

// WriteMessage 发送 WebSocket 消息（供 Gateway 直接使用）
func (c *WebSocketConn) WriteMessage(messageType int, data []byte) error {
	return c.conn.WriteMessage(messageType, data)
}

// Underlying 返回底层 websocket.Conn
func (c *WebSocketConn) Underlying() *websocket.Conn {
	return c.conn
}
