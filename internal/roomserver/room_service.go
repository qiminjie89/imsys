package roomserver

import (
	"sync"
	"time"

	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/logger"
	"github.com/qiminjie89/imsys/pkg/metrics"
	"go.uber.org/zap"
)

// RoomService 房间管理服务（房间维度）
// 职责：
//   - 房间生命周期管理（CreateRoom/CloseRoom）
//   - 房间成员管理（room → users 映射）
//   - 房间与 Gateway 映射（room → gateways）
type RoomService struct {
	mu    sync.RWMutex
	rooms map[string]*Room // room_id → Room
}

// NewRoomService 创建房间服务
func NewRoomService() *RoomService {
	return &RoomService{
		rooms: make(map[string]*Room),
	}
}

// GetRoom 获取房间
func (rs *RoomService) GetRoom(roomID string) *Room {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.rooms[roomID]
}

// GetOrCreateRoom 获取或创建房间（懒创建）
func (rs *RoomService) GetOrCreateRoom(roomID string) *Room {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if room, ok := rs.rooms[roomID]; ok {
		return room
	}

	room := NewRoom(roomID)
	rs.rooms[roomID] = room

	metrics.RoomserverRooms.Inc()
	logger.Debug("room created", zap.String("room_id", roomID))

	return room
}

// CloseRoom 关闭房间（Python 调用）
func (rs *RoomService) CloseRoom(roomID string) (*Room, bool) {
	rs.mu.Lock()
	room, ok := rs.rooms[roomID]
	if ok {
		delete(rs.rooms, roomID)
	}
	rs.mu.Unlock()

	if ok {
		metrics.RoomserverRooms.Dec()
		logger.Info("room closed", zap.String("room_id", roomID))
	}

	return room, ok
}

// AddUser 添加用户到房间
func (rs *RoomService) AddUser(roomID, userID, gatewayID string) error {
	room := rs.GetOrCreateRoom(roomID)

	room.mu.Lock()
	defer room.mu.Unlock()

	member := &RoomMember{
		UserID:    userID,
		GatewayID: gatewayID,
		Status:    protocol.MemberStatusOnline,
		JoinTime:  time.Now(),
	}
	room.Members[userID] = member

	// 更新 Gateway 视图
	if room.Gateways[gatewayID] == nil {
		room.Gateways[gatewayID] = &GatewayRoomView{
			GatewayID: gatewayID,
			UserIDs:   make(map[string]bool),
		}
	}
	room.Gateways[gatewayID].UserIDs[userID] = true

	metrics.RoomserverUsers.Inc()
	return nil
}

// RemoveUser 从房间移除用户
func (rs *RoomService) RemoveUser(roomID, userID string) {
	room := rs.GetRoom(roomID)
	if room == nil {
		return
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	member, ok := room.Members[userID]
	if !ok {
		return
	}

	delete(room.Members, userID)

	// 更新 Gateway 视图
	if view := room.Gateways[member.GatewayID]; view != nil {
		delete(view.UserIDs, userID)
		if len(view.UserIDs) == 0 {
			delete(room.Gateways, member.GatewayID)
		}
	}

	metrics.RoomserverUsers.Dec()
}

// SetUserDisconnected 标记用户断连
func (rs *RoomService) SetUserDisconnected(roomID, userID string) {
	room := rs.GetRoom(roomID)
	if room == nil {
		return
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	if member, ok := room.Members[userID]; ok {
		member.Status = protocol.MemberStatusDisconnected
		member.DisconnectTime = time.Now()
	}
}

// UpdateUserGateway 更新用户的 Gateway（Resume 场景）
func (rs *RoomService) UpdateUserGateway(roomID, userID, newGatewayID string) bool {
	room := rs.GetRoom(roomID)
	if room == nil {
		return false
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	member, ok := room.Members[userID]
	if !ok {
		return false
	}

	oldGatewayID := member.GatewayID

	// Gateway 变了，更新视图
	if oldGatewayID != newGatewayID {
		// 从旧 Gateway 移除
		if view := room.Gateways[oldGatewayID]; view != nil {
			delete(view.UserIDs, userID)
			if len(view.UserIDs) == 0 {
				delete(room.Gateways, oldGatewayID)
			}
		}

		// 添加到新 Gateway
		if room.Gateways[newGatewayID] == nil {
			room.Gateways[newGatewayID] = &GatewayRoomView{
				GatewayID: newGatewayID,
				UserIDs:   make(map[string]bool),
			}
		}
		room.Gateways[newGatewayID].UserIDs[userID] = true

		member.GatewayID = newGatewayID
	}

	member.Status = protocol.MemberStatusOnline
	member.DisconnectTime = time.Time{}

	return true
}

// GetRoomGatewayIDs 获取房间涉及的所有 Gateway ID
func (rs *RoomService) GetRoomGatewayIDs(roomID string) []string {
	room := rs.GetRoom(roomID)
	if room == nil {
		return nil
	}

	room.mu.RLock()
	defer room.mu.RUnlock()

	result := make([]string, 0, len(room.Gateways))
	for gwID := range room.Gateways {
		result = append(result, gwID)
	}
	return result
}

// GetRoomMemberCount 获取房间成员数
func (rs *RoomService) GetRoomMemberCount(roomID string) int {
	room := rs.GetRoom(roomID)
	if room == nil {
		return 0
	}

	room.mu.RLock()
	defer room.mu.RUnlock()
	return len(room.Members)
}

// RoomExists 检查房间是否存在
func (rs *RoomService) RoomExists(roomID string) bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	_, ok := rs.rooms[roomID]
	return ok
}

// GetAllRoomIDs 获取所有房间 ID（用于调试/监控）
func (rs *RoomService) GetAllRoomIDs() []string {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	result := make([]string, 0, len(rs.rooms))
	for roomID := range rs.rooms {
		result = append(result, roomID)
	}
	return result
}
