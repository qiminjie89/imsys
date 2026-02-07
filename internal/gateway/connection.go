package gateway

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/config"
	"github.com/qiminjie89/imsys/pkg/logger"
	"github.com/qiminjie89/imsys/pkg/metrics"
	"go.uber.org/zap"
)

// Connection 表示一个客户端连接
type Connection struct {
	UserID string
	ConnID string
	ws     *websocket.Conn

	// 下行消息队列
	sendCh chan []byte

	// 状态
	JoinState protocol.JoinState
	RoomID    string // 当前所在房间

	// 健康度
	health ConnHealth

	// 所属 Distributor
	distributor *Distributor

	// 配置
	cfg *config.ConnectionConfig

	// 关闭控制
	closeOnce sync.Once
	closeCh   chan struct{}

	// 所属服务器
	server *Server
}

// ConnHealth 连接健康度
type ConnHealth struct {
	// 队列维度
	QueueFullSince time.Time

	// 投递维度
	DropCount        int
	ConsecutiveDrops int

	// 写入维度
	WriteTimeoutCount        int
	ConsecutiveWriteTimeouts int
	LastWriteTime            time.Time
}

// NewConnection 创建连接
func NewConnection(userID, connID string, ws *websocket.Conn, cfg *config.ConnectionConfig, server *Server) *Connection {
	return &Connection{
		UserID:    userID,
		ConnID:    connID,
		ws:        ws,
		sendCh:    make(chan []byte, cfg.SendChSize),
		JoinState: protocol.JoinStateInit,
		cfg:       cfg,
		closeCh:   make(chan struct{}),
		server:    server,
	}
}

// Start 启动连接的读写循环
func (c *Connection) Start() {
	go c.readLoop()
	go c.writeLoop()
}

// readLoop 读循环
func (c *Connection) readLoop() {
	defer func() {
		c.Close("read_error")
	}()

	for {
		select {
		case <-c.closeCh:
			return
		default:
		}

		_, data, err := c.ws.ReadMessage()
		if err != nil {
			logger.Debug("connection read error",
				zap.String("user_id", c.UserID),
				zap.Error(err),
			)
			return
		}

		c.handleMessage(data)
	}
}

// writeLoop 写循环（带批量发送）
func (c *Connection) writeLoop() {
	ticker := time.NewTicker(c.cfg.BatchInterval)
	defer ticker.Stop()

	batch := make([][]byte, 0, c.cfg.BatchMaxSize)

	for {
		select {
		case <-c.closeCh:
			c.flushBatch(batch)
			return

		case msg, ok := <-c.sendCh:
			if !ok {
				c.flushBatch(batch)
				return
			}

			batch = append(batch, msg)
			if len(batch) >= c.cfg.BatchMaxSize {
				if !c.flushBatchWithTimeout(batch) {
					return
				}
				batch = batch[:0]
			}

		case <-ticker.C:
			if len(batch) > 0 {
				if !c.flushBatchWithTimeout(batch) {
					return
				}
				batch = batch[:0]
			}
		}
	}
}

// flushBatch 批量发送（不带超时检测）
func (c *Connection) flushBatch(batch [][]byte) {
	if len(batch) == 0 {
		return
	}
	packed := packMessages(batch)
	c.ws.WriteMessage(websocket.BinaryMessage, packed)
}

// flushBatchWithTimeout 带超时的批量发送
func (c *Connection) flushBatchWithTimeout(batch [][]byte) bool {
	if len(batch) == 0 {
		return true
	}

	c.ws.SetWriteDeadline(time.Now().Add(c.server.cfg.Protection.WriteTimeout))
	packed := packMessages(batch)
	err := c.ws.WriteMessage(websocket.BinaryMessage, packed)

	if err != nil {
		if isTimeout(err) {
			c.health.WriteTimeoutCount++
			c.health.ConsecutiveWriteTimeouts++
			metrics.GatewayWriteTimeouts.Inc()

			if c.health.ConsecutiveWriteTimeouts >= c.server.cfg.Protection.MaxConsecutiveWriteTimeouts {
				c.Close("write_timeout_exceeded")
				return false
			}
		} else {
			c.Close("write_error")
			return false
		}
	} else {
		c.health.ConsecutiveWriteTimeouts = 0
		c.health.LastWriteTime = time.Now()
	}

	return true
}

