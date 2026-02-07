// Package gateway 实现 IM 系统的接入层
package gateway

import (
	"context"
	"sync"

	"github.com/qiminjie89/imsys/pkg/config"
	"github.com/qiminjie89/imsys/pkg/logger"
	"go.uber.org/zap"
)

// Server Gateway 服务器
type Server struct {
	cfg *config.GatewayConfig

	// 连接管理
	connections map[string]*Connection // user_id → connection
	connMu      sync.RWMutex

	// Distributor 分片
	distributors []*Distributor

	// 本地房间成员缓存（ACK 驱动更新）
	localRooms map[string]map[string]bool // room_id → Set<user_id>
	roomMu     sync.RWMutex

	// Roomserver 连接
	roomserverClient *RoomserverClient
	knownEpoch       uint64

	// Kafka 生产者
	kafkaProducer *KafkaProducer

	// 生命周期
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewServer 创建 Gateway 服务器
func NewServer(cfg *config.GatewayConfig) *Server {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		cfg:         cfg,
		connections: make(map[string]*Connection),
		localRooms:  make(map[string]map[string]bool),
		ctx:         ctx,
		cancel:      cancel,
	}

	// 初始化 Distributors
	s.distributors = make([]*Distributor, cfg.Distributor.Shards)
	for i := 0; i < cfg.Distributor.Shards; i++ {
		s.distributors[i] = NewDistributor(i, cfg.Distributor.InputQueueSize, &cfg.Protection)
	}

	return s
}

// Start 启动服务
func (s *Server) Start() error {
	logger.Info("starting gateway server",
		zap.String("id", s.cfg.Server.ID),
		zap.String("addr", s.cfg.Server.Addr),
	)

	// 启动 Distributors
	for _, d := range s.distributors {
		s.wg.Add(1)
		go func(dist *Distributor) {
			defer s.wg.Done()
			dist.Run(s.ctx)
		}(d)
	}

	// 启动 Kafka 生产者
	var err error
	s.kafkaProducer, err = NewKafkaProducer(&s.cfg.Kafka)
	if err != nil {
		return err
	}

	// 启动 Roomserver 客户端
	s.roomserverClient = NewRoomserverClient(s, &s.cfg.Roomserver)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.roomserverClient.Run(s.ctx)
	}()

	// 启动健康检查
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runHealthServer()
	}()

	// 启动 WebSocket 服务
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runWebSocketServer()
	}()

	logger.Info("gateway server started")
	return nil
}

// Stop 停止服务
func (s *Server) Stop() {
	logger.Info("stopping gateway server")
	s.cancel()
	s.wg.Wait()

	if s.kafkaProducer != nil {
		s.kafkaProducer.Close()
	}

	logger.Info("gateway server stopped")
}

// AddConnection 添加连接
func (s *Server) AddConnection(conn *Connection) {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	// 如果已存在旧连接，先关闭
	if old, ok := s.connections[conn.UserID]; ok {
		old.Close("replaced")
	}

	s.connections[conn.UserID] = conn

	// 分配到 Distributor
	shardIdx := hashUserID(conn.UserID) % len(s.distributors)
	s.distributors[shardIdx].AddConn(conn)
	conn.distributor = s.distributors[shardIdx]
}

// RemoveConnection 移除连接
func (s *Server) RemoveConnection(userID string) {
	s.connMu.Lock()
	conn, ok := s.connections[userID]
	if ok {
		delete(s.connections, userID)
	}
	s.connMu.Unlock()

	if ok && conn.distributor != nil {
		conn.distributor.RemoveConn(userID)
	}
}

// GetConnection 获取连接
func (s *Server) GetConnection(userID string) *Connection {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	return s.connections[userID]
}

// AddUserToRoom 添加用户到房间（ACK 后调用）
func (s *Server) AddUserToRoom(roomID, userID string) {
	s.roomMu.Lock()
	defer s.roomMu.Unlock()

	if s.localRooms[roomID] == nil {
		s.localRooms[roomID] = make(map[string]bool)
	}
	s.localRooms[roomID][userID] = true
}

// RemoveUserFromRoom 从房间移除用户
func (s *Server) RemoveUserFromRoom(roomID, userID string) {
	s.roomMu.Lock()
	defer s.roomMu.Unlock()

	if users, ok := s.localRooms[roomID]; ok {
		delete(users, userID)
		if len(users) == 0 {
			delete(s.localRooms, roomID)
		}
	}
}

// GetRoomUsers 获取房间的本地用户列表
func (s *Server) GetRoomUsers(roomID string) []string {
	s.roomMu.RLock()
	defer s.roomMu.RUnlock()

	users, ok := s.localRooms[roomID]
	if !ok {
		return nil
	}

	result := make([]string, 0, len(users))
	for userID := range users {
		result = append(result, userID)
	}
	return result
}

// ClearRoom 清空房间（RoomClose 时调用）
func (s *Server) ClearRoom(roomID string) []string {
	s.roomMu.Lock()
	defer s.roomMu.Unlock()

	users, ok := s.localRooms[roomID]
	if !ok {
		return nil
	}

	result := make([]string, 0, len(users))
	for userID := range users {
		result = append(result, userID)
	}
	delete(s.localRooms, roomID)
	return result
}

// GetConfirmedUsers 获取所有 CONFIRMED 状态的用户（用于 BatchReSync）
func (s *Server) GetConfirmedUsers() map[string][]string {
	s.roomMu.RLock()
	defer s.roomMu.RUnlock()

	result := make(map[string][]string)
	for roomID, users := range s.localRooms {
		userList := make([]string, 0, len(users))
		for userID := range users {
			userList = append(userList, userID)
		}
		result[roomID] = userList
	}
	return result
}

// hashUserID 计算用户 ID 的哈希值
func hashUserID(userID string) int {
	h := 0
	for _, c := range userID {
		h = 31*h + int(c)
	}
	if h < 0 {
		h = -h
	}
	return h
}
