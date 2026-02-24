package gateway

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/auth"
	"github.com/qiminjie89/imsys/pkg/logger"
	"github.com/qiminjie89/imsys/pkg/metrics"
	"go.uber.org/zap"
)

var (
	ErrAuthTimeout = errors.New("authentication timeout")
	ErrInvalidAuth = errors.New("invalid authentication")
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

	s.httpServer = &http.Server{
		Addr:    s.cfg.Server.Addr,
		Handler: mux,
	}

	logger.Info("starting websocket server",
		zap.String("addr", s.cfg.Server.Addr),
	)

	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

	// 设置握手超时
	ws.SetReadDeadline(time.Now().Add(s.cfg.WebSocket.HandshakeTimeout))

	// 等待认证消息
	userID, err := s.authenticate(ws)
	if err != nil {
		logger.Warn("authentication failed",
			zap.Error(err),
			zap.String("remote_addr", r.RemoteAddr),
		)
		s.sendAuthResp(ws, false, "", protocol.ErrCodeAuthFailed, err.Error())
		ws.Close()
		return
	}

	// 清除读超时
	ws.SetReadDeadline(time.Time{})

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
		return "", ErrAuthTimeout
	}

	// 解析认证帧
	frame, err := protocol.DecodeFrame(data)
	if err != nil {
		return "", ErrInvalidAuth
	}

	if frame.MsgType != protocol.MsgTypeAuth {
		return "", ErrInvalidAuth
	}

	// 解析认证 payload
	var authReq struct {
		Token  string `msgpack:"token"`
		UserID string `msgpack:"user_id"` // 兼容开发环境
	}

	if err := protocol.Decode(frame.Payload, &authReq); err != nil {
		return "", ErrInvalidAuth
	}

	// 验证 JWT Token
	var userID string

	if s.jwtValidator != nil {
		claims, err := s.jwtValidator.ValidateOrMock(authReq.Token, authReq.UserID)
		if err != nil {
			if errors.Is(err, auth.ErrTokenExpired) {
				s.sendAuthResp(ws, false, "", protocol.ErrCodeTokenExpired, "token expired")
				return "", err
			}
			return "", ErrInvalidAuth
		}
		userID = claims.UserID
	} else {
		// 未配置 JWT 验证器，使用请求中的 user_id（仅开发环境）
		userID = authReq.UserID
	}

	if userID == "" {
		return "", ErrInvalidAuth
	}

	// 发送认证成功响应
	s.sendAuthResp(ws, true, userID, protocol.ErrCodeSuccess, "")

	return userID, nil
}

// sendAuthResp 发送认证响应
func (s *Server) sendAuthResp(ws *websocket.Conn, success bool, userID string, code int, message string) {
	resp := struct {
		Success   bool   `msgpack:"success"`
		UserID    string `msgpack:"user_id,omitempty"`
		GatewayID string `msgpack:"gateway_id,omitempty"`
		Code      int    `msgpack:"code"`
		Message   string `msgpack:"message,omitempty"`
	}{
		Success:   success,
		UserID:    userID,
		GatewayID: s.cfg.Gateway.ID,
		Code:      code,
		Message:   message,
	}

	payload, err := protocol.Encode(resp)
	if err != nil {
		logger.Warn("encode auth response failed", zap.Error(err))
		return
	}

	frame := &protocol.Frame{
		MsgType: protocol.MsgTypeAuthResp,
		Seq:     0,
		Payload: payload,
	}

	data := protocol.EncodeFrame(frame)
	ws.WriteMessage(websocket.BinaryMessage, data)
}
