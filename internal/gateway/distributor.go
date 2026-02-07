package gateway

import (
	"context"
	"sync"

	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/config"
	"github.com/qiminjie89/imsys/pkg/metrics"
)

// Distributor 负责消息分发的分片
type Distributor struct {
	id         int
	inputQueue chan *DispatchMessage
	conns      map[string]*Connection
	mu         sync.RWMutex
	protection *config.ProtectionConfig
}

// DispatchMessage 分发消息
type DispatchMessage struct {
	Priority protocol.MessagePriority
	Payload  []byte
	RoomID   string   // 广播时使用
	UserIDs  []string // 多播时使用（空表示广播给房间全部用户）
}

// NewDistributor 创建 Distributor
func NewDistributor(id int, queueSize int, protection *config.ProtectionConfig) *Distributor {
	return &Distributor{
		id:         id,
		inputQueue: make(chan *DispatchMessage, queueSize),
		conns:      make(map[string]*Connection),
		protection: protection,
	}
}

// Run 运行 Distributor
func (d *Distributor) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-d.inputQueue:
			d.dispatch(msg)
		}
	}
}

// Enqueue 入队消息
func (d *Distributor) Enqueue(msg *DispatchMessage) bool {
	select {
	case d.inputQueue <- msg:
		return true
	default:
		return false
	}
}

// dispatch 分发消息
func (d *Distributor) dispatch(msg *DispatchMessage) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// 如果指定了用户列表，只发给这些用户
	if len(msg.UserIDs) > 0 {
		for _, userID := range msg.UserIDs {
			if conn, ok := d.conns[userID]; ok {
				d.sendToConn(conn, msg)
			}
		}
		return
	}

	// 否则发给所有连接
	for _, conn := range d.conns {
		d.sendToConn(conn, msg)
	}
}

// sendToConn 发送消息到连接
func (d *Distributor) sendToConn(conn *Connection, msg *DispatchMessage) {
	queueRatio := conn.QueueUsage()

	// 根据队列状态和消息优先级决定是否投递
	shouldDrop := false
	switch {
	case queueRatio >= d.protection.QueueCriticalRatio:
		shouldDrop = msg.Priority > protocol.PriorityCritical
	case queueRatio >= d.protection.QueueSevereRatio:
		shouldDrop = msg.Priority > protocol.PriorityHigh
	case queueRatio >= d.protection.QueueWarningRatio:
		shouldDrop = msg.Priority > protocol.PriorityNormal
	}

	if shouldDrop {
		conn.health.DropCount++
		conn.health.ConsecutiveDrops++
		metrics.GatewayMessagesDropped.WithLabelValues(priorityName(msg.Priority)).Inc()
		d.checkAndCloseSlowConn(conn)
		return
	}

	// 非阻塞投递
	if !conn.Send(msg.Payload) {
		metrics.GatewayMessagesDropped.WithLabelValues(priorityName(msg.Priority)).Inc()
		d.checkAndCloseSlowConn(conn)
	}
}

// checkAndCloseSlowConn 检查并关闭慢连接
func (d *Distributor) checkAndCloseSlowConn(conn *Connection) {
	h := &conn.health

	// 连续丢弃次数过多
	if h.ConsecutiveDrops >= d.protection.MaxConsecutiveDrops {
		conn.Close("consecutive_drops_exceeded")
		return
	}

	// 队列满时间过长
	if !h.QueueFullSince.IsZero() {
		// 使用简单的时间比较
		// TODO: 使用配置的超时值
	}
}

// AddConn 添加连接
func (d *Distributor) AddConn(conn *Connection) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.conns[conn.UserID] = conn
}

// RemoveConn 移除连接
func (d *Distributor) RemoveConn(userID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.conns, userID)
}

// GetConnsForRoom 获取房间内的连接（用于广播）
func (d *Distributor) GetConnsForRoom(roomID string, roomUsers map[string]bool) []*Connection {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var result []*Connection
	for userID := range roomUsers {
		if conn, ok := d.conns[userID]; ok {
			result = append(result, conn)
		}
	}
	return result
}

func priorityName(p protocol.MessagePriority) string {
	switch p {
	case protocol.PriorityCritical:
		return "critical"
	case protocol.PriorityHigh:
		return "high"
	case protocol.PriorityNormal:
		return "normal"
	case protocol.PriorityLow:
		return "low"
	default:
		return "unknown"
	}
}
