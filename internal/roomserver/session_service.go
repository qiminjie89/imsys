package roomserver

import (
	"sync"
	"time"

	"github.com/qiminjie89/imsys/internal/protocol"
)

const (
	// SessionShardCount 会话分片数（减少锁竞争）
	SessionShardCount = 256
)

// SessionService 会话管理服务（用户维度）
// 职责：
//   - 用户会话管理（user → gateway 映射）
//   - 用户房间映射（user → room）
//   - 连接状态管理（断线重连）
type SessionService struct {
	shards [SessionShardCount]*sessionShard
}

// sessionShard 会话分片
type sessionShard struct {
	mu       sync.RWMutex
	sessions map[string]*Session // user_id → Session
}

// Session 用户会话
type Session struct {
	UserID         string
	RoomID         string // 当前所在房间（1:1）
	GatewayID      string
	Status         protocol.MemberStatus
	ConnectedAt    time.Time
	DisconnectedAt time.Time
}

// NewSessionService 创建会话服务
func NewSessionService() *SessionService {
	ss := &SessionService{}
	for i := 0; i < SessionShardCount; i++ {
		ss.shards[i] = &sessionShard{
			sessions: make(map[string]*Session),
		}
	}
	return ss
}

// getShard 根据 userID 获取分片
func (ss *SessionService) getShard(userID string) *sessionShard {
	h := fnvHash(userID)
	return ss.shards[h%SessionShardCount]
}

// fnvHash 简单的 FNV-1a 哈希
func fnvHash(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// GetSession 获取用户会话
func (ss *SessionService) GetSession(userID string) *Session {
	shard := ss.getShard(userID)
	shard.mu.RLock()
	defer shard.mu.RUnlock()
	return shard.sessions[userID]
}

// GetUserRoom 获取用户当前所在房间
func (ss *SessionService) GetUserRoom(userID string) string {
	session := ss.GetSession(userID)
	if session == nil {
		return ""
	}
	return session.RoomID
}

// GetUserGateway 获取用户所在 Gateway
func (ss *SessionService) GetUserGateway(userID string) string {
	session := ss.GetSession(userID)
	if session == nil {
		return ""
	}
	return session.GatewayID
}

// CreateSession 创建会话（JoinRoom 时）
func (ss *SessionService) CreateSession(userID, roomID, gatewayID string) {
	shard := ss.getShard(userID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	shard.sessions[userID] = &Session{
		UserID:      userID,
		RoomID:      roomID,
		GatewayID:   gatewayID,
		Status:      protocol.MemberStatusOnline,
		ConnectedAt: time.Now(),
	}
}

// UpdateSession 更新会话
func (ss *SessionService) UpdateSession(userID, roomID, gatewayID string) {
	shard := ss.getShard(userID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if session, ok := shard.sessions[userID]; ok {
		session.RoomID = roomID
		session.GatewayID = gatewayID
		session.Status = protocol.MemberStatusOnline
		session.DisconnectedAt = time.Time{}
	} else {
		shard.sessions[userID] = &Session{
			UserID:      userID,
			RoomID:      roomID,
			GatewayID:   gatewayID,
			Status:      protocol.MemberStatusOnline,
			ConnectedAt: time.Now(),
		}
	}
}

// SetDisconnected 标记用户断连
func (ss *SessionService) SetDisconnected(userID string) {
	shard := ss.getShard(userID)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	if session, ok := shard.sessions[userID]; ok {
		session.Status = protocol.MemberStatusDisconnected
		session.DisconnectedAt = time.Now()
	}
}

// DeleteSession 删除会话（LeaveRoom 或超时）
func (ss *SessionService) DeleteSession(userID string) {
	shard := ss.getShard(userID)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	delete(shard.sessions, userID)
}

// CheckAndLeaveOldRoom 检查并离开旧房间（JoinRoom 前调用）
// 返回旧房间 ID（如果有）
func (ss *SessionService) CheckAndLeaveOldRoom(userID string) string {
	session := ss.GetSession(userID)
	if session == nil {
		return ""
	}
	return session.RoomID
}

// IsUserOnline 检查用户是否在线
func (ss *SessionService) IsUserOnline(userID string) bool {
	session := ss.GetSession(userID)
	return session != nil && session.Status == protocol.MemberStatusOnline
}

// IsUserDisconnected 检查用户是否处于断连状态
func (ss *SessionService) IsUserDisconnected(userID string) bool {
	session := ss.GetSession(userID)
	return session != nil && session.Status == protocol.MemberStatusDisconnected
}

// GetDisconnectedDuration 获取断连持续时间
func (ss *SessionService) GetDisconnectedDuration(userID string) time.Duration {
	session := ss.GetSession(userID)
	if session == nil || session.Status != protocol.MemberStatusDisconnected {
		return 0
	}
	return time.Since(session.DisconnectedAt)
}

// GetSessionCount 获取会话总数（用于监控）
func (ss *SessionService) GetSessionCount() int {
	total := 0
	for i := 0; i < SessionShardCount; i++ {
		ss.shards[i].mu.RLock()
		total += len(ss.shards[i].sessions)
		ss.shards[i].mu.RUnlock()
	}
	return total
}

// GetAllUserIDs 获取所有用户 ID（用于 BatchReSync）
func (ss *SessionService) GetAllUserIDs() []string {
	result := make([]string, 0)
	for i := 0; i < SessionShardCount; i++ {
		ss.shards[i].mu.RLock()
		for userID := range ss.shards[i].sessions {
			result = append(result, userID)
		}
		ss.shards[i].mu.RUnlock()
	}
	return result
}
