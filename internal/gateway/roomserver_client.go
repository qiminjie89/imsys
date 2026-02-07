package gateway

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/config"
	"github.com/qiminjie89/imsys/pkg/logger"
	"go.uber.org/zap"
)

// RoomserverClient 与 Roomserver 通信的客户端
type RoomserverClient struct {
	server    *Server
	cfg       *config.RoomserverClient
	connected atomic.Bool

	// 发送队列
	sendCh chan *protocol.Frame

	mu sync.RWMutex
}

// NewRoomserverClient 创建 Roomserver 客户端
func NewRoomserverClient(server *Server, cfg *config.RoomserverClient) *RoomserverClient {
	return &RoomserverClient{
		server: server,
		cfg:    cfg,
		sendCh: make(chan *protocol.Frame, 10000),
	}
}

// Run 运行客户端
func (c *RoomserverClient) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			c.connect(ctx)
			// 重连间隔
			time.Sleep(c.cfg.ReconnectInterval)
		}
	}
}

// connect 连接到 Roomserver
func (c *RoomserverClient) connect(ctx context.Context) {
	// TODO: 从 Nacos 获取 Roomserver 地址
	// TODO: 建立 gRPC stream 连接

	logger.Info("connecting to roomserver")

	// 模拟连接成功
	c.connected.Store(true)
	defer c.connected.Store(false)

	// TODO: 启动收发循环
	// go c.sendLoop(ctx, stream)
	// c.recvLoop(ctx, stream)

	// 暂时阻塞
	<-ctx.Done()
}

// IsConnected 返回是否已连接
func (c *RoomserverClient) IsConnected() bool {
	return c.connected.Load()
}

// SendJoinRoom 发送 JoinRoom 请求
func (c *RoomserverClient) SendJoinRoom(userID, roomID string) {
	req := &protocol.JoinRoomRequest{
		UserID:    userID,
		RoomID:    roomID,
		GatewayID: c.server.cfg.Server.ID,
	}

	payload, _ := protocol.Encode(req)
	frame := &protocol.Frame{
		MsgType: protocol.InternalMsgJoinRoom,
		Payload: payload,
	}

	c.send(frame)
}

// SendLeaveRoom 发送 LeaveRoom 请求
func (c *RoomserverClient) SendLeaveRoom(userID, roomID string) {
	req := &protocol.LeaveRoomRequest{
		UserID: userID,
		RoomID: roomID,
	}

	payload, _ := protocol.Encode(req)
	frame := &protocol.Frame{
		MsgType: protocol.InternalMsgLeaveRoom,
		Payload: payload,
	}

	c.send(frame)
}

// SendResume 发送 Resume 请求
func (c *RoomserverClient) SendResume(userID, roomID string) {
	req := &protocol.ResumeRequest{
		UserID:    userID,
		RoomID:    roomID,
		GatewayID: c.server.cfg.Server.ID,
	}

	payload, _ := protocol.Encode(req)
	frame := &protocol.Frame{
		MsgType: protocol.InternalMsgResume,
		Payload: payload,
	}

	c.send(frame)
}

// SendBatchReSync 发送 BatchReSync
func (c *RoomserverClient) SendBatchReSync(users []protocol.UserRoomPair) {
	req := &protocol.BatchReSync{
		GatewayID: c.server.cfg.Server.ID,
		Users:     users,
	}

	payload, _ := protocol.Encode(req)
	frame := &protocol.Frame{
		MsgType: protocol.InternalMsgBatchReSync,
		Payload: payload,
	}

	c.send(frame)
}

// send 发送消息
func (c *RoomserverClient) send(frame *protocol.Frame) {
	select {
	case c.sendCh <- frame:
	default:
		logger.Warn("roomserver send queue full, dropping message",
			zap.Uint32("msg_type", frame.MsgType),
		)
	}
}

// handleMessage 处理从 Roomserver 收到的消息
func (c *RoomserverClient) handleMessage(frame *protocol.Frame) {
	switch frame.MsgType {
	case protocol.InternalMsgJoinRoomAck:
		c.handleJoinRoomAck(frame)
	case protocol.InternalMsgBroadcast:
		c.handleBroadcast(frame)
	case protocol.InternalMsgUnicast:
		c.handleUnicast(frame)
	case protocol.InternalMsgMulticast:
		c.handleMulticast(frame)
	case protocol.InternalMsgEpochNotify:
		c.handleEpochNotify(frame)
	case protocol.InternalMsgRoomClose:
		c.handleRoomClose(frame)
	default:
		logger.Warn("unknown internal message type",
			zap.Uint32("msg_type", frame.MsgType),
		)
	}
}

// handleJoinRoomAck 处理 JoinRoom ACK
func (c *RoomserverClient) handleJoinRoomAck(frame *protocol.Frame) {
	var ack protocol.JoinRoomAck
	if err := protocol.Decode(frame.Payload, &ack); err != nil {
		logger.Warn("decode join room ack failed", zap.Error(err))
		return
	}

	c.server.OnJoinRoomAck(ack.UserID, ack.RoomID, ack.Success, ack.Code, ack.Message)
}

