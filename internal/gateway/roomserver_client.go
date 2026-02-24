package gateway

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	pb "github.com/qiminjie89/imsys/api/proto/gen"
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

	// gRPC 连接
	conn   *grpc.ClientConn
	client pb.RoomServerGatewayClient
	stream pb.RoomServerGateway_ChannelClient

	// 发送队列
	sendCh chan *pb.Envelope

	// 当前连接的 Roomserver 地址
	currentAddr string

	mu sync.RWMutex
}

// NewRoomserverClient 创建 Roomserver 客户端
func NewRoomserverClient(server *Server, cfg *config.RoomserverClient) *RoomserverClient {
	return &RoomserverClient{
		server: server,
		cfg:    cfg,
		sendCh: make(chan *pb.Envelope, 10000),
	}
}

// Run 运行客户端（自动重连）
func (c *RoomserverClient) Run(ctx context.Context) {
	reconnectInterval := c.cfg.ReconnectInterval
	maxInterval := c.cfg.ReconnectMaxInterval

	for {
		select {
		case <-ctx.Done():
			c.close()
			return
		default:
			if err := c.connectAndServe(ctx); err != nil {
				logger.Warn("roomserver connection error",
					zap.Error(err),
					zap.Duration("reconnect_in", reconnectInterval),
				)
				// 确保设置为降级状态
				c.connected.Store(false)
				c.server.SetDegraded()
			}

			// 指数退避重连
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectInterval):
			}

			reconnectInterval = reconnectInterval * 2
			if reconnectInterval > maxInterval {
				reconnectInterval = maxInterval
			}
		}
	}
}

// connectAndServe 连接并服务
func (c *RoomserverClient) connectAndServe(ctx context.Context) error {
	// 获取 Roomserver 地址（从 Nacos 或配置）
	addr := c.getRoomserverAddr()
	if addr == "" {
		return nil
	}

	logger.Info("connecting to roomserver", zap.String("addr", addr))

	// 建立 gRPC 连接
	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             3 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.client = pb.NewRoomServerGatewayClient(conn)
	c.currentAddr = addr
	c.mu.Unlock()

	// 建立双向流
	stream, err := c.client.Channel(ctx)
	if err != nil {
		conn.Close()
		return err
	}

	c.mu.Lock()
	c.stream = stream
	c.mu.Unlock()

	c.connected.Store(true)
	c.server.SetNormal() // 恢复正常状态

	logger.Info("connected to roomserver", zap.String("addr", addr))

	// 发送 Gateway 信息
	c.sendGatewayInfo()

	// 启动收发循环
	errCh := make(chan error, 2)

	go func() {
		errCh <- c.sendLoop(ctx, stream)
	}()

	go func() {
		errCh <- c.recvLoop(ctx, stream)
	}()

	// 等待任一方出错
	err = <-errCh

	c.connected.Store(false)
	c.server.SetDegraded() // 进入降级状态

	c.close()

	return err
}

// getRoomserverAddr 获取 Roomserver 地址
func (c *RoomserverClient) getRoomserverAddr() string {
	// 优先从 Nacos 获取
	if c.server.nacosClient != nil {
		serviceName := c.server.cfg.Nacos.RoomserverService
		if serviceName == "" {
			serviceName = "roomserver"
		}

		instance := c.server.nacosClient.GetHealthyInstance(serviceName)
		if instance != nil {
			return instance.Address()
		}

		logger.Warn("no healthy roomserver instance from nacos")
	}

	// 回退到默认地址
	return "localhost:9000"
}

// sendLoop 发送循环
func (c *RoomserverClient) sendLoop(ctx context.Context, stream pb.RoomServerGateway_ChannelClient) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case envelope, ok := <-c.sendCh:
			if !ok {
				return nil
			}
			if err := stream.Send(envelope); err != nil {
				return err
			}
		}
	}
}

// recvLoop 接收循环
func (c *RoomserverClient) recvLoop(ctx context.Context, stream pb.RoomServerGateway_ChannelClient) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			envelope, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			c.handleEnvelope(envelope)
		}
	}
}