// Send 发送消息到队列（非阻塞）
func (c *Connection) Send(data []byte) bool {
	select {
	case c.sendCh <- data:
		c.health.ConsecutiveDrops = 0
		c.health.QueueFullSince = time.Time{}
		return true
	default:
		c.health.DropCount++
		c.health.ConsecutiveDrops++
		if c.health.QueueFullSince.IsZero() {
			c.health.QueueFullSince = time.Now()
		}
		return false
	}
}

// QueueUsage 返回队列使用率
func (c *Connection) QueueUsage() float64 {
	return float64(len(c.sendCh)) / float64(cap(c.sendCh))
}

// Close 关闭连接
func (c *Connection) Close(reason string) {
	c.closeOnce.Do(func() {
		close(c.closeCh)
		c.ws.Close()

		metrics.GatewayConnectionCloseReason.WithLabelValues(reason).Inc()
		metrics.GatewayConnections.Dec()

		logger.Debug("connection closed",
			zap.String("user_id", c.UserID),
			zap.String("reason", reason),
		)

		// 从服务器移除
		if c.server != nil {
			c.server.RemoveConnection(c.UserID)

			// 如果在房间中，移除并通知 Roomserver
			if c.RoomID != "" && c.JoinState == protocol.JoinStateConfirmed {
				c.server.RemoveUserFromRoom(c.RoomID, c.UserID)
				// TODO: 通知 Roomserver 断连
			}
		}
	})
}

// handleMessage 处理收到的消息
func (c *Connection) handleMessage(data []byte) {
	frame, err := protocol.DecodeFrame(data)
	if err != nil {
		logger.Warn("decode frame error",
			zap.String("user_id", c.UserID),
			zap.Error(err),
		)
		return
	}

	metrics.GatewayMessagesReceived.WithLabelValues(msgTypeName(frame.MsgType)).Inc()

	// TODO: 根据消息类型分发处理
	switch frame.MsgType {
	case protocol.MsgTypeJoinRoom:
		c.server.handleJoinRoom(c, frame)
	case protocol.MsgTypeLeaveRoom:
		c.server.handleLeaveRoom(c, frame)
	case protocol.MsgTypeResume:
		c.server.handleResume(c, frame)
	case protocol.MsgTypeHeartbeat:
		c.server.handleHeartbeat(c, frame)
	case protocol.MsgTypeBizRequest:
		c.server.handleBizRequest(c, frame)
	default:
		logger.Warn("unknown message type",
			zap.String("user_id", c.UserID),
			zap.Uint32("msg_type", frame.MsgType),
		)
	}
}

// packMessages 打包多条消息
func packMessages(messages [][]byte) []byte {
	// 简单实现：计算总长度，拼接
	totalLen := 0
	for _, m := range messages {
		totalLen += len(m)
	}

	result := make([]byte, 0, totalLen)
	for _, m := range messages {
		result = append(result, m...)
	}
	return result
}

// isTimeout 判断是否超时错误
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	// 简单判断
	return err.Error() == "i/o timeout"
}

// msgTypeName 返回消息类型名称
func msgTypeName(msgType uint32) string {
	switch msgType {
	case protocol.MsgTypeAuth:
		return "auth"
	case protocol.MsgTypeJoinRoom:
		return "join_room"
	case protocol.MsgTypeLeaveRoom:
		return "leave_room"
	case protocol.MsgTypeResume:
		return "resume"
	case protocol.MsgTypeHeartbeat:
		return "heartbeat"
	case protocol.MsgTypeBizRequest:
		return "biz_request"
	default:
		return "unknown"
	}
}