// handleBroadcast 处理广播消息
func (c *RoomserverClient) handleBroadcast(frame *protocol.Frame) {
	var msg protocol.BroadcastMessage
	if err := protocol.Decode(frame.Payload, &msg); err != nil {
		logger.Warn("decode broadcast message failed", zap.Error(err))
		return
	}

	// 获取房间用户列表
	users := c.server.GetRoomUsers(msg.RoomID)
	if len(users) == 0 {
		return
	}

	// 构造分发消息
	dispatchMsg := &DispatchMessage{
		Priority: protocol.PriorityNormal, // TODO: 从消息中解析优先级
		Payload:  msg.Payload,
		RoomID:   msg.RoomID,
	}

	// 分发到各个 Distributor
	for _, d := range c.server.distributors {
		d.Enqueue(dispatchMsg)
	}
}

// handleUnicast 处理单播消息
func (c *RoomserverClient) handleUnicast(frame *protocol.Frame) {
	var msg protocol.UnicastMessage
	if err := protocol.Decode(frame.Payload, &msg); err != nil {
		logger.Warn("decode unicast message failed", zap.Error(err))
		return
	}

	conn := c.server.GetConnection(msg.UserID)
	if conn == nil {
		return
	}

	conn.Send(msg.Payload)
}

// handleMulticast 处理多播消息
func (c *RoomserverClient) handleMulticast(frame *protocol.Frame) {
	var msg protocol.MulticastMessage
	if err := protocol.Decode(frame.Payload, &msg); err != nil {
		logger.Warn("decode multicast message failed", zap.Error(err))
		return
	}

	// 分发到各个 Distributor
	dispatchMsg := &DispatchMessage{
		Priority: protocol.PriorityNormal,
		Payload:  msg.Payload,
		RoomID:   msg.RoomID,
		UserIDs:  msg.UserIDs,
	}

	for _, d := range c.server.distributors {
		d.Enqueue(dispatchMsg)
	}
}

// handleEpochNotify 处理 Epoch 变更通知
func (c *RoomserverClient) handleEpochNotify(frame *protocol.Frame) {
	var notify protocol.EpochNotify
	if err := protocol.Decode(frame.Payload, &notify); err != nil {
		logger.Warn("decode epoch notify failed", zap.Error(err))
		return
	}

	if notify.Epoch > c.server.knownEpoch {
		logger.Info("epoch changed, starting resync",
			zap.Uint64("old_epoch", c.server.knownEpoch),
			zap.Uint64("new_epoch", notify.Epoch),
		)
		c.server.knownEpoch = notify.Epoch

		// 触发渐进式 BatchReSync
		go c.startProgressiveReSync()
	}
}

// handleRoomClose 处理房间关闭通知
func (c *RoomserverClient) handleRoomClose(frame *protocol.Frame) {
	var msg protocol.RoomClose
	if err := protocol.Decode(frame.Payload, &msg); err != nil {
		logger.Warn("decode room close failed", zap.Error(err))
		return
	}

	logger.Info("room closed",
		zap.String("room_id", msg.RoomID),
		zap.String("reason", msg.Reason),
	)

	// 获取并清理房间用户
	users := c.server.ClearRoom(msg.RoomID)

	// 通知用户
	for _, userID := range users {
		conn := c.server.GetConnection(userID)
		if conn != nil {
			conn.JoinState = protocol.JoinStateInit
			conn.RoomID = ""
			// TODO: 发送 RoomClosed 通知给客户端
		}
	}
}

// startProgressiveReSync 渐进式 BatchReSync
func (c *RoomserverClient) startProgressiveReSync() {
	roomUsers := c.server.GetConfirmedUsers()

	const batchSize = 1000
	const batchInterval = 10 * time.Millisecond

	batch := make([]protocol.UserRoomPair, 0, batchSize)

	for roomID, users := range roomUsers {
		for _, userID := range users {
			batch = append(batch, protocol.UserRoomPair{
				UserID: userID,
				RoomID: roomID,
			})

			if len(batch) >= batchSize {
				c.SendBatchReSync(batch)
				batch = batch[:0]
				time.Sleep(batchInterval)
			}
		}
	}

	// 发送剩余数据
	if len(batch) > 0 {
		c.SendBatchReSync(batch)
	}

	// 发送完成标记
	c.sendReSyncComplete()
}

// sendReSyncComplete 发送 ReSync 完成标记
func (c *RoomserverClient) sendReSyncComplete() {
	roomUsers := c.server.GetConfirmedUsers()
	totalUsers := 0
	for _, users := range roomUsers {
		totalUsers += len(users)
	}

	req := &protocol.ReSyncComplete{
		GatewayID:  c.server.cfg.Server.ID,
		TotalUsers: totalUsers,
	}

	payload, _ := protocol.Encode(req)
	frame := &protocol.Frame{
		MsgType: protocol.InternalMsgReSyncComplete,
		Payload: payload,
	}

	c.send(frame)
}
