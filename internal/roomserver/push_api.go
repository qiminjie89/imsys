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

	// 推送接口（通过控制面发送，仅适用于小规模场景）
	// 注意：大规模推送建议通过 Kafka 数据面（Python → Kafka → Gateway）
	mux.HandleFunc("/api/v1/push/unicast", s.handleUnicastHTTP)
	mux.HandleFunc("/api/v1/push/multicast", s.handleMulticastHTTP)
	mux.HandleFunc("/api/v1/push/room_broadcast", s.handleRoomBroadcastHTTP)
	mux.HandleFunc("/api/v1/push/global_broadcast", s.handleGlobalBroadcastHTTP)

	// 房间管理
	mux.HandleFunc("/api/v1/room/close", s.handleRoomCloseHTTP)

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

// handleUnicastHTTP 处理单播
func (s *Server) handleUnicastHTTP(w http.ResponseWriter, r *http.Request) {
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

	// 查找用户会话
	session := s.sessionSvc.GetSession(req.UserID)
	if session == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	// 发送到 Gateway
	gw := s.GetGatewayByID(session.GatewayID)
	if gw == nil {
		http.Error(w, "gateway not found", http.StatusNotFound)
		return
	}

	msg := &protocol.UnicastMessage{
		UserID:  req.UserID,
		Payload: req.Message,
	}
	payload, _ := protocol.Encode(msg)
	s.sendToGateway(gw, protocol.InternalMsgUnicast, payload)

	metrics.RoomserverBroadcasts.WithLabelValues("unicast").Inc()
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleMulticastHTTP 处理多播
func (s *Server) handleMulticastHTTP(w http.ResponseWriter, r *http.Request) {
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
		session := s.sessionSvc.GetSession(userID)
		if session != nil {
			gatewayUsers[session.GatewayID] = append(gatewayUsers[session.GatewayID], userID)
		}
	}

	// 发送到各个 Gateway
	for gwID, userIDs := range gatewayUsers {
		gw := s.GetGatewayByID(gwID)
		if gw == nil {
			continue
		}

		msg := &protocol.MulticastMessage{
			RoomID:  req.RoomID,
			UserIDs: userIDs,
			Payload: req.Message,
		}
		payload, _ := protocol.Encode(msg)
		s.sendToGateway(gw, protocol.InternalMsgMulticast, payload)
	}

	metrics.RoomserverBroadcasts.WithLabelValues("multicast").Inc()
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleRoomBroadcastHTTP 处理房间广播
// 注意：大规模场景建议使用 Kafka 数据面（Python → Kafka → Gateway）
func (s *Server) handleRoomBroadcastHTTP(w http.ResponseWriter, r *http.Request) {
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
	room := s.roomSvc.GetRoom(req.RoomID)
	if room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// 获取房间涉及的所有 Gateway
	gatewayIDs := s.roomSvc.GetRoomGatewayIDs(req.RoomID)
	if len(gatewayIDs) == 0 {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "note": "no gateways"})
		return
	}

	// 发送广播到各个 Gateway
	msg := &protocol.BroadcastMessage{
		RoomID:  req.RoomID,
		Payload: req.Message,
	}
	payload, _ := protocol.Encode(msg)

	s.gatewaysMu.RLock()
	for _, gwID := range gatewayIDs {
		if gw, ok := s.gateways[gwID]; ok {
			s.sendToGateway(gw, protocol.InternalMsgBroadcast, payload)
		}
	}
	s.gatewaysMu.RUnlock()

	metrics.RoomserverBroadcasts.WithLabelValues("room").Inc()
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleGlobalBroadcastHTTP 处理全局广播
// 注意：大规模场景建议使用 Kafka 数据面（Python → Kafka → Gateway）
func (s *Server) handleGlobalBroadcastHTTP(w http.ResponseWriter, r *http.Request) {
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

	// 获取所有房间 ID
	roomIDs := s.roomSvc.GetAllRoomIDs()

	// 收集所有涉及的 Gateway
	allGatewayIDs := make(map[string]bool)
	for _, roomID := range roomIDs {
		for _, gwID := range s.roomSvc.GetRoomGatewayIDs(roomID) {
			allGatewayIDs[gwID] = true
		}
	}

	// 广播给所有 Gateway（平台广播）
	msg := &protocol.BroadcastMessage{
		Payload: req.Message,
	}
	payload, _ := protocol.Encode(msg)

	s.gatewaysMu.RLock()
	for gwID := range allGatewayIDs {
		if gw, ok := s.gateways[gwID]; ok {
			s.sendToGateway(gw, protocol.InternalMsgBroadcast, payload)
		}
	}
	s.gatewaysMu.RUnlock()

	metrics.RoomserverBroadcasts.WithLabelValues("global").Inc()
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleRoomCloseHTTP 处理房间关闭
func (s *Server) handleRoomCloseHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RoomCloseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	room := s.roomSvc.GetRoom(req.RoomID)
	if room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	// 获取房间的所有 Gateway
	gatewayIDs := s.roomSvc.GetRoomGatewayIDs(req.RoomID)

	// 通知所有 Gateway
	closeMsg := &protocol.RoomCloseNotify{
		RoomID: req.RoomID,
		Reason: req.Reason,
	}
	payload, _ := protocol.Encode(closeMsg)

	s.gatewaysMu.RLock()
	for _, gwID := range gatewayIDs {
		if gw, ok := s.gateways[gwID]; ok {
			s.sendToGateway(gw, protocol.InternalMsgRoomClose, payload)
		}
	}
	s.gatewaysMu.RUnlock()

	// 清理房间
	s.roomSvc.CloseRoom(req.RoomID)

	logger.Info("room closed",
		zap.String("room_id", req.RoomID),
		zap.String("reason", req.Reason),
	)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
