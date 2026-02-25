// Package roomserver 实现 IM 系统的房间服务（控制面）
package roomserver

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/qiminjie89/imsys/api/proto/gen"
	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/config"
	"github.com/qiminjie89/imsys/pkg/logger"
	"github.com/qiminjie89/imsys/pkg/metrics"
	"go.uber.org/zap"
)

// Server Roomserver 服务器
// 仅负责控制面：房间管理、会话管理
// 消息推送通过 Kafka 直达 Gateway（数据面）
type Server struct {
	pb.UnimplementedRoomServerGatewayServer // gRPC forward compatibility
	cfg                                     *config.RoomserverConfig

	// 服务状态
	status atomic.Int32  // ServerStatus
	epoch  atomic.Uint64 // 启动时间戳，用于检测重启

	// 内部服务（职责分离）
	roomSvc    *RoomService    // 房间管理（房间维度）
	sessionSvc *SessionService // 会话管理（用户维度）

	// Gateway 连接管理
	gateways   map[string]*GatewayConnection
	gatewaysMu sync.RWMutex

	// 恢复状态
	recovery *RecoveryState

	// 一致性哈希（用于内部转发）
	hashRing *HashRing

	// Roomserver 集群（用于内部转发）
	peers   map[string]*PeerConn
	peersMu sync.RWMutex

	// 生命周期
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// PeerConn Roomserver 节点连接（用于内部转发）
type PeerConn struct {
	ServerID string
	// TODO: gRPC 连接
}

// NewServer 创建 Roomserver
func NewServer(cfg *config.RoomserverConfig) *Server {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		cfg:        cfg,
		roomSvc:    NewRoomService(),
		sessionSvc: NewSessionService(),
		gateways:   make(map[string]*GatewayConnection),
		peers:      make(map[string]*PeerConn),
		ctx:        ctx,
		cancel:     cancel,
	}

	// 初始化为恢复模式
	s.status.Store(int32(protocol.ServerStatusRecovering))

	// epoch = 启动时间戳（毫秒）
	s.epoch.Store(uint64(time.Now().UnixMilli()))

	// 初始化恢复状态
	s.recovery = NewRecoveryState(cfg.Recovery.Timeout)

	// 初始化一致性哈希
	s.hashRing = NewHashRing(cfg.HashRing.VirtualNodes)

	return s
}

// Start 启动服务
func (s *Server) Start() error {
	logger.Info("starting roomserver",
		zap.String("id", s.cfg.Server.ID),
		zap.Uint64("epoch", s.epoch.Load()),
	)

	metrics.RoomserverEpoch.Set(float64(s.epoch.Load()))
	metrics.RoomserverRecoveryMode.Set(1)

	// 启动 gRPC 服务（供 Gateway 连接）
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runGRPCServer()
	}()

	// 启动 HTTP 服务（供 Python 后端调用）
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runHTTPServer()
	}()

	// 启动恢复模式监控
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.recoveryMonitor()
	}()

	// 启动断连超时检查
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.disconnectTimeoutChecker()
	}()

	logger.Info("roomserver started")
	return nil
}

// Stop 停止服务
func (s *Server) Stop() {
	logger.Info("stopping roomserver")
	s.cancel()
	s.wg.Wait()
	logger.Info("roomserver stopped")
}

// GetStatus 获取服务状态
func (s *Server) GetStatus() protocol.ServerStatus {
	return protocol.ServerStatus(s.status.Load())
}

// GetEpoch 获取当前 epoch
func (s *Server) GetEpoch() uint64 {
	return s.epoch.Load()
}

// RoomService 获取房间服务
func (s *Server) RoomService() *RoomService {
	return s.roomSvc
}

// SessionService 获取会话服务
func (s *Server) SessionService() *SessionService {
	return s.sessionSvc
}

// JoinRoom 用户加入房间
func (s *Server) JoinRoom(userID, roomID, gatewayID string) error {
	// 检查并离开旧房间
	oldRoomID := s.sessionSvc.GetUserRoom(userID)
	if oldRoomID != "" && oldRoomID != roomID {
		s.roomSvc.RemoveUser(oldRoomID, userID)
		logger.Debug("user left old room",
			zap.String("user_id", userID),
			zap.String("old_room_id", oldRoomID),
		)
	}

	// 加入新房间
	if err := s.roomSvc.AddUser(roomID, userID, gatewayID); err != nil {
		return err
	}

	// 更新会话
	s.sessionSvc.UpdateSession(userID, roomID, gatewayID)

	logger.Debug("user joined room",
		zap.String("user_id", userID),
		zap.String("room_id", roomID),
		zap.String("gateway_id", gatewayID),
	)

	return nil
}

