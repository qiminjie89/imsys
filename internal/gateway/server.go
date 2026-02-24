// Package gateway 实现 IM 系统的接入层
package gateway

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qiminjie89/imsys/pkg/auth"
	"github.com/qiminjie89/imsys/pkg/config"
	"github.com/qiminjie89/imsys/pkg/kafka"
	"github.com/qiminjie89/imsys/pkg/logger"
	"github.com/qiminjie89/imsys/pkg/nacos"
	"go.uber.org/zap"
)

// GatewayStatus Gateway 状态
type GatewayStatus int32

const (
	StatusNormal   GatewayStatus = iota // 正常
	StatusDegraded                      // 降级（Roomserver 不可用）
)

// Server Gateway 服务器
type Server struct {
	cfg *config.GatewayConfig

	// 服务状态
	status        atomic.Int32 // GatewayStatus
	degradedSince time.Time

	// 连接管理（不支持多端登录，user_id 唯一）
	connections map[string]*Connection // user_id → connection
	connsMu     sync.RWMutex

	// Distributor 分片（广播扇出）
	distributors []*Distributor

	// 本地房间成员缓存（ACK 驱动更新）
	localRooms   map[string]map[string]bool // room_id → Set<user_id>
	localRoomsMu sync.RWMutex

	// 本地用户索引（用于单播消息快速判断）
	localUsers   map[string]bool // user_id → true
	localUsersMu sync.RWMutex

	// Roomserver 连接
	roomserverClient *RoomserverClient
	knownEpochs      map[string]uint64 // roomserver_id → epoch

	// Kafka
	kafkaProducer  *KafkaProducer   // 业务请求转发
	kafkaConsumer  *kafka.Consumer  // 数据面消息消费

	// 认证
	jwtValidator *auth.JWTValidator // JWT 验证器（nil 表示开发模式）

	// 服务发现
	nacosClient *nacos.Client // Nacos 客户端

	// 降级期间待补偿的请求
	pendingLeaves      []PendingRequest
	pendingDisconnects []PendingRequest
	pendingMu          sync.Mutex

	// 用户请求串行化锁
	userLocks sync.Map // user_id → *sync.Mutex

	// 生命周期
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	httpServer *http.Server // WebSocket 服务器（用于优雅关闭）
}

// PendingRequest 待补偿的请求
type PendingRequest struct {
	UserID  string
	RoomID  string
	Type    string // "leave" | "disconnect"
	Time    time.Time
}

// NewServer 创建 Gateway 服务器
func NewServer(cfg *config.GatewayConfig) *Server {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		cfg:          cfg,
		connections:  make(map[string]*Connection),
		localRooms:   make(map[string]map[string]bool),
		localUsers:   make(map[string]bool),
		knownEpochs:  make(map[string]uint64),
		ctx:          ctx,
		cancel:       cancel,
	}

	// 初始化为正常状态
	s.status.Store(int32(StatusNormal))

	// 初始化 Distributors
	s.distributors = make([]*Distributor, cfg.Distributor.Shards)
	for i := 0; i < cfg.Distributor.Shards; i++ {
		s.distributors[i] = NewDistributor(i, cfg.Distributor.InputQueueSize, &cfg.Protection)
	}

	// 初始化 JWT 验证器（如果配置了密钥）
	if cfg.Auth.JWTSecret != "" {
		s.jwtValidator = auth.NewJWTValidator(cfg.Auth.JWTSecret)
		logger.Info("JWT authentication enabled")
	} else {
		logger.Warn("JWT authentication disabled (dev mode)")
	}

	// 初始化 Nacos 客户端（如果配置了）
	if cfg.Nacos.ServerAddr != "" {
		s.nacosClient = nacos.NewClient(&nacos.Config{
			ServerAddr: cfg.Nacos.ServerAddr,
			Namespace:  cfg.Nacos.Namespace,
		})
		logger.Info("Nacos service discovery enabled",
			zap.String("server", cfg.Nacos.ServerAddr),
		)
	}

	return s
}

// Start 启动服务
func (s *Server) Start() error {
	logger.Info("starting gateway server",
		zap.String("id", s.cfg.Gateway.ID),
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

	// 启动 Kafka 生产者（业务请求转发）
	var err error
	s.kafkaProducer, err = NewKafkaProducer(&s.cfg.Kafka)
	if err != nil {
		return err
	}

	// 启动 Kafka 消费者（数据面消息接收）
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.startKafkaConsumer(s.ctx)
	}()

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

// Stop 停止服务（优雅关闭）
func (s *Server) Stop() {
	logger.Info("stopping gateway server")

	// 1. 停止接受新连接
	if s.httpServer != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http server shutdown error", zap.Error(err))
		}
	}

	// 2. 关闭所有现有连接
	s.closeAllConnections()

	// 3. 取消上下文，停止所有 goroutine
	s.cancel()
	s.wg.Wait()

	// 4. 关闭 Kafka 连接
	if s.kafkaProducer != nil {
		s.kafkaProducer.Close()
	}
	if s.kafkaConsumer != nil {
		s.kafkaConsumer.Close()
	}

	// 5. 关闭 Nacos 客户端
	if s.nacosClient != nil {
		s.nacosClient.Close()
	}

	logger.Info("gateway server stopped")
}

// closeAllConnections 关闭所有连接
func (s *Server) closeAllConnections() {
	s.connsMu.Lock()
	conns := make([]*Connection, 0, len(s.connections))
	for _, conn := range s.connections {
		conns = append(conns, conn)
	}
	s.connsMu.Unlock()

	for _, conn := range conns {
		conn.Close("server_shutdown")
	}

	logger.Info("closed all connections", zap.Int("count", len(conns)))
}

