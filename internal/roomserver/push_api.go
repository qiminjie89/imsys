package roomserver

import (
	"encoding/json"
	"net/http"

	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/logger"
	"github.com/qiminjie89/imsys/pkg/metrics"
	"go.uber.org/zap"
)

// runHTTPServer 运行 HTTP 推送服务
func (s *Server) runHTTPServer() {
	mux := http.NewServeMux()

	// 推送接口
	mux.HandleFunc("/api/v1/push/unicast", s.handleUnicast)
	mux.HandleFunc("/api/v1/push/multicast", s.handleMulticast)
	mux.HandleFunc("/api/v1/push/room_broadcast", s.handleRoomBroadcast)
	mux.HandleFunc("/api/v1/push/global_broadcast", s.handleGlobalBroadcast)

	// 房间管理
	mux.HandleFunc("/api/v1/room/close", s.handleRoomClose)

	server := &http.Server{
		Addr:    s.cfg.Server.HTTPAddr,
		Handler: mux,
	}

	logger.Info("starting http server",
		zap.String("addr", s.cfg.Server.HTTPAddr),
	)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("http server error", zap.Error(err))
	}
}

// UnicastRequest 单播请求
type UnicastRequest struct {
	RoomID  string          `json:"room_id"`
	UserID  string          `json:"user_id"`
	Message json.RawMessage `json:"message"`
}

// MulticastRequest 多播请求
type MulticastRequest struct {
	RoomID  string          `json:"room_id"`
	UserIDs []string        `json:"user_ids"`
	Message json.RawMessage `json:"message"`
}

// RoomBroadcastRequest 房间广播请求
type RoomBroadcastRequest struct {
	RoomID  string          `json:"room_id"`
	Message json.RawMessage `json:"message"`
}

// GlobalBroadcastRequest 全局广播请求
type GlobalBroadcastRequest struct {
	Message json.RawMessage `json:"message"`
}

// RoomCloseRequest 房间关闭请求
type RoomCloseRequest struct {
	RoomID string `json:"room_id"`
	Reason string `json:"reason"`
}

// handleUnicast 处理单播
func (s *Server) handleUnicast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 检查服务状态
	if s.GetStatus() == protocol.ServerStatusRecovering {
		http.Error(w, "service recovering", http.StatusServiceUnavailable)
		return
	}

	var req UnicastRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// 查找用户
	user := s.GetUser(req.UserID)
	if user == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	// 发送到 Gateway
	gw := s.GetGateway(user.GatewayID)
	if gw == nil {
		http.Error(w, "gateway not found", http.StatusNotFound)
		return
	}

	msg := &protocol.UnicastMessage{
		UserID:  req.UserID,
		Payload: req.Message,
	}
	gw.SendUnicast(msg)

	metrics.RoomserverBroadcasts.WithLabelValues("unicast").Inc()
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleMulticast 处理多播
func (s *Server) handleMulticast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.GetStatus() == protocol.ServerStatusRecovering {
		http.Error(w, "service recovering", http.StatusServiceUnavailable)
		return
	}

	var req MulticastRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// 按 gateway_id 分组
	gatewayUsers := make(map[string][]string)
	for _, userID := range req.UserIDs {
		user := s.GetUser(userID)
		if user != nil {
			gatewayUsers[user.GatewayID] = append(gatewayUsers[user.GatewayID], userID)
		}
	}

	// 发送到各个 Gateway
	for gwID, userIDs := range gatewayUsers {
		gw := s.GetGateway(gwID)
		if gw == nil {
			continue
		}

		msg := &protocol.MulticastMessage{
			RoomID:  req.RoomID,
			UserIDs: userIDs,
			Payload: req.Message,
		}
		gw.SendMulticast(msg)
	}

	metrics.RoomserverBroadcasts.WithLabelValues("multicast").Inc()
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleRoomBroadcast 处理房间广播
func (s *Server) handleRoomBroadcast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.GetStatus() == protocol.ServerStatusRecovering {
		http.Error(w, "service recovering", http.StatusServiceUnavailable)
		return
	}

	var req RoomBroadcastRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// 获取房间
	room := s.GetRoom(req.RoomID)
	if room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// 提交广播请求到房间队列
	if !room.Broadcast(req.Message) {
		http.Error(w, "room queue full", http.StatusServiceUnavailable)
		return
	}

	metrics.RoomserverBroadcasts.WithLabelValues("room").Inc()
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleGlobalBroadcast 处理全局广播
func (s *Server) handleGlobalBroadcast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.GetStatus() == protocol.ServerStatusRecovering {
		http.Error(w, "service recovering", http.StatusServiceUnavailable)
		return
	}

	var req GlobalBroadcastRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// 遍历所有房间广播
	s.roomsMu.RLock()
	rooms := make([]*Room, 0, len(s.rooms))
	for _, room := range s.rooms {
		rooms = append(rooms, room)
	}
	s.roomsMu.RUnlock()

	for _, room := range rooms {
		room.Broadcast(req.Message)
	}

	metrics.RoomserverBroadcasts.WithLabelValues("global").Inc()
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleRoomClose 处理房间关闭
func (s *Server) handleRoomClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RoomCloseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	room := s.GetRoom(req.RoomID)
	if room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// 获取房间的所有 Gateway
	gatewayIDs := room.GetGatewayIDs()

	// 通知所有 Gateway
	closeMsg := &protocol.RoomClose{
		RoomID: req.RoomID,
		Reason: req.Reason,
	}

	s.gatewaysMu.RLock()
	for _, gwID := range gatewayIDs {
		if gw, ok := s.gateways[gwID]; ok {
			gw.SendRoomClose(closeMsg)
		}
	}
	s.gatewaysMu.RUnlock()

	// 清理房间
	s.RemoveRoom(req.RoomID)

	logger.Info("room closed",
		zap.String("room_id", req.RoomID),
		zap.String("reason", req.Reason),
	)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
