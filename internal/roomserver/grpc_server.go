package roomserver

import (
	"io"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	pb "github.com/qiminjie89/imsys/api/proto/gen"
	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/logger"
	"go.uber.org/zap"
)

// GatewayConnection 表示一个 Gateway 连接
type GatewayConnection struct {
	id       string
	addr     string
	stream   pb.RoomServerGateway_ChannelServer
	sendCh   chan *pb.Envelope
	mu       sync.RWMutex
	closed   bool
}

// runGRPCServer 运行 gRPC 服务（供 Gateway 连接）
func (s *Server) runGRPCServer() {
	addr := s.cfg.Server.GRPCAddr

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("failed to listen for gRPC", zap.Error(err))
		return
	}

	grpcServer := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     0,                 // 无空闲超时
			MaxConnectionAge:      0,                 // 无连接年龄限制
			MaxConnectionAgeGrace: 0,                 // 无优雅关闭期
			Time:                  10 * time.Second,  // ping 间隔
			Timeout:               3 * time.Second,   // ping 超时
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second, // 允许客户端最小 ping 间隔
			PermitWithoutStream: true,            // 允许无 stream 时 ping
		}),
	)

	pb.RegisterRoomServerGatewayServer(grpcServer, s)

	logger.Info("starting grpc server",
		zap.String("addr", addr),
	)

	go func() {
		<-s.ctx.Done()
		grpcServer.GracefulStop()
	}()

	if err := grpcServer.Serve(lis); err != nil {
		logger.Error("grpc server error", zap.Error(err))
	}
}

// Channel 实现双向流 RPC
func (s *Server) Channel(stream pb.RoomServerGateway_ChannelServer) error {
	conn := &GatewayConnection{
		stream: stream,
		sendCh: make(chan *pb.Envelope, 10000),
	}

	// 启动发送循环
	go s.gatewayWriteLoop(conn)

	// 接收循环
	for {
		envelope, err := stream.Recv()
		if err == io.EOF {
			logger.Info("gateway connection closed (EOF)")
			break
		}
		if err != nil {
			logger.Warn("gateway recv error", zap.Error(err))
			break
		}

		s.handleGatewayMessage(conn, envelope)
	}

	// 清理
	conn.mu.Lock()
	conn.closed = true
	close(conn.sendCh)
	conn.mu.Unlock()

	if conn.id != "" {
		s.removeGateway(conn.id)
	}

	return nil
}

// gatewayWriteLoop 发送循环
func (s *Server) gatewayWriteLoop(conn *GatewayConnection) {
	for envelope := range conn.sendCh {
		if err := conn.stream.Send(envelope); err != nil {
			logger.Warn("gateway send error", zap.Error(err))
			return
		}
	}
}

// handleGatewayMessage 处理来自 Gateway 的消息
func (s *Server) handleGatewayMessage(conn *GatewayConnection, envelope *pb.Envelope) {
	switch envelope.MsgType {
	case protocol.InternalMsgGatewayInfo:
		s.handleGatewayInfo(conn, envelope.Payload)
	case protocol.InternalMsgJoinRoom:
		s.handleJoinRoom(conn, envelope.Payload)
	case protocol.InternalMsgLeaveRoom:
		s.handleLeaveRoom(conn, envelope.Payload)
	case protocol.InternalMsgResume:
		s.handleResume(conn, envelope.Payload)
	case protocol.InternalMsgDisconnect:
		s.handleDisconnect(conn, envelope.Payload)
	case protocol.InternalMsgBatchReSync:
		s.handleBatchReSync(conn, envelope.Payload)
	case protocol.InternalMsgReSyncComplete:
		s.handleReSyncComplete(conn, envelope.Payload)
	default:
		logger.Warn("unknown message type from gateway",
			zap.Uint32("msg_type", envelope.MsgType),
		)
	}
}

// handleGatewayInfo 处理 Gateway 信息上报
func (s *Server) handleGatewayInfo(conn *GatewayConnection, payload []byte) {
	var info protocol.GatewayInfo
	if err := protocol.Decode(payload, &info); err != nil {
		logger.Warn("decode gateway info failed", zap.Error(err))
		return
	}

	conn.id = info.GatewayID
	conn.addr = info.Addr

	s.addGateway(conn)

	logger.Info("gateway registered",
		zap.String("gateway_id", info.GatewayID),
		zap.String("addr", info.Addr),
	)

	// 发送 Epoch 通知
	s.sendEpochNotify(conn)
}

// handleJoinRoom 处理 JoinRoom 请求
func (s *Server) handleJoinRoom(conn *GatewayConnection, payload []byte) {
	var req protocol.JoinRoomRequest
	if err := protocol.Decode(payload, &req); err != nil {
		logger.Warn("decode join room request failed", zap.Error(err))
		return
	}

	logger.Debug("join room request",
		zap.String("user_id", req.UserID),
		zap.String("room_id", req.RoomID),
		zap.String("gateway_id", req.GatewayID),
	)

	// 使用 Server 的 JoinRoom 方法
	err := s.JoinRoom(req.UserID, req.RoomID, req.GatewayID)

	success := err == nil
	code := protocol.ErrCodeSuccess
	message := ""
	if err != nil {
		code = protocol.ErrCodeInternalError
		message = err.Error()
	}

	// 发送 ACK
	s.sendJoinRoomAck(conn, req.UserID, req.RoomID, success, code, message)
}