// GetStatus 获取服务状态
func (s *Server) GetStatus() GatewayStatus {
	return GatewayStatus(s.status.Load())
}

// SetDegraded 设置为降级状态
func (s *Server) SetDegraded() {
	if s.status.CompareAndSwap(int32(StatusNormal), int32(StatusDegraded)) {
		s.degradedSince = time.Now()
		logger.Warn("gateway entered degraded mode")
	}
}

// SetNormal 恢复正常状态
func (s *Server) SetNormal() {
	if s.status.CompareAndSwap(int32(StatusDegraded), int32(StatusNormal)) {
		logger.Info("gateway recovered from degraded mode",
			zap.Duration("duration", time.Since(s.degradedSince)),
		)
		s.degradedSince = time.Time{}
		// 处理待补偿的请求
		go s.processPendingRequests()
	}
}

// processPendingRequests 处理降级期间积累的请求
func (s *Server) processPendingRequests() {
	s.pendingMu.Lock()
	leaves := s.pendingLeaves
	disconnects := s.pendingDisconnects
	s.pendingLeaves = nil
	s.pendingDisconnects = nil
	s.pendingMu.Unlock()

	for _, req := range leaves {
		s.roomserverClient.SendLeaveRoom(req.UserID, req.RoomID)
	}
	for _, req := range disconnects {
		s.roomserverClient.SendDisconnect(req.UserID, req.RoomID)
	}

	logger.Info("processed pending requests",
		zap.Int("leaves", len(leaves)),
		zap.Int("disconnects", len(disconnects)),
	)
}

// AddConnection 添加连接
func (s *Server) AddConnection(conn *Connection) {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()

	// 如果已存在旧连接，先关闭
	if old, ok := s.connections[conn.UserID]; ok {
		old.Close("replaced")
	}

	s.connections[conn.UserID] = conn

	// 分配到 Distributor
	shardIdx := hashUserID(conn.UserID) % len(s.distributors)
	s.distributors[shardIdx].AddConn(conn)
	conn.distributor = s.distributors[shardIdx]

	// 添加到本地用户索引
	s.localUsersMu.Lock()
	s.localUsers[conn.UserID] = true
	s.localUsersMu.Unlock()
}

// RemoveConnection 移除连接
func (s *Server) RemoveConnection(userID string) {
	s.connsMu.Lock()
	conn, ok := s.connections[userID]
	if ok {
		delete(s.connections, userID)
	}
	s.connsMu.Unlock()

	if ok && conn.distributor != nil {
		conn.distributor.RemoveConn(userID)
	}

	// 从本地用户索引移除
	s.localUsersMu.Lock()
	delete(s.localUsers, userID)
	s.localUsersMu.Unlock()
}

// GetConnection 获取连接
func (s *Server) GetConnection(userID string) *Connection {
	s.connsMu.RLock()
	defer s.connsMu.RUnlock()
	return s.connections[userID]
}

// AddUserToRoom 添加用户到房间（ACK 后调用）
func (s *Server) AddUserToRoom(roomID, userID string) {
	s.localRoomsMu.Lock()
	defer s.localRoomsMu.Unlock()

	if s.localRooms[roomID] == nil {
		s.localRooms[roomID] = make(map[string]bool)
	}
	s.localRooms[roomID][userID] = true
}

// RemoveUserFromRoom 从房间移除用户
func (s *Server) RemoveUserFromRoom(roomID, userID string) {
	s.localRoomsMu.Lock()
	defer s.localRoomsMu.Unlock()

	if users, ok := s.localRooms[roomID]; ok {
		delete(users, userID)
		if len(users) == 0 {
			delete(s.localRooms, roomID)
		}
	}
}

// GetRoomUsers 获取房间的本地用户列表
func (s *Server) GetRoomUsers(roomID string) []string {
	s.localRoomsMu.RLock()
	defer s.localRoomsMu.RUnlock()

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
	s.localRoomsMu.Lock()
	defer s.localRoomsMu.Unlock()

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
	s.localRoomsMu.RLock()
	defer s.localRoomsMu.RUnlock()

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

// GetUserLock 获取用户请求锁（用于请求串行化）
func (s *Server) GetUserLock(userID string) *sync.Mutex {
	lock, _ := s.userLocks.LoadOrStore(userID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// IsKafkaConnected 检查 Kafka 是否连接
func (s *Server) IsKafkaConnected() bool {
	return s.kafkaConsumer != nil && s.kafkaConsumer.IsConnected()
}

// IsRoomserverConnected 检查 Roomserver 是否连接
func (s *Server) IsRoomserverConnected() bool {
	return s.roomserverClient != nil && s.roomserverClient.IsConnected()
}

// notifyDisconnect 通知 Roomserver 用户断连
// 如果处于降级状态，暂存请求待恢复后补偿
func (s *Server) notifyDisconnect(userID, roomID string) {
	if s.GetStatus() == StatusDegraded {
		// 降级状态，暂存请求
		s.pendingMu.Lock()
		s.pendingDisconnects = append(s.pendingDisconnects, PendingRequest{
			UserID: userID,
			RoomID: roomID,
			Type:   "disconnect",
			Time:   time.Now(),
		})
		s.pendingMu.Unlock()
		return
	}

	// 正常状态，直接发送
	s.roomserverClient.SendDisconnect(userID, roomID)
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