// handleEnvelope 处理收到的消息
func (c *RoomserverClient) handleEnvelope(envelope *pb.Envelope) {
	switch envelope.MsgType {
	case protocol.InternalMsgJoinRoomAck:
		c.handleJoinRoomAck(envelope.Payload)
	case protocol.InternalMsgBroadcast:
		c.handleBroadcast(envelope.Payload)
	case protocol.InternalMsgUnicast:
		c.handleUnicast(envelope.Payload)
	case protocol.InternalMsgMulticast:
		c.handleMulticast(envelope.Payload)
	case protocol.InternalMsgEpochNotify:
		c.handleEpochNotify(envelope.Payload)
	case protocol.InternalMsgRoomClose:
		c.handleRoomClose(envelope.Payload)
	default:
		logger.Warn("unknown internal message type",
			zap.Uint32("msg_type", envelope.MsgType),
		)
	}
}

// close 关闭连接
func (c *RoomserverClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stream != nil {
		c.stream.CloseSend()
		c.stream = nil
	}
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// IsConnected 返回是否已连接
func (c *RoomserverClient) IsConnected() bool {
	return c.connected.Load()
}

// send 发送消息
func (c *RoomserverClient) send(msgType uint32, payload []byte) {
	envelope := &pb.Envelope{
		MsgType: msgType,
		Payload: payload,
	}

	select {
	case c.sendCh <- envelope:
	default:
		logger.Warn("roomserver send queue full, dropping message",
			zap.Uint32("msg_type", msgType),
		)
	}
}

// sendGatewayInfo 发送 Gateway 信息
func (c *RoomserverClient) sendGatewayInfo() {
	info := &protocol.GatewayInfo{
		GatewayID: c.server.cfg.Gateway.ID,
		Addr:      c.server.cfg.Server.Addr,
	}

	payload, _ := protocol.Encode(info)
	c.send(protocol.InternalMsgGatewayInfo, payload)
}

// SendJoinRoom 发送 JoinRoom 请求
func (c *RoomserverClient) SendJoinRoom(userID, roomID string) {
	req := &protocol.JoinRoomRequest{
		UserID:    userID,
		RoomID:    roomID,
		GatewayID: c.server.cfg.Gateway.ID,
	}

	payload, _ := protocol.Encode(req)
	c.send(protocol.InternalMsgJoinRoom, payload)
}

// SendLeaveRoom 发送 LeaveRoom 请求
func (c *RoomserverClient) SendLeaveRoom(userID, roomID string) {
	req := &protocol.LeaveRoomRequest{
		UserID: userID,
		RoomID: roomID,
	}

	payload, _ := protocol.Encode(req)
	c.send(protocol.InternalMsgLeaveRoom, payload)
}

// SendResume 发送 Resume 请求
func (c *RoomserverClient) SendResume(userID, roomID string) {
	req := &protocol.ResumeRequest{
		UserID:    userID,
		RoomID:    roomID,
		GatewayID: c.server.cfg.Gateway.ID,
	}

	payload, _ := protocol.Encode(req)
	c.send(protocol.InternalMsgResume, payload)
}

// SendDisconnect 发送用户断连通知
func (c *RoomserverClient) SendDisconnect(userID, roomID string) {
	req := &protocol.DisconnectNotify{
		UserID:    userID,
		RoomID:    roomID,
		GatewayID: c.server.cfg.Gateway.ID,
	}

	payload, _ := protocol.Encode(req)
	c.send(protocol.InternalMsgDisconnect, payload)
}

// SendBatchReSync 发送 BatchReSync
func (c *RoomserverClient) SendBatchReSync(users []protocol.UserRoomPair) {
	req := &protocol.BatchReSync{
		GatewayID: c.server.cfg.Gateway.ID,
		Users:     users,
	}

	payload, _ := protocol.Encode(req)
	c.send(protocol.InternalMsgBatchReSync, payload)
}

// handleJoinRoomAck 处理 JoinRoom ACK
func (c *RoomserverClient) handleJoinRoomAck(payload []byte) {
	var ack protocol.JoinRoomAck
	if err := protocol.Decode(payload, &ack); err != nil {
		logger.Warn("decode join room ack failed", zap.Error(err))
		return
	}

	c.server.OnJoinRoomAck(ack.UserID, ack.RoomID, ack.Success, ack.Code, ack.Message)
}

// handleBroadcast 处理广播消息（控制面，非 Kafka 数据面）
func (c *RoomserverClient) handleBroadcast(payload []byte) {
	var msg protocol.BroadcastMessage
	if err := protocol.Decode(payload, &msg); err != nil {
		logger.Warn("decode broadcast message failed", zap.Error(err))
		return
	}

	// 获取房间用户列表
	users := c.server.GetRoomUsers(msg.RoomID)
	if len(users) == 0 {
		return
	}

	// 构造推送帧
	pushFrame := &protocol.Frame{
		MsgType: protocol.MsgTypePushMessage,
		Seq:     msg.Seq,
		Payload: msg.Payload,
	}
	data := protocol.EncodeFrame(pushFrame)

	// 分发给用户
	for _, userID := range users {
		conn := c.server.GetConnection(userID)
		if conn != nil {
			conn.Send(data)
		}
	}
}

// handleUnicast 处理单播消息
func (c *RoomserverClient) handleUnicast(payload []byte) {
	var msg protocol.UnicastMessage
	if err := protocol.Decode(payload, &msg); err != nil {
		logger.Warn("decode unicast message failed", zap.Error(err))
		return
	}

	conn := c.server.GetConnection(msg.UserID)
	if conn == nil {
		return
	}

	// 构造推送帧
	pushFrame := &protocol.Frame{
		MsgType: protocol.MsgTypePushMessage,
		Payload: msg.Payload,
	}
	data := protocol.EncodeFrame(pushFrame)
	conn.Send(data)
}

// handleMulticast 处理多播消息
func (c *RoomserverClient) handleMulticast(payload []byte) {
	var msg protocol.MulticastMessage
	if err := protocol.Decode(payload, &msg); err != nil {
		logger.Warn("decode multicast message failed", zap.Error(err))
		return
	}

	// 构造推送帧
	pushFrame := &protocol.Frame{
		MsgType: protocol.MsgTypePushMessage,
		Payload: msg.Payload,
	}
	data := protocol.EncodeFrame(pushFrame)

	// 分发给指定用户
	for _, userID := range msg.UserIDs {
		conn := c.server.GetConnection(userID)
		if conn != nil {
			conn.Send(data)
		}
	}
}

// handleEpochNotify 处理 Epoch 变更通知
func (c *RoomserverClient) handleEpochNotify(payload []byte) {
	var notify protocol.EpochNotify
	if err := protocol.Decode(payload, &notify); err != nil {
		logger.Warn("decode epoch notify failed", zap.Error(err))
		return
	}

	oldEpoch := c.server.knownEpochs[notify.ServerID]
	if notify.Epoch > oldEpoch {
		logger.Info("epoch changed, starting resync",
			zap.String("server_id", notify.ServerID),
			zap.Uint64("old_epoch", oldEpoch),
			zap.Uint64("new_epoch", notify.Epoch),
		)
		c.server.knownEpochs[notify.ServerID] = notify.Epoch

		// 触发渐进式 BatchReSync
		go c.startProgressiveReSync()
	}
}

// handleRoomClose 处理房间关闭通知
func (c *RoomserverClient) handleRoomClose(payload []byte) {
	var msg protocol.RoomCloseNotify
	if err := protocol.Decode(payload, &msg); err != nil {
		logger.Warn("decode room close failed", zap.Error(err))
		return
	}

	logger.Info("room closed",
		zap.String("room_id", msg.RoomID),
		zap.String("reason", msg.Reason),
	)

	// 获取并清理房间用户
	users := c.server.ClearRoom(msg.RoomID)

	// 构造房间关闭通知
	closeNotify := struct {
		RoomID string `msgpack:"room_id"`
		Reason string `msgpack:"reason"`
	}{
		RoomID: msg.RoomID,
		Reason: msg.Reason,
	}
	notifyPayload, _ := protocol.Encode(closeNotify)

	pushFrame := &protocol.Frame{
		MsgType: protocol.MsgTypePushMessage,
		Payload: notifyPayload,
	}
	data := protocol.EncodeFrame(pushFrame)

	// 通知用户
	for _, userID := range users {
		conn := c.server.GetConnection(userID)
		if conn != nil {
			conn.JoinState = protocol.JoinStateInit
			conn.RoomID = ""
			conn.Send(data)
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
		GatewayID:  c.server.cfg.Gateway.ID,
		TotalUsers: totalUsers,
	}

	payload, _ := protocol.Encode(req)
	c.send(protocol.InternalMsgReSyncComplete, payload)
}
