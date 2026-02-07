package roomserver

import (
	"sync"

	"github.com/qiminjie89/imsys/internal/protocol"
	"github.com/qiminjie89/imsys/pkg/logger"
	"go.uber.org/zap"
)

// GatewayConn Gateway 连接
type GatewayConn struct {
	GatewayID string
	Addr      string

	// 发送队列
	sendCh chan *protocol.Frame

	// 关闭控制
	closeCh   chan struct{}
	closeOnce sync.Once

	// 所属服务器
	server *Server
}

// NewGatewayConn 创建 Gateway 连接
func NewGatewayConn(gatewayID, addr string, server *Server) *GatewayConn {
	return &GatewayConn{
		GatewayID: gatewayID,
		Addr:      addr,
		sendCh:    make(chan *protocol.Frame, 10000),
		closeCh:   make(chan struct{}),
		server:    server,
	}
}

// Close 关闭连接
func (c *GatewayConn) Close() {
	c.closeOnce.Do(func() {
		close(c.closeCh)
	})
}

// SendBroadcast 发送广播消息
func (c *GatewayConn) SendBroadcast(msg *protocol.BroadcastMessage) {
	payload, err := protocol.Encode(msg)
	if err != nil {
		logger.Warn("encode broadcast message failed", zap.Error(err))
		return
	}

	frame := &protocol.Frame{
		MsgType: protocol.InternalMsgBroadcast,
		Payload: payload,
	}

	c.send(frame)
}

// SendUnicast 发送单播消息
func (c *GatewayConn) SendUnicast(msg *protocol.UnicastMessage) {
	payload, err := protocol.Encode(msg)
	if err != nil {
		logger.Warn("encode unicast message failed", zap.Error(err))
		return
	}

	frame := &protocol.Frame{
		MsgType: protocol.InternalMsgUnicast,
		Payload: payload,
	}

	c.send(frame)
}

// SendMulticast 发送多播消息
func (c *GatewayConn) SendMulticast(msg *protocol.MulticastMessage) {
	payload, err := protocol.Encode(msg)
	if err != nil {
		logger.Warn("encode multicast message failed", zap.Error(err))
		return
	}

	frame := &protocol.Frame{
		MsgType: protocol.InternalMsgMulticast,
		Payload: payload,
	}

	c.send(frame)
}

// SendJoinRoomAck 发送 JoinRoom ACK
func (c *GatewayConn) SendJoinRoomAck(ack *protocol.JoinRoomAck) {
	payload, err := protocol.Encode(ack)
	if err != nil {
		logger.Warn("encode join room ack failed", zap.Error(err))
		return
	}

	frame := &protocol.Frame{
		MsgType: protocol.InternalMsgJoinRoomAck,
		Payload: payload,
	}

	c.send(frame)
}

// SendEpochNotify 发送 Epoch 通知
func (c *GatewayConn) SendEpochNotify(notify *protocol.EpochNotify) {
	payload, err := protocol.Encode(notify)
	if err != nil {
		logger.Warn("encode epoch notify failed", zap.Error(err))
		return
	}

	frame := &protocol.Frame{
		MsgType: protocol.InternalMsgEpochNotify,
		Payload: payload,
	}

	c.send(frame)
}

// SendRoomClose 发送房间关闭通知
func (c *GatewayConn) SendRoomClose(msg *protocol.RoomClose) {
	payload, err := protocol.Encode(msg)
	if err != nil {
		logger.Warn("encode room close failed", zap.Error(err))
		return
	}

	frame := &protocol.Frame{
		MsgType: protocol.InternalMsgRoomClose,
		Payload: payload,
	}

	c.send(frame)
}

// send 发送消息
func (c *GatewayConn) send(frame *protocol.Frame) {
	select {
	case c.sendCh <- frame:
	case <-c.closeCh:
	default:
		logger.Warn("gateway send queue full",
			zap.String("gateway_id", c.GatewayID),
		)
	}
}
