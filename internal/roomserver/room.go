package roomserver

import (
	"context"
	"time"

	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/logger"
	"github.com/qiminjie89/imsys/pkg/metrics"
	"go.uber.org/zap"
)

// Room 房间（每房间一个 goroutine）
type Room struct {
	RoomID string
	seq    uint64

	// 广播请求队列
	msgCh chan *BroadcastRequest

	// 关闭信号
	done chan struct{}

	// 房间成员
	members map[string]*RoomMember // user_id → member

	// 房间内 Gateway 列表
	gateways map[string]*GatewayRoomView // gateway_id → view

	// 所属服务器
	server *Server
}

// BroadcastRequest 广播请求
type BroadcastRequest struct {
	RoomID  string
	Seq     uint64 // 由 Room goroutine 填充
	Payload []byte
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
func NewRoom(roomID string, msgChSize int, server *Server) *Room {
	return &Room{
		RoomID:   roomID,
		msgCh:    make(chan *BroadcastRequest, msgChSize),
		done:     make(chan struct{}),
		members:  make(map[string]*RoomMember),
		gateways: make(map[string]*GatewayRoomView),
		server:   server,
	}
}

// Run 运行房间 goroutine（核心：串行处理，天然有序）
func (r *Room) Run(ctx context.Context) {
	logger.Debug("room started", zap.String("room_id", r.RoomID))

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.done:
			return
		case req := <-r.msgCh:
			r.seq++
			req.Seq = r.seq
			r.broadcastToGateways(req)
		}
	}
}

// Close 关闭房间
func (r *Room) Close() {
	select {
	case <-r.done:
		// 已关闭
	default:
		close(r.done)
	}
}

// Broadcast 提交广播请求（非阻塞）
func (r *Room) Broadcast(payload []byte) bool {
	req := &BroadcastRequest{
		RoomID:  r.RoomID,
		Payload: payload,
	}

	select {
	case r.msgCh <- req:
		return true
	default:
		// 队列满
		logger.Warn("room broadcast queue full",
			zap.String("room_id", r.RoomID),
		)
		return false
	}
}

// broadcastToGateways 广播到所有相关的 Gateway
func (r *Room) broadcastToGateways(req *BroadcastRequest) {
	msg := &protocol.BroadcastMessage{
		RoomID:  r.RoomID,
		Seq:     req.Seq,
		Payload: req.Payload,
	}

	gatewayIDs := make([]string, 0, len(r.gateways))
	for gwID := range r.gateways {
		gatewayIDs = append(gatewayIDs, gwID)
	}

	r.server.BroadcastToGateways(gatewayIDs, msg)
}

// AddMember 添加成员
func (r *Room) AddMember(userID, gatewayID string) {
	member := &RoomMember{
		UserID:    userID,
		GatewayID: gatewayID,
		Status:    protocol.MemberStatusOnline,
		JoinTime:  time.Now(),
	}
	r.members[userID] = member

	// 更新 Gateway 视图
	if r.gateways[gatewayID] == nil {
		r.gateways[gatewayID] = &GatewayRoomView{
			GatewayID: gatewayID,
			UserIDs:   make(map[string]bool),
		}
	}
	r.gateways[gatewayID].UserIDs[userID] = true

	metrics.RoomserverUsers.Inc()
	metrics.RoomserverRoomMembers.Observe(float64(len(r.members)))
}

// RemoveMember 移除成员
func (r *Room) RemoveMember(userID string) {
	member, ok := r.members[userID]
	if !ok {
		return
	}

	delete(r.members, userID)

	// 更新 Gateway 视图
	if view := r.gateways[member.GatewayID]; view != nil {
		delete(view.UserIDs, userID)
		if len(view.UserIDs) == 0 {
			delete(r.gateways, member.GatewayID)
		}
	}

	metrics.RoomserverUsers.Dec()
}

// SetMemberDisconnected 标记成员断连
func (r *Room) SetMemberDisconnected(userID string) {
	if member, ok := r.members[userID]; ok {
		member.Status = protocol.MemberStatusDisconnected
		member.DisconnectTime = time.Now()
	}
}

// SetMemberOnline 标记成员在线（Resume）
func (r *Room) SetMemberOnline(userID, newGatewayID string) {
	member, ok := r.members[userID]
	if !ok {
		return
	}

	oldGatewayID := member.GatewayID

	// 如果 Gateway 变了，更新视图
	if oldGatewayID != newGatewayID {
		// 从旧 Gateway 移除
		if view := r.gateways[oldGatewayID]; view != nil {
			delete(view.UserIDs, userID)
			if len(view.UserIDs) == 0 {
				delete(r.gateways, oldGatewayID)
			}
		}

		// 添加到新 Gateway
		if r.gateways[newGatewayID] == nil {
			r.gateways[newGatewayID] = &GatewayRoomView{
				GatewayID: newGatewayID,
				UserIDs:   make(map[string]bool),
			}
		}
		r.gateways[newGatewayID].UserIDs[userID] = true

		member.GatewayID = newGatewayID
	}

	member.Status = protocol.MemberStatusOnline
	member.DisconnectTime = time.Time{}
}

// GetMember 获取成员
func (r *Room) GetMember(userID string) *RoomMember {
	return r.members[userID]
}

// MemberCount 返回成员数量
func (r *Room) MemberCount() int {
	return len(r.members)
}

// GetGatewayIDs 获取所有 Gateway ID
func (r *Room) GetGatewayIDs() []string {
	result := make([]string, 0, len(r.gateways))
	for gwID := range r.gateways {
		result = append(result, gwID)
	}
	return result
}
