// Package roomserver 实现 IM 系统的房间服务
package roomserver

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/config"
	"github.com/qiminjie89/imsys/pkg/logger"
	"github.com/qiminjie89/imsys/pkg/metrics"
	"go.uber.org/zap"
)

// Server Roomserver 服务器
type Server struct {
	cfg *config.RoomserverConfig

	// 服务状态
	status atomic.Int32 // ServerStatus
	epoch  atomic.Uint64

	// 房间数据
	rooms   map[string]*Room
	roomsMu sync.RWMutex

	// 用户会话
	users   map[string]*UserState
	usersMu sync.RWMutex

	// Gateway 连接管理
	gateways   map[string]*GatewayConn
	gatewaysMu sync.RWMutex

	// 恢复状态
	recovery *RecoveryState

	// 一致性哈希（用于内部转发）
	hashRing *HashRing

	// 生命周期
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// UserState 用户状态
type UserState struct {
	UserID    string
	RoomID    string
	GatewayID string
	Status    protocol.MemberStatus
}

// NewServer 创建 Roomserver
func NewServer(cfg *config.RoomserverConfig) *Server {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		cfg:      cfg,
		rooms:    make(map[string]*Room),
		users:    make(map[string]*UserState),
		gateways: make(map[string]*GatewayConn),
		ctx:      ctx,
		cancel:   cancel,
	}

	// 初始化为恢复模式
	s.status.Store(int32(protocol.ServerStatusRecovering))
	s.epoch.Add(1)

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

	logger.Info("roomserver started")
	return nil
}

// Stop 停止服务
func (s *Server) Stop() {
	logger.Info("stopping roomserver")
	s.cancel()

	// 关闭所有房间
	s.roomsMu.Lock()
	for _, room := range s.rooms {
		room.Close()
	}
	s.roomsMu.Unlock()

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

// GetOrCreateRoom 获取或创建房间
func (s *Server) GetOrCreateRoom(roomID string) *Room {
	s.roomsMu.Lock()
	defer s.roomsMu.Unlock()

	if room, ok := s.rooms[roomID]; ok {
		return room
	}

	room := NewRoom(roomID, s.cfg.Room.MsgChSize, s)
	s.rooms[roomID] = room

	// 启动房间 goroutine
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		room.Run(s.ctx)
	}()

	metrics.RoomserverRooms.Inc()
	return room
}

// GetRoom 获取房间
func (s *Server) GetRoom(roomID string) *Room {
	s.roomsMu.RLock()
	defer s.roomsMu.RUnlock()
	return s.rooms[roomID]
}

// RemoveRoom 移除房间
func (s *Server) RemoveRoom(roomID string) {
	s.roomsMu.Lock()
	room, ok := s.rooms[roomID]
	if ok {
		delete(s.rooms, roomID)
	}
	s.roomsMu.Unlock()

	if ok {
		room.Close()
		metrics.RoomserverRooms.Dec()
	}
}

// GetUser 获取用户状态
func (s *Server) GetUser(userID string) *UserState {
	s.usersMu.RLock()
	defer s.usersMu.RUnlock()
	return s.users[userID]
}

// SetUser 设置用户状态
func (s *Server) SetUser(state *UserState) {
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	s.users[state.UserID] = state
}

// RemoveUser 移除用户
func (s *Server) RemoveUser(userID string) {
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	delete(s.users, userID)
}

// GetGateway 获取 Gateway 连接
func (s *Server) GetGateway(gatewayID string) *GatewayConn {
	s.gatewaysMu.RLock()
	defer s.gatewaysMu.RUnlock()
	return s.gateways[gatewayID]
}

// AddGateway 添加 Gateway 连接
func (s *Server) AddGateway(conn *GatewayConn) {
	s.gatewaysMu.Lock()
	defer s.gatewaysMu.Unlock()
	s.gateways[conn.GatewayID] = conn
	metrics.RoomserverGatewayConnections.Inc()
}

// RemoveGateway 移除 Gateway 连接
func (s *Server) RemoveGateway(gatewayID string) {
	s.gatewaysMu.Lock()
	defer s.gatewaysMu.Unlock()
	delete(s.gateways, gatewayID)
	metrics.RoomserverGatewayConnections.Dec()
}

// BroadcastToGateways 广播消息到指定的 Gateways
func (s *Server) BroadcastToGateways(gatewayIDs []string, msg *protocol.BroadcastMessage) {
	s.gatewaysMu.RLock()
	defer s.gatewaysMu.RUnlock()

	for _, gwID := range gatewayIDs {
		if gw, ok := s.gateways[gwID]; ok {
			gw.SendBroadcast(msg)
		}
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

// NotifyEpochChange 通知所有 Gateway epoch 变更
func (s *Server) NotifyEpochChange() {
	s.gatewaysMu.RLock()
	defer s.gatewaysMu.RUnlock()

	notify := &protocol.EpochNotify{
		Epoch:     s.epoch.Load(),
		Timestamp: time.Now().UnixMilli(),
	}

	for _, gw := range s.gateways {
		gw.SendEpochNotify(notify)
	}
}