// LeaveRoom 用户离开房间
func (s *Server) LeaveRoom(userID, roomID string) error {
	s.roomSvc.RemoveUser(roomID, userID)
	s.sessionSvc.DeleteSession(userID)

	logger.Debug("user left room",
		zap.String("user_id", userID),
		zap.String("room_id", roomID),
	)

	return nil
}

// Resume 恢复连接
func (s *Server) Resume(userID, roomID, gatewayID string) (int, string) {
	session := s.sessionSvc.GetSession(userID)

	// 用户不存在
	if session == nil {
		return protocol.ErrCodeUserNotInRoom, ""
	}

	// room_id 不匹配
	if session.RoomID != roomID {
		return protocol.ErrCodeRoomMismatch, session.RoomID
	}

	// 房间不存在
	if !s.roomSvc.RoomExists(roomID) {
		return protocol.ErrCodeRoomClosed, ""
	}

	// 更新 Gateway
	s.roomSvc.UpdateUserGateway(roomID, userID, gatewayID)
	s.sessionSvc.UpdateSession(userID, roomID, gatewayID)

	logger.Debug("user resumed",
		zap.String("user_id", userID),
		zap.String("room_id", roomID),
		zap.String("gateway_id", gatewayID),
	)

	return protocol.ErrCodeSuccess, ""
}

// UserDisconnect 用户断连
func (s *Server) UserDisconnect(userID, roomID string) {
	s.roomSvc.SetUserDisconnected(roomID, userID)
	s.sessionSvc.SetDisconnected(userID)

	logger.Debug("user disconnected",
		zap.String("user_id", userID),
		zap.String("room_id", roomID),
	)
}

// GetGateway 获取 Gateway 连接
func (s *Server) GetGatewayByID(gatewayID string) *GatewayConnection {
	s.gatewaysMu.RLock()
	defer s.gatewaysMu.RUnlock()
	return s.gateways[gatewayID]
}

// NotifyRoomClose 通知房间关闭
func (s *Server) NotifyRoomClose(roomID, reason string) {
	gatewayIDs := s.roomSvc.GetRoomGatewayIDs(roomID)

	s.gatewaysMu.RLock()
	defer s.gatewaysMu.RUnlock()

	msg := &protocol.RoomCloseNotify{
		RoomID: roomID,
		Reason: reason,
	}
	payload, _ := protocol.Encode(msg)

	for _, gwID := range gatewayIDs {
		if gw, ok := s.gateways[gwID]; ok {
			s.sendToGateway(gw, protocol.InternalMsgRoomClose, payload)
		}
	}
}

// NotifyEpochChange 通知所有 Gateway epoch 变更
func (s *Server) NotifyEpochChange() {
	s.gatewaysMu.RLock()
	defer s.gatewaysMu.RUnlock()

	notify := &protocol.EpochNotify{
		ServerID:  s.cfg.Server.ID,
		Epoch:     s.epoch.Load(),
		Timestamp: time.Now().UnixMilli(),
	}
	payload, _ := protocol.Encode(notify)

	for _, gw := range s.gateways {
		s.sendToGateway(gw, protocol.InternalMsgEpochNotify, payload)
	}
}

// recoveryMonitor 恢复模式监控
func (s *Server) recoveryMonitor() {
	// 等待恢复完成或超时
	s.recovery.Wait()

	// 退出恢复模式
	s.status.Store(int32(protocol.ServerStatusReady))
	metrics.RoomserverRecoveryMode.Set(0)

	logger.Info("exited recovery mode",
		zap.Duration("duration", s.recovery.Duration()),
		zap.Int("received_users", s.recovery.ReceivedUsers()),
		zap.Int("completed_gateways", s.recovery.CompletedGateways()),
	)
}

// disconnectTimeoutChecker 断连超时检查
func (s *Server) disconnectTimeoutChecker() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	timeout := time.Duration(s.cfg.Disconnect.Timeout) * time.Second

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.checkDisconnectTimeout(timeout)
		}
	}
}

// checkDisconnectTimeout 检查断连超时的用户
func (s *Server) checkDisconnectTimeout(timeout time.Duration) {
	userIDs := s.sessionSvc.GetAllUserIDs()

	for _, userID := range userIDs {
		if s.sessionSvc.IsUserDisconnected(userID) {
			duration := s.sessionSvc.GetDisconnectedDuration(userID)
			if duration >= timeout {
				session := s.sessionSvc.GetSession(userID)
				if session != nil {
					s.LeaveRoom(userID, session.RoomID)
					logger.Info("user timed out",
						zap.String("user_id", userID),
						zap.String("room_id", session.RoomID),
						zap.Duration("duration", duration),
					)
				}
			}
		}
	}
}
