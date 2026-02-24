package roomserver

import (
	"sync"
	"time"

	"github.com/qiminjie89/imsys/internal/protocol"
)

// Room 房间数据结构
// 注意：消息 seq 由 Python 业务进程管理，消息推送通过 Kafka 直达 Gateway
type Room struct {
	RoomID string // MongoDB _id，全局唯一

	// 房间成员
	Members map[string]*RoomMember // user_id → member
	mu      sync.RWMutex           // 保护 Members 和 Gateways

	// 房间内 Gateway 列表（用于 RoomClose 通知）
	Gateways map[string]*GatewayRoomView // gateway_id → 该 Gateway 上的用户列表
}

// RoomMember 房间成员
type RoomMember struct {
	UserID         string
	GatewayID      string
	Status         protocol.MemberStatus
	JoinTime       time.Time
	DisconnectTime time.Time
}

// GatewayRoomView Gateway 在房间内的视图
type GatewayRoomView struct {
	GatewayID string
	UserIDs   map[string]bool
}

// NewRoom 创建房间
func NewRoom(roomID string) *Room {
	return &Room{
		RoomID:   roomID,
		Members:  make(map[string]*RoomMember),
		Gateways: make(map[string]*GatewayRoomView),
	}
}

// GetMember 获取成员（需要在持有锁的情况下调用，或使用 RoomService）
func (r *Room) GetMember(userID string) *RoomMember {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.Members[userID]
}

// MemberCount 返回成员数量
func (r *Room) MemberCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.Members)
}

// GetGatewayIDs 获取所有 Gateway ID
func (r *Room) GetGatewayIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]string, 0, len(r.Gateways))
	for gwID := range r.Gateways {
		result = append(result, gwID)
	}
	return result
}

// GetUserIDs 获取所有用户 ID
func (r *Room) GetUserIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]string, 0, len(r.Members))
	for userID := range r.Members {
		result = append(result, userID)
	}
	return result
}