// handleLeaveRoom 处理 LeaveRoom 请求
func (s *Server) handleLeaveRoom(conn *GatewayConnection, payload []byte) {
	var req protocol.LeaveRoomRequest
	if err := protocol.Decode(payload, &req); err != nil {
		logger.Warn("decode leave room request failed", zap.Error(err))
		return
	}

	logger.Debug("leave room request",
		zap.String("user_id", req.UserID),
		zap.String("room_id", req.RoomID),
	)

	s.LeaveRoom(req.UserID, req.RoomID)
}

// handleResume 处理 Resume 请求
func (s *Server) handleResume(conn *GatewayConnection, payload []byte) {
	var req protocol.ResumeRequest
	if err := protocol.Decode(payload, &req); err != nil {
		logger.Warn("decode resume request failed", zap.Error(err))
		return
	}

	logger.Debug("resume request",
		zap.String("user_id", req.UserID),
		zap.String("room_id", req.RoomID),
		zap.String("gateway_id", req.GatewayID),
	)

	code, _ := s.Resume(req.UserID, req.RoomID, req.GatewayID)
	success := code == protocol.ErrCodeSuccess
	message := protocol.ErrCodeMessage[code]

	s.sendJoinRoomAck(conn, req.UserID, req.RoomID, success, code, message)
}

// handleDisconnect 处理断连通知
func (s *Server) handleDisconnect(conn *GatewayConnection, payload []byte) {
	var notify protocol.DisconnectNotify
	if err := protocol.Decode(payload, &notify); err != nil {
		logger.Warn("decode disconnect notify failed", zap.Error(err))
		return
	}

	logger.Debug("disconnect notify",
		zap.String("user_id", notify.UserID),
		zap.String("room_id", notify.RoomID),
	)

	s.UserDisconnect(notify.UserID, notify.RoomID)
}

// handleBatchReSync 处理批量重同步
func (s *Server) handleBatchReSync(conn *GatewayConnection, payload []byte) {
	var req protocol.BatchReSync
	if err := protocol.Decode(payload, &req); err != nil {
		logger.Warn("decode batch resync failed", zap.Error(err))
		return
	}

	logger.Debug("batch resync",
		zap.String("gateway_id", req.GatewayID),
		zap.Int("users", len(req.Users)),
	)

	for _, pair := range req.Users {
		s.JoinRoom(pair.UserID, pair.RoomID, req.GatewayID)
	}

	// 更新恢复状态
	s.recovery.OnUsersReceived(len(req.Users))
}

// handleReSyncComplete 处理重同步完成
func (s *Server) handleReSyncComplete(conn *GatewayConnection, payload []byte) {
	var req protocol.ReSyncComplete
	if err := protocol.Decode(payload, &req); err != nil {
		logger.Warn("decode resync complete failed", zap.Error(err))
		return
	}

	logger.Info("resync complete from gateway",
		zap.String("gateway_id", req.GatewayID),
		zap.Int("total_users", req.TotalUsers),
	)

	// 标记该 Gateway 完成重同步
	s.recovery.OnGatewayComplete()
}

// sendJoinRoomAck 发送 JoinRoom ACK
func (s *Server) sendJoinRoomAck(conn *GatewayConnection, userID, roomID string, success bool, code int, message string) {
	ack := &protocol.JoinRoomAck{
		UserID:  userID,
		RoomID:  roomID,
		Success: success,
		Code:    code,
		Message: message,
	}

	payload, _ := protocol.Encode(ack)
	s.sendToGateway(conn, protocol.InternalMsgJoinRoomAck, payload)
}

// sendEpochNotify 发送 Epoch 通知
func (s *Server) sendEpochNotify(conn *GatewayConnection) {
	notify := &protocol.EpochNotify{
		ServerID: s.cfg.Server.ID,
		Epoch:    s.epoch.Load(),
	}

	payload, _ := protocol.Encode(notify)
	s.sendToGateway(conn, protocol.InternalMsgEpochNotify, payload)
}

// sendToGateway 发送消息到 Gateway
func (s *Server) sendToGateway(conn *GatewayConnection, msgType uint32, payload []byte) {
	conn.mu.RLock()
	if conn.closed {
		conn.mu.RUnlock()
		return
	}
	conn.mu.RUnlock()

	envelope := &pb.Envelope{
		MsgType: msgType,
		Payload: payload,
	}

	select {
	case conn.sendCh <- envelope:
	default:
		logger.Warn("gateway send queue full, dropping message",
			zap.String("gateway_id", conn.id),
			zap.Uint32("msg_type", msgType),
		)
	}
}

// addGateway 添加 Gateway 连接
func (s *Server) addGateway(conn *GatewayConnection) {
	s.gatewaysMu.Lock()
	defer s.gatewaysMu.Unlock()

	// 关闭旧连接
	if old, ok := s.gateways[conn.id]; ok {
		old.mu.Lock()
		old.closed = true
		close(old.sendCh)
		old.mu.Unlock()
	}

	s.gateways[conn.id] = conn
}

// removeGateway 移除 Gateway 连接
func (s *Server) removeGateway(gatewayID string) {
	s.gatewaysMu.Lock()
	defer s.gatewaysMu.Unlock()

	delete(s.gateways, gatewayID)

	logger.Info("gateway disconnected",
		zap.String("gateway_id", gatewayID),
	)
}

// getGateway 获取 Gateway 连接
func (s *Server) getGateway(gatewayID string) *GatewayConnection {
	s.gatewaysMu.RLock()
	defer s.gatewaysMu.RUnlock()
	return s.gateways[gatewayID]
}
