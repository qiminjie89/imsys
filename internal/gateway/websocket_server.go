package gateway

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/logger"
	"github.com/qiminjie89/imsys/pkg/metrics"
	"go.uber.org/zap"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true // 生产环境应检查 Origin
	},
}

// runWebSocketServer 运行 WebSocket 服务
func (s *Server) runWebSocketServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)

	server := &http.Server{
		Addr:    s.cfg.Server.Addr,
		Handler: mux,
	}

	logger.Info("starting websocket server",
		zap.String("addr", s.cfg.Server.Addr),
	)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("websocket server error", zap.Error(err))
	}
}

// handleWebSocket 处理 WebSocket 连接
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Warn("websocket upgrade failed", zap.Error(err))
		return
	}

	// 等待认证消息
	userID, err := s.authenticate(ws)
	if err != nil {
		logger.Warn("authentication failed", zap.Error(err))
		ws.Close()
		return
	}

	connID := uuid.New().String()

	conn := NewConnection(userID, connID, ws, &s.cfg.Connection, s)
	s.AddConnection(conn)

	metrics.GatewayConnections.Inc()

	logger.Info("new connection",
		zap.String("user_id", userID),
		zap.String("conn_id", connID),
		zap.String("remote_addr", r.RemoteAddr),
	)

	// 启动读写循环
	conn.Start()
}

// authenticate 认证连接
func (s *Server) authenticate(ws *websocket.Conn) (string, error) {
	// 读取第一条消息作为认证请求
	_, data, err := ws.ReadMessage()
	if err != nil {
		return "", err
	}

	// TODO: 解析认证消息，验证 JWT
	// 这里简化处理，从消息中提取 user_id

	var authReq struct {
		Token  string `msgpack:"token"`
		UserID string `msgpack:"user_id"`
	}

	// 尝试解析
	if err := msgpackDecode(data, &authReq); err != nil {
		return "", err
	}

	// TODO: 验证 token
	// jwtToken, err := jwt.Parse(authReq.Token, ...)

	// 发送认证成功响应
	s.sendAuthResp(ws, true, authReq.UserID)

	return authReq.UserID, nil
}

// sendAuthResp 发送认证响应
func (s *Server) sendAuthResp(ws *websocket.Conn, success bool, userID string) {
	// TODO: 实现
}

// msgpackDecode 简单的 msgpack 解码（跳过帧头）
func msgpackDecode(data []byte, v interface{}) error {
	// 跳过帧头（16字节），直接解码 payload
	if len(data) < 16 {
		return nil
	}
	return protocol.Decode(data[16:], v)
}
